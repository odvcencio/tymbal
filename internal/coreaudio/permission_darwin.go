//go:build darwin && (amd64 || arm64)

package coreaudio

// Microphone consent belongs to the process opening input, not to enumeration
// or the UI host. AVFoundation's completion handler is a native Block: its
// assembly entry only publishes a result, and never calls Go from an OS thread.

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
)

//go:cgo_import_dynamic tymbal_permission_objc_msgSend objc_msgSend "/usr/lib/libobjc.A.dylib"
//go:cgo_import_dynamic tymbal_permission_objc_getClass objc_getClass "/usr/lib/libobjc.A.dylib"
//go:cgo_import_dynamic tymbal_permission_sel_registerName sel_registerName "/usr/lib/libobjc.A.dylib"
//go:cgo_import_dynamic tymbal_permission_poolPush objc_autoreleasePoolPush "/usr/lib/libobjc.A.dylib"
//go:cgo_import_dynamic tymbal_permission_poolPop objc_autoreleasePoolPop "/usr/lib/libobjc.A.dylib"
//go:cgo_import_dynamic tymbal_permission_dlsym dlsym "/usr/lib/libSystem.B.dylib"
// Anchor the framework for its Objective-C classes; this function is not called.
//go:cgo_import_dynamic tymbal_permission_AVMakeRect AVMakeRectWithAspectRatioInsideRect "/System/Library/Frameworks/AVFoundation.framework/Versions/A/AVFoundation"

var (
	fnPermissionMsgSend, fnPermissionGetClass, fnPermissionSelector uintptr
	fnPermissionPoolPush, fnPermissionPoolPop, fnPermissionDlsym    uintptr
	fnPermissionAVFoundation, fnPermissionInvoke                    uintptr
)

func permissionInvoke()

// Apple Blocks ABI, with one native pointer captured after the fixed header.
// The block, descriptor, signature and completion word all live in one mmap.
// BLOCK_IS_GLOBAL makes copies retain this process-lifetime native storage.
type permissionBlock struct {
	isa        uintptr
	flags      uint32
	reserved   uint32
	invoke     uintptr
	descriptor uintptr
	result     uintptr
}

type permissionBlockDescriptor struct{ reserved, size, signature uintptr }

type permissionRequest struct {
	mem   []byte
	block *permissionBlock
	done  *uint32 // 0 pending, 1 denied, 2 granted
}

var inputPermission struct {
	sync.Mutex
	request *permissionRequest
}

const inputPermissionTimeout = 2 * time.Minute

func permissionCString(s string) []byte { return append([]byte(s), 0) }

func permissionClass(name string) uintptr {
	b := permissionCString(name)
	v, _, _ := syscall_syscall(fnPermissionGetClass, uintptr(unsafe.Pointer(&b[0])), 0, 0)
	return v
}

func permissionSelector(name string) uintptr {
	b := permissionCString(name)
	v, _, _ := syscall_syscall(fnPermissionSelector, uintptr(unsafe.Pointer(&b[0])), 0, 0)
	return v
}

func permissionSend(obj uintptr, name string, arg uintptr) uintptr {
	v, _, _ := syscall_syscall(fnPermissionMsgSend, obj, permissionSelector(name), arg)
	return v
}

func permissionString(value string) uintptr {
	b := permissionCString(value)
	return permissionSend(permissionClass("NSString"), "stringWithUTF8String:", uintptr(unsafe.Pointer(&b[0])))
}

func microphoneUsageDescriptionError() error {
	bundle := permissionSend(permissionClass("NSBundle"), "mainBundle", 0)
	description := permissionSend(bundle, "objectForInfoDictionaryKey:", permissionString("NSMicrophoneUsageDescription"))
	return validateMicrophoneUsageDescription(description)
}

func validateMicrophoneUsageDescription(description uintptr) error {
	if description == 0 || permissionSend(description, "isKindOfClass:", permissionClass("NSString"))&0xff == 0 || permissionSend(description, "length", 0) == 0 {
		return fmt.Errorf("%w: microphone usage description is missing; run the packaged .app, or add NSMicrophoneUsageDescription to this program's Info.plist", driver.ErrPermission)
	}
	return nil
}

