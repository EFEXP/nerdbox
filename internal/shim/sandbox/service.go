/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	sandboxAPI "github.com/containerd/containerd/api/runtime/sandbox/v1"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/containerd/nerdbox/internal/vm"
)

// Compile-time check that Service implements shim.TTRPCService
var _ shim.TTRPCService = (*Service)(nil)

// sandboxState represents the state of the sandbox
type sandboxState int

const (
	sandboxStateUnknown sandboxState = iota
	sandboxStateCreated
	sandboxStateRunning
	sandboxStateStopped
)

func (s sandboxState) String() string {
	switch s {
	case sandboxStateCreated:
		return "created"
	case sandboxStateRunning:
		return "running"
	case sandboxStateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// Service is the sandbox service implementation
type Service struct {
	mu sync.Mutex

	// id is the sandbox ID
	id string

	// bundlePath is the path to the sandbox bundle
	bundlePath string

	// vmm is the VM manager used to create VM instances
	vmm vm.Manager

	// vm is the VM instance for this sandbox
	vm vm.Instance

	// vmStarted tracks whether the VM is actually running.
	// This is separate from sandbox state because VM startup happens
	// in Task.Create (after mount/network setup), not in StartSandbox.
	vmStarted bool

	// state is the current state of the sandbox
	state sandboxState

	// pid is the shim process ID
	pid uint32

	// createdAt is when the sandbox was created
	createdAt time.Time

	// startedAt is when the sandbox was started
	startedAt time.Time

	// exitedAt is when the sandbox exited
	exitedAt time.Time

	// exitStatus is the exit status of the sandbox
	exitStatus uint32

	// exitCh is closed when the sandbox exits
	exitCh chan struct{}

	// context is the root context
	context context.Context

	// initiateShutdown is called to shutdown the shim
	initiateShutdown func()
}

// NewService creates a new sandbox service
func NewService(ctx context.Context, vmm vm.Manager, sd shutdown.Service) (*Service, error) {
	s := &Service{
		vmm:              vmm,
		context:          ctx,
		exitCh:           make(chan struct{}),
		initiateShutdown: sd.Shutdown,
	}
	sd.RegisterCallback(s.shutdown)
	return s, nil
}

// RegisterTTRPC registers the sandbox service with the TTRPC server
func (s *Service) RegisterTTRPC(server *ttrpc.Server) error {
	sandboxAPI.RegisterTTRPCSandboxService(server, s)
	return nil
}

// VM returns the VM instance for this sandbox
func (s *Service) VM() vm.Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vm
}

// Client returns the TTRPC client for the VM
func (s *Service) Client() *ttrpc.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm == nil {
		return nil
	}
	return s.vm.Client()
}

// IsRunning returns true if the VM is actually running.
// This is different from IsSandboxRunning which checks sandbox state.
func (s *Service) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vmStarted
}

// IsSandboxRunning returns true if the sandbox is in running state.
// Note: This does not mean the VM is running - VM startup happens in Task.Create.
func (s *Service) IsSandboxRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == sandboxStateRunning
}

// StartVM starts the VM with the given options.
// This is called by Task service when creating the first container.
// If the VM is already running, this is a no-op.
// Note: Caller (Task.Create) should check kvm.CheckKVM() before calling this.
func (s *Service) StartVM(ctx context.Context, opts ...vm.StartOpt) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.vmStarted {
		return nil // VM already running
	}

	// VM can only be started when sandbox is in running state (StartSandbox was called)
	if s.state != sandboxStateRunning {
		return errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "sandbox not in running state: %s", s.state)
	}

	if s.vm == nil {
		return errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "VM not initialized")
	}

	log.G(ctx).Info("starting sandbox VM")
	if err := s.vm.Start(ctx, opts...); err != nil {
		return errgrpc.ToGRPCf(err, "failed to start VM")
	}

	s.vmStarted = true
	s.startedAt = time.Now()

	return nil
}

func (s *Service) shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Only shutdown VM if it was actually started
	if s.vmStarted && s.vm != nil {
		if err := s.vm.Shutdown(ctx); err != nil {
			log.G(ctx).WithError(err).Error("failed to shutdown VM")
		}
		s.vmStarted = false
	}

	// Signal exit
	select {
	case <-s.exitCh:
	default:
		close(s.exitCh)
	}

	return nil
}

