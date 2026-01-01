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

package integration

import (
	"os"
	"runtime"
	"testing"

	sandboxAPI "github.com/containerd/containerd/api/runtime/sandbox/v1"
	"github.com/containerd/containerd/v2/pkg/shutdown"

	"github.com/containerd/nerdbox/internal/shim/sandbox"
	"github.com/containerd/nerdbox/internal/vm"
	"github.com/containerd/nerdbox/internal/vm/libkrun"
)

// shortTempDir creates a short temp directory to avoid Unix socket path length limits on macOS.
// Unix socket paths are limited to ~104 characters.
func shortTempDir(t *testing.T) string {
	dir, err := os.MkdirTemp("/tmp", "nb")
	if err != nil {
		t.Fatal("Failed to create temp dir:", err)
	}
	t.Cleanup(func() {
		os.RemoveAll(dir)
	})
	return dir
}

func runWithSandbox(t *testing.T, runTest func(*testing.T, *sandbox.Service)) {
	for _, tc := range []struct {
		name string
		vmm  vm.Manager
	}{
		{
			name: "libkrun",
			vmm:  libkrun.NewManager(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, sd := shutdown.WithShutdown(t.Context())
			sb, err := sandbox.NewService(ctx, tc.vmm, sd)
			if err != nil {
				t.Fatal("Failed to create sandbox service:", err)
			}

			t.Cleanup(func() {
				sd.Shutdown()
			})

			runTest(t, sb)
		})
	}
}

func TestSandboxCreateStart(t *testing.T) {
	runWithSandbox(t, func(t *testing.T, sb *sandbox.Service) {
		ctx := t.Context()
		bundlePath := shortTempDir(t)

		// CreateSandbox
		_, err := sb.CreateSandbox(ctx, &sandboxAPI.CreateSandboxRequest{
			SandboxID:  "test-sandbox",
			BundlePath: bundlePath,
		})
		if err != nil {
			t.Fatal("Failed to create sandbox:", err)
		}

		// StartSandbox
		startResp, err := sb.StartSandbox(ctx, &sandboxAPI.StartSandboxRequest{
			SandboxID: "test-sandbox",
		})
		if err != nil {
			t.Fatal("Failed to start sandbox:", err)
		}
		if startResp.Pid == 0 {
			t.Fatal("Expected non-zero PID")
		}
		t.Logf("Sandbox started with PID: %d", startResp.Pid)
	})
}

func TestSandboxStatus(t *testing.T) {
	runWithSandbox(t, func(t *testing.T, sb *sandbox.Service) {
		ctx := t.Context()
		bundlePath := shortTempDir(t)

		// Status before create should fail
		_, err := sb.SandboxStatus(ctx, &sandboxAPI.SandboxStatusRequest{
			SandboxID: "test-sandbox",
		})
		if err == nil {
			t.Fatal("Expected error for non-existent sandbox")
		}

		// Create sandbox
		_, err = sb.CreateSandbox(ctx, &sandboxAPI.CreateSandboxRequest{
			SandboxID:  "test-sandbox",
			BundlePath: bundlePath,
		})
		if err != nil {
			t.Fatal("Failed to create sandbox:", err)
		}

		// Status after create
		statusResp, err := sb.SandboxStatus(ctx, &sandboxAPI.SandboxStatusRequest{
			SandboxID: "test-sandbox",
		})
		if err != nil {
			t.Fatal("Failed to get sandbox status:", err)
		}
		if statusResp.State != "created" {
			t.Fatalf("Expected state 'created', got '%s'", statusResp.State)
		}

		// Start sandbox
		_, err = sb.StartSandbox(ctx, &sandboxAPI.StartSandboxRequest{
			SandboxID: "test-sandbox",
		})
		if err != nil {
			t.Fatal("Failed to start sandbox:", err)
		}

		// Status after start
		statusResp, err = sb.SandboxStatus(ctx, &sandboxAPI.SandboxStatusRequest{
			SandboxID: "test-sandbox",
		})
		if err != nil {
			t.Fatal("Failed to get sandbox status:", err)
		}
		if statusResp.State != "running" {
			t.Fatalf("Expected state 'running', got '%s'", statusResp.State)
		}
	})
}

func TestSandboxPing(t *testing.T) {
	runWithSandbox(t, func(t *testing.T, sb *sandbox.Service) {
		ctx := t.Context()
		bundlePath := shortTempDir(t)

		// Ping before start should fail
		_, err := sb.PingSandbox(ctx, &sandboxAPI.PingRequest{
			SandboxID: "test-sandbox",
		})
		if err == nil {
			t.Fatal("Expected error for non-running sandbox")
		}

		// Create and start sandbox
		_, err = sb.CreateSandbox(ctx, &sandboxAPI.CreateSandboxRequest{
			SandboxID:  "test-sandbox",
			BundlePath: bundlePath,
		})
		if err != nil {
			t.Fatal("Failed to create sandbox:", err)
		}

		_, err = sb.StartSandbox(ctx, &sandboxAPI.StartSandboxRequest{
			SandboxID: "test-sandbox",
		})
		if err != nil {
			t.Fatal("Failed to start sandbox:", err)
		}

		// Ping after start should succeed
		_, err = sb.PingSandbox(ctx, &sandboxAPI.PingRequest{
			SandboxID: "test-sandbox",
		})
		if err != nil {
			t.Fatal("Failed to ping sandbox:", err)
		}
	})
}