// newPermissionRequest needs no Go pointers in the Block or its descriptor.
// Keep this one page until process exit: consent can arrive after an Open has
// timed out, and AVFoundation may still retain its completion block then.
func newPermissionRequest() (*permissionRequest, error) {
	symbol := permissionCString("_NSConcreteGlobalBlock")
	isa, _, _ := syscall_syscall(fnPermissionDlsym, ^uintptr(1), uintptr(unsafe.Pointer(&symbol[0])), 0) // RTLD_DEFAULT = -2
	if isa == 0 || fnPermissionInvoke == 0 {
		return nil, fmt.Errorf("%w: native microphone authorization block is unavailable", driver.ErrPermission)
	}
	mem, err := syscall.Mmap(-1, 0, syscall.Getpagesize(), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return nil, fmt.Errorf("coreaudio: allocate microphone authorization: %w", err)
	}
	block := (*permissionBlock)(unsafe.Pointer(&mem[0]))
	descriptor := (*permissionBlockDescriptor)(unsafe.Pointer(&mem[64]))
	done := (*uint32)(unsafe.Pointer(&mem[128]))
	signature := "v@?B"
	if runtime.GOARCH == "amd64" {
		signature = "v@?c"
	}
	copy(mem[192:], permissionCString(signature))
	*descriptor = permissionBlockDescriptor{size: unsafe.Sizeof(*block), signature: uintptr(unsafe.Pointer(&mem[192]))}
	*block = permissionBlock{
		isa: isa, flags: 1<<28 | 1<<30, invoke: fnPermissionInvoke,
		descriptor: uintptr(unsafe.Pointer(descriptor)), result: uintptr(unsafe.Pointer(done)),
	}
	return &permissionRequest{mem: mem, block: block, done: done}, nil
}

func microphoneAuthorizationError(status uintptr) error {
	switch status {
	case 3: // AVAuthorizationStatusAuthorized
		return nil
	case 2: // AVAuthorizationStatusDenied
		return fmt.Errorf("%w: microphone access denied; enable this app in System Settings > Privacy & Security > Microphone", driver.ErrPermission)
	case 1: // AVAuthorizationStatusRestricted
		return fmt.Errorf("%w: microphone access is restricted by macOS", driver.ErrPermission)
	default:
		return fmt.Errorf("%w: microphone authorization is pending; answer the macOS permission prompt, then try recording again", driver.ErrPermission)
	}
}

// ensureInputPermission is called only after input request validation, before
// opening the input AudioUnit. Playback and device discovery never request it.
func ensureInputPermission() error {
	inputPermission.Lock()
	defer inputPermission.Unlock()
	status, err := beginInputPermission()
	if err != nil || status != 0 {
		if err != nil {
			return err
		}
		return microphoneAuthorizationError(status)
	}
	deadline := time.Now().Add(inputPermissionTimeout)
	for time.Now().Before(deadline) {
		switch atomic.LoadUint32(inputPermission.request.done) {
		case 1:
			return microphoneAuthorizationError(2)
		case 2:
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return microphoneAuthorizationError(0)
}

// Every autoreleased Foundation object stays on the thread owning this pool.
// Drop the pool and unlock before waiting for the asynchronous OS decision.
func beginInputPermission() (uintptr, error) {
	if fnPermissionAVFoundation == 0 {
		return 0, fmt.Errorf("%w: AVFoundation is unavailable", driver.ErrPermission)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pool, _, _ := syscall_syscall(fnPermissionPoolPush, 0, 0, 0)
	defer syscall_syscall(fnPermissionPoolPop, pool, 0, 0)
	device := permissionClass("AVCaptureDevice")
	if device == 0 {
		return 0, fmt.Errorf("%w: AVCaptureDevice is unavailable", driver.ErrPermission)
	}
	media := permissionString("soun") // AVMediaTypeAudio
	status := permissionSend(device, "authorizationStatusForMediaType:", media)
	if status != 0 {
		return status, nil
	}
	if inputPermission.request == nil {
		// AVFoundation terminates a process that requests consent without its
		// usage key. Report an actionable error for unbundled CLI programs.
		if err := microphoneUsageDescriptionError(); err != nil {
			return 0, err
		}
		request, err := newPermissionRequest()
		if err != nil {
			return 0, err
		}
		inputPermission.request = request
		syscall_syscall6(fnPermissionMsgSend, device, permissionSelector("requestAccessForMediaType:completionHandler:"),
			media, uintptr(unsafe.Pointer(request.block)), 0, 0)
	}
	return 0, nil
}