// CreateSandbox creates a new sandbox
func (s *Service) CreateSandbox(ctx context.Context, r *sandboxAPI.CreateSandboxRequest) (*sandboxAPI.CreateSandboxResponse, error) {
	log.G(ctx).WithFields(log.Fields{
		"sandbox_id":  r.SandboxID,
		"bundle_path": r.BundlePath,
	}).Info("creating sandbox")

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != sandboxStateUnknown {
		return nil, errgrpc.ToGRPCf(errdefs.ErrAlreadyExists, "sandbox already exists")
	}

	s.id = r.SandboxID
	s.bundlePath = r.BundlePath
	s.createdAt = time.Now()

	// Create VM state directory
	vmState := filepath.Join(r.BundlePath, "vm")
	if err := os.MkdirAll(vmState, 0700); err != nil {
		return nil, errgrpc.ToGRPCf(err, "failed to create VM state directory")
	}

	// Create VM instance (but don't start it yet)
	vmi, err := s.vmm.NewInstance(ctx, vmState)
	if err != nil {
		return nil, errgrpc.ToGRPCf(err, "failed to create VM instance")
	}
	s.vm = vmi
	s.state = sandboxStateCreated

	return &sandboxAPI.CreateSandboxResponse{}, nil
}

// StartSandbox starts the sandbox.
// This method transitions the sandbox to running state but does NOT start the VM.
// VM startup happens in Task.Create after mounts/networking are configured.
// This method is idempotent - if already running, it returns success.
func (s *Service) StartSandbox(ctx context.Context, r *sandboxAPI.StartSandboxRequest) (*sandboxAPI.StartSandboxResponse, error) {
	log.G(ctx).WithFields(log.Fields{
		"sandbox_id": r.SandboxID,
	}).Info("starting sandbox")

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate sandbox ID
	if s.id != "" && s.id != r.SandboxID {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "sandbox %q not found", r.SandboxID)
	}

	// Idempotent: if already running, return success
	if s.state == sandboxStateRunning {
		return &sandboxAPI.StartSandboxResponse{
			Pid:       s.pid,
			CreatedAt: timestamppb.New(s.createdAt),
		}, nil
	}

	if s.state != sandboxStateCreated {
		return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "sandbox not in created state")
	}

	if s.vm == nil {
		return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "VM not initialized")
	}

	// Transition to running state (but don't start VM yet).
	// VM will be started by Task.Create after mount/network setup.
	s.state = sandboxStateRunning
	s.pid = uint32(os.Getpid())

	return &sandboxAPI.StartSandboxResponse{
		Pid:       s.pid,
		CreatedAt: timestamppb.New(s.createdAt),
	}, nil
}

// Platform returns the platform of the sandbox
func (s *Service) Platform(ctx context.Context, r *sandboxAPI.PlatformRequest) (*sandboxAPI.PlatformResponse, error) {
	log.G(ctx).WithFields(log.Fields{
		"sandbox_id": r.SandboxID,
	}).Debug("platform")

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate sandbox ID
	if s.id != "" && s.id != r.SandboxID {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "sandbox %q not found", r.SandboxID)
	}

	return &sandboxAPI.PlatformResponse{
		Platform: &types.Platform{
			OS:           "linux",
			Architecture: runtime.GOARCH,
		},
	}, nil
}

// StopSandbox stops the sandbox
func (s *Service) StopSandbox(ctx context.Context, r *sandboxAPI.StopSandboxRequest) (*sandboxAPI.StopSandboxResponse, error) {
	log.G(ctx).WithFields(log.Fields{
		"sandbox_id":   r.SandboxID,
		"timeout_secs": r.TimeoutSecs,
	}).Info("stopping sandbox")

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate sandbox ID
	if s.id != "" && s.id != r.SandboxID {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "sandbox %q not found", r.SandboxID)
	}

	if s.state == sandboxStateStopped {
		return &sandboxAPI.StopSandboxResponse{}, nil
	}

	if s.state != sandboxStateRunning {
		return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "sandbox not running")
	}

	// Apply timeout if specified
	if r.TimeoutSecs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(r.TimeoutSecs)*time.Second)
		defer cancel()
	}

	// Only shutdown VM if it was actually started
	if s.vmStarted && s.vm != nil {
		if err := s.vm.Shutdown(ctx); err != nil {
			log.G(ctx).WithError(err).Warn("failed to shutdown VM gracefully")
		}
		s.vmStarted = false
	}

	s.state = sandboxStateStopped
	s.exitedAt = time.Now()

	// Signal exit
	select {
	case <-s.exitCh:
	default:
		close(s.exitCh)
	}

	return &sandboxAPI.StopSandboxResponse{}, nil
}