func TestSandboxStop(t *testing.T) {
	runWithSandbox(t, func(t *testing.T, sb *sandbox.Service) {
		ctx := t.Context()
		bundlePath := shortTempDir(t)

		// Create and start sandbox
		_, err := sb.CreateSandbox(ctx, &sandboxAPI.CreateSandboxRequest{
			SandboxID:  "test-sandbox",
			BundlePath: bundlePath,
		})
		if err != nil {
			t.Fatal("Failed to create sandbox:", err)
		}

		_, err = sb.StartSandbox(ctx, &sandboxAPI.StartSandboxRequest{
			SandboxID: "test-sandbox",
		})
		if err != nil {
			t.Fatal("Failed to start sandbox:", err)
		}

		// Stop sandbox
		_, err = sb.StopSandbox(ctx, &sandboxAPI.StopSandboxRequest{
			SandboxID:   "test-sandbox",
			TimeoutSecs: 5,
		})
		if err != nil {
			t.Fatal("Failed to stop sandbox:", err)
		}

		// Status after stop
		statusResp, err := sb.SandboxStatus(ctx, &sandboxAPI.SandboxStatusRequest{
			SandboxID: "test-sandbox",
		})
		if err != nil {
			t.Fatal("Failed to get sandbox status:", err)
		}
		if statusResp.State != "stopped" {
			t.Fatalf("Expected state 'stopped', got '%s'", statusResp.State)
		}
	})
}

func TestSandboxPlatform(t *testing.T) {
	runWithSandbox(t, func(t *testing.T, sb *sandbox.Service) {
		ctx := t.Context()

		resp, err := sb.Platform(ctx, &sandboxAPI.PlatformRequest{
			SandboxID: "test-sandbox",
		})
		if err != nil {
			t.Fatal("Failed to get platform:", err)
		}
		if resp.Platform.OS != "linux" {
			t.Fatalf("Expected OS 'linux', got '%s'", resp.Platform.OS)
		}
		if resp.Platform.Architecture != runtime.GOARCH {
			t.Fatalf("Expected architecture '%s', got '%s'", runtime.GOARCH, resp.Platform.Architecture)
		}
	})
}

func TestSandboxLifecycle(t *testing.T) {
	runWithSandbox(t, func(t *testing.T, sb *sandbox.Service) {
		ctx := t.Context()
		bundlePath := shortTempDir(t)

		// 1. Create
		_, err := sb.CreateSandbox(ctx, &sandboxAPI.CreateSandboxRequest{
			SandboxID:  "lifecycle-test",
			BundlePath: bundlePath,
		})
		if err != nil {
			t.Fatal("Failed to create sandbox:", err)
		}

		// 2. Start
		_, err = sb.StartSandbox(ctx, &sandboxAPI.StartSandboxRequest{
			SandboxID: "lifecycle-test",
		})
		if err != nil {
			t.Fatal("Failed to start sandbox:", err)
		}

		// 3. Status (running)
		statusResp, err := sb.SandboxStatus(ctx, &sandboxAPI.SandboxStatusRequest{
			SandboxID: "lifecycle-test",
		})
		if err != nil {
			t.Fatal("Failed to get status:", err)
		}
		if statusResp.State != "running" {
			t.Fatalf("Expected 'running', got '%s'", statusResp.State)
		}

		// 4. Ping
		_, err = sb.PingSandbox(ctx, &sandboxAPI.PingRequest{
			SandboxID: "lifecycle-test",
		})
		if err != nil {
			t.Fatal("Failed to ping sandbox:", err)
		}

		// 5. Stop
		_, err = sb.StopSandbox(ctx, &sandboxAPI.StopSandboxRequest{
			SandboxID:   "lifecycle-test",
			TimeoutSecs: 5,
		})
		if err != nil {
			t.Fatal("Failed to stop sandbox:", err)
		}

		// 6. Status (stopped)
		statusResp, err = sb.SandboxStatus(ctx, &sandboxAPI.SandboxStatusRequest{
			SandboxID: "lifecycle-test",
		})
		if err != nil {
			t.Fatal("Failed to get status:", err)
		}
		if statusResp.State != "stopped" {
			t.Fatalf("Expected 'stopped', got '%s'", statusResp.State)
		}

		// 7. Shutdown
		_, err = sb.ShutdownSandbox(ctx, &sandboxAPI.ShutdownSandboxRequest{
			SandboxID: "lifecycle-test",
		})
		if err != nil {
			t.Fatal("Failed to shutdown sandbox:", err)
		}
	})
}
