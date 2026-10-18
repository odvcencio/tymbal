//go:build darwin && (amd64 || arm64)

package coreaudio

import (
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
)

// This calls only our native completion entry, never AVFoundation's consent
// request or an audio device. It is safe on unattended CI and a user's Mac.
func TestPermissionCompletionBlock(t *testing.T) {
	for _, granted := range []uintptr{0, 1} {
		request, err := newPermissionRequest()
		if err != nil {
			t.Fatal(err)
		}
		if request.block.isa == 0 || request.block.flags != 1<<28|1<<30 {
			t.Fatalf("invalid native Block header: %+v", request.block)
		}
		if got := atomic.LoadUint32(request.done); got != 0 {
			t.Fatalf("new request already completed: %d", got)
		}
		syscall_syscall(fnPermissionInvoke, uintptr(unsafe.Pointer(request.block)), granted, 0)
		if got := atomic.LoadUint32(request.done); got != uint32(granted+1) {
			t.Errorf("completion(%d) published %d", granted, got)
		}
		// This test block was never passed to AVFoundation; no OS callback can
		// retain it, unlike the process-lifetime request used by input Open.
		if err := syscall.Munmap(request.mem); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMicrophoneAuthorizationErrors(t *testing.T) {
	if err := microphoneAuthorizationError(3); err != nil {
		t.Fatalf("authorized: %v", err)
	}
	for status, message := range map[uintptr]string{
		0: "pending", 1: "restricted", 2: "System Settings", 9: "pending",
	} {
		err := microphoneAuthorizationError(status)
		if !errors.Is(err, driver.ErrPermission) || !strings.Contains(err.Error(), message) {
			t.Errorf("status %d: %v, want permission error containing %q", status, err, message)
		}
	}
}

func TestPermissionFrameworkAvailableWithoutRequest(t *testing.T) {
	if fnPermissionAVFoundation == 0 || permissionClass("AVCaptureDevice") == 0 {
		t.Fatal("AVFoundation authorization API is unavailable")
	}
}

func TestPermissionUsageDescriptionPreflight(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pool, _, _ := syscall_syscall(fnPermissionPoolPush, 0, 0, 0)
	defer syscall_syscall(fnPermissionPoolPop, pool, 0, 0)
	if err := validateMicrophoneUsageDescription(permissionString("Record microphone input.")); err != nil {
		t.Fatal(err)
	}
	for _, value := range []uintptr{0, permissionString(""), permissionClass("NSNumber")} {
		if err := validateMicrophoneUsageDescription(value); !errors.Is(err, driver.ErrPermission) || !strings.Contains(err.Error(), "NSMicrophoneUsageDescription") {
			t.Errorf("missing or invalid usage description: %v", err)
		}
	}
}