// WaitSandbox waits for the sandbox to exit
func (s *Service) WaitSandbox(ctx context.Context, r *sandboxAPI.WaitSandboxRequest) (*sandboxAPI.WaitSandboxResponse, error) {
	log.G(ctx).WithFields(log.Fields{
		"sandbox_id": r.SandboxID,
	}).Debug("waiting for sandbox")

	// Validate sandbox exists and ID matches before waiting
	s.mu.Lock()
	if s.state == sandboxStateUnknown {
		s.mu.Unlock()
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "sandbox not found")
	}
	if s.id != "" && s.id != r.SandboxID {
		s.mu.Unlock()
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "sandbox %q not found", r.SandboxID)
	}
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.exitCh:
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return &sandboxAPI.WaitSandboxResponse{
		ExitStatus: s.exitStatus,
		ExitedAt:   timestamppb.New(s.exitedAt),
	}, nil
}

// SandboxStatus returns the status of the sandbox
func (s *Service) SandboxStatus(ctx context.Context, r *sandboxAPI.SandboxStatusRequest) (*sandboxAPI.SandboxStatusResponse, error) {
	log.G(ctx).WithFields(log.Fields{
		"sandbox_id": r.SandboxID,
	}).Debug("sandbox status")

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate sandbox ID
	if s.id != "" && s.id != r.SandboxID {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "sandbox %q not found", r.SandboxID)
	}

	if s.state == sandboxStateUnknown {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "sandbox not found")
	}

	resp := &sandboxAPI.SandboxStatusResponse{
		SandboxID: s.id,
		Pid:       s.pid,
		State:     s.state.String(),
		CreatedAt: timestamppb.New(s.createdAt),
	}

	if !s.exitedAt.IsZero() {
		resp.ExitedAt = timestamppb.New(s.exitedAt)
	}

	return resp, nil
}

// PingSandbox checks if the sandbox is alive.
// Returns success if the sandbox is in running state.
// Note: This does not require the VM to be running - VM starts in Task.Create.
func (s *Service) PingSandbox(ctx context.Context, r *sandboxAPI.PingRequest) (*sandboxAPI.PingResponse, error) {
	log.G(ctx).WithFields(log.Fields{
		"sandbox_id": r.SandboxID,
	}).Debug("ping sandbox")

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate sandbox ID
	if s.id != "" && s.id != r.SandboxID {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "sandbox %q not found", r.SandboxID)
	}

	if s.state != sandboxStateRunning {
		return nil, errgrpc.ToGRPCf(errdefs.ErrUnavailable, "sandbox not running")
	}

	// Sandbox is alive if it's in running state.
	// VM may or may not be running yet (starts in Task.Create).
	return &sandboxAPI.PingResponse{}, nil
}

// ShutdownSandbox shuts down the sandbox and the shim
func (s *Service) ShutdownSandbox(ctx context.Context, r *sandboxAPI.ShutdownSandboxRequest) (*sandboxAPI.ShutdownSandboxResponse, error) {
	log.G(ctx).WithFields(log.Fields{
		"sandbox_id": r.SandboxID,
	}).Info("shutting down sandbox")

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate sandbox ID
	if s.id != "" && s.id != r.SandboxID {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "sandbox %q not found", r.SandboxID)
	}

	// Only shutdown VM if it was actually started
	if s.vmStarted && s.vm != nil {
		if err := s.vm.Shutdown(ctx); err != nil {
			log.G(ctx).WithError(err).Warn("failed to shutdown VM")
		}
		s.vmStarted = false
	}

	s.state = sandboxStateStopped
	s.exitedAt = time.Now()

	// Signal exit
	select {
	case <-s.exitCh:
	default:
		close(s.exitCh)
	}

	// Initiate shim shutdown
	if s.initiateShutdown != nil {
		s.initiateShutdown()
	}

	return &sandboxAPI.ShutdownSandboxResponse{}, nil
}

// SandboxMetrics returns metrics for the sandbox
func (s *Service) SandboxMetrics(ctx context.Context, r *sandboxAPI.SandboxMetricsRequest) (*sandboxAPI.SandboxMetricsResponse, error) {
	log.G(ctx).WithFields(log.Fields{
		"sandbox_id": r.SandboxID,
	}).Debug("sandbox metrics")

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate sandbox ID
	if s.id != "" && s.id != r.SandboxID {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "sandbox %q not found", r.SandboxID)
	}

	if s.state != sandboxStateRunning {
		return nil, errgrpc.ToGRPCf(errdefs.ErrUnavailable, "sandbox not running")
	}

	// TODO: Collect metrics from the VM
	// For now, return empty metrics
	return &sandboxAPI.SandboxMetricsResponse{}, nil
}
