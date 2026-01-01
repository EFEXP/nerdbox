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
	"net"
	"sync"
	"testing"

	sandboxAPI "github.com/containerd/containerd/api/runtime/sandbox/v1"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/ttrpc"

	"github.com/containerd/nerdbox/internal/vm"
)

type fakeManager struct {
	vm *fakeVM
}

func (m *fakeManager) NewInstance(_ context.Context, _ string) (vm.Instance, error) {
	return m.vm, nil
}

type fakeVM struct {
	mu            sync.Mutex
	startCount    int
	shutdownCount int
}

func (f *fakeVM) Start(_ context.Context, _ ...vm.StartOpt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCount++
	return nil
}

func (f *fakeVM) Client() *ttrpc.Client {
	return nil
}

func (f *fakeVM) Shutdown(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdownCount++
	return nil
}

func (f *fakeVM) AddFS(_ context.Context, _, _ string, _ ...vm.MountOpt) error {
	return nil
}

func (f *fakeVM) AddDisk(_ context.Context, _, _ string, _ ...vm.MountOpt) error {
	return nil
}

func (f *fakeVM) AddNIC(_ context.Context, _ string, _ net.HardwareAddr, _ vm.NetworkMode, _, _ uint32) error {
	return nil
}

func (f *fakeVM) StartStream(_ context.Context) (uint32, net.Conn, error) {
	return 0, nil, nil
}

func (f *fakeVM) StartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startCount
}

func newTestService(t *testing.T) (*Service, *fakeVM) {
	t.Helper()
	ctx, sd := shutdown.WithShutdown(t.Context())
	fvm := &fakeVM{}
	svc, err := NewService(ctx, &fakeManager{vm: fvm}, sd)
	if err != nil {
		t.Fatalf("failed to create sandbox service: %v", err)
	}
	t.Cleanup(func() {
		sd.Shutdown()
	})
	return svc, fvm
}

func TestStartSandboxDoesNotStartVM(t *testing.T) {
	svc, fvm := newTestService(t)
	ctx := t.Context()

	_, err := svc.CreateSandbox(ctx, &sandboxAPI.CreateSandboxRequest{
		SandboxID:  "sb1",
		BundlePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox failed: %v", err)
	}

	_, err = svc.StartSandbox(ctx, &sandboxAPI.StartSandboxRequest{
		SandboxID: "sb1",
	})
	if err != nil {
		t.Fatalf("StartSandbox failed: %v", err)
	}

	if got := fvm.StartCount(); got != 0 {
		t.Fatalf("StartSandbox should not start the VM, start count=%d", got)
	}
}

func TestStartVMRequiresStartSandbox(t *testing.T) {
	svc, fvm := newTestService(t)
	ctx := t.Context()

	_, err := svc.CreateSandbox(ctx, &sandboxAPI.CreateSandboxRequest{
		SandboxID:  "sb1",
		BundlePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox failed: %v", err)
	}

	if err := svc.StartVM(ctx); err == nil {
		t.Fatalf("StartVM should fail before StartSandbox")
	}

	_, err = svc.StartSandbox(ctx, &sandboxAPI.StartSandboxRequest{
		SandboxID: "sb1",
	})
	if err != nil {
		t.Fatalf("StartSandbox failed: %v", err)
	}

	if err := svc.StartVM(ctx); err != nil {
		t.Fatalf("StartVM failed after StartSandbox: %v", err)
	}

	if got := fvm.StartCount(); got != 1 {
		t.Fatalf("expected VM start count=1 after StartVM, got %d", got)
	}
}
