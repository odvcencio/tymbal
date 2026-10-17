//go:build darwin && (amd64 || arm64)

package coreaudio

// This file is the whole of the "FFI layer": the load-time imports, the
// runtime gateways that call them, and thin typed wrappers. It follows
// crypto/x509/internal/macos (see coreaudio_darwin.go) with one change that
// outside code needs. The standard library reaches the entry points through
// abi.FuncPCABI0, which only the standard library may import; every other
// package, including golang.org/x/sys/unix, publishes the trampoline address
// from assembly instead: a DATA symbol (the fn* variables below) that holds
// the address of a bare "JMP imported_symbol" stub (bind_darwin.s).

import (
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

// Load-time imports. The Go linker records each library once and binds every
// symbol when the process starts, so a missing symbol fails the whole binary
// at launch rather than at first use, so each name here is long-standing public
// API. The first field is the symbol's link-time name, kept unique to this
// package; the second is the C name; the third is the install name.
//
// Only this four-field form is accepted outside the standard library and cgo
// output (cmd/compile/internal/noder). //go:cgo_ldflag is not, so there is no
// -framework flag: external linking is not supported, which CGO_ENABLED=0
// never uses.

// CoreAudio
//go:cgo_import_dynamic tymbal_AudioObjectGetPropertyData AudioObjectGetPropertyData "/System/Library/Frameworks/CoreAudio.framework/Versions/A/CoreAudio"
//go:cgo_import_dynamic tymbal_AudioObjectGetPropertyDataSize AudioObjectGetPropertyDataSize "/System/Library/Frameworks/CoreAudio.framework/Versions/A/CoreAudio"
//go:cgo_import_dynamic tymbal_AudioObjectSetPropertyData AudioObjectSetPropertyData "/System/Library/Frameworks/CoreAudio.framework/Versions/A/CoreAudio"

// AudioToolbox
//go:cgo_import_dynamic tymbal_AudioComponentFindNext AudioComponentFindNext "/System/Library/Frameworks/AudioToolbox.framework/Versions/A/AudioToolbox"
//go:cgo_import_dynamic tymbal_AudioComponentInstanceNew AudioComponentInstanceNew "/System/Library/Frameworks/AudioToolbox.framework/Versions/A/AudioToolbox"
//go:cgo_import_dynamic tymbal_AudioComponentInstanceDispose AudioComponentInstanceDispose "/System/Library/Frameworks/AudioToolbox.framework/Versions/A/AudioToolbox"
//go:cgo_import_dynamic tymbal_AudioUnitSetProperty AudioUnitSetProperty "/System/Library/Frameworks/AudioToolbox.framework/Versions/A/AudioToolbox"
//go:cgo_import_dynamic tymbal_AudioUnitInitialize AudioUnitInitialize "/System/Library/Frameworks/AudioToolbox.framework/Versions/A/AudioToolbox"
//go:cgo_import_dynamic tymbal_AudioUnitUninitialize AudioUnitUninitialize "/System/Library/Frameworks/AudioToolbox.framework/Versions/A/AudioToolbox"
//go:cgo_import_dynamic tymbal_AudioOutputUnitStart AudioOutputUnitStart "/System/Library/Frameworks/AudioToolbox.framework/Versions/A/AudioToolbox"
//go:cgo_import_dynamic tymbal_AudioOutputUnitStop AudioOutputUnitStop "/System/Library/Frameworks/AudioToolbox.framework/Versions/A/AudioToolbox"

// CoreFoundation
//go:cgo_import_dynamic tymbal_CFRelease CFRelease "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic tymbal_CFStringGetLength CFStringGetLength "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic tymbal_CFStringGetMaximumSizeForEncoding CFStringGetMaximumSizeForEncoding "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic tymbal_CFStringGetCString CFStringGetCString "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"

// libSystem
//go:cgo_import_dynamic tymbal_mach_task_self mach_task_self "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic tymbal_semaphore_create semaphore_create "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic tymbal_semaphore_destroy semaphore_destroy "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic tymbal_semaphore_signal semaphore_signal "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic tymbal_semaphore_timedwait semaphore_timedwait "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic tymbal_pthread_sigmask pthread_sigmask "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic tymbal_memcpy memcpy "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic tymbal_memset memset "/usr/lib/libSystem.B.dylib"

// fn* hold the address of each import's trampoline. bind_darwin.s defines
// them with DATA directives; Go never assigns them.
var (
	fnAudioObjectGetPropertyData        uintptr
	fnAudioObjectGetPropertyDataSize    uintptr
	fnAudioObjectSetPropertyData        uintptr
	fnAudioComponentFindNext            uintptr
	fnAudioComponentInstanceNew         uintptr
	fnAudioComponentInstanceDispose     uintptr
	fnAudioUnitSetProperty              uintptr
	fnAudioUnitInitialize               uintptr
	fnAudioUnitUninitialize             uintptr
	fnAudioOutputUnitStart              uintptr
	fnAudioOutputUnitStop               uintptr
	fnCFRelease                         uintptr
	fnCFStringGetLength                 uintptr
	fnCFStringGetMaximumSizeForEncoding uintptr
	fnCFStringGetCString                uintptr
	fnmach_task_self                    uintptr
	fnsemaphore_create                  uintptr
	fnsemaphore_destroy                 uintptr
	fnsemaphore_signal                  uintptr
	fnsemaphore_timedwait               uintptr
	fnpthread_sigmask                   uintptr
	fnmemcpy                            uintptr
	fnmemset                            uintptr
)

// Runtime gateways. These are the pull side of the linknames that the Go
// runtime and package syscall keep alive for golang.org/x/sys/unix ("Do not
// remove or change the type signature", go.dev/issue/67401). Each one switches
// to the thread's system stack, calls fn with the C ABI, and brackets the call
// with entersyscall/exitsyscall, so a blocking C call (semaphore_timedwait,
// AudioOutputUnitStart) does not stall the scheduler. They do not need
// runtime.cgocall or iscgo, which exist only for C calling back into Go.
//
// The declarations have no body, so the compiler keeps any pointer passed as
// uintptr(unsafe.Pointer(p)) in the argument list alive for the whole call.
// Go memory may therefore be passed to a call that does not retain it. Memory
// that C keeps after the call returns lives in a ring.Arena instead.

//go:noescape
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:noescape
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:linkname syscall_syscall syscall.syscall
//go:linkname syscall_syscall6 syscall.syscall6

// OSStatus and kern_return_t values this package interprets.
const (
	kernAborted  = 14
	kernTimedOut = 49

	statusNotRunning       = 's'<<24 | 't'<<16 | 'o'<<8 | 'p' // kAudioHardwareNotRunningError
	statusUnknownProperty  = 'w'<<24 | 'h'<<16 | 'o'<<8 | '?' // kAudioHardwareUnknownPropertyError
	statusBadObject        = '!'<<24 | 'o'<<16 | 'b'<<8 | 'j' // kAudioHardwareBadObjectError
	statusBadDevice        = '!'<<24 | 'd'<<16 | 'e'<<8 | 'v' // kAudioHardwareBadDeviceError
	statusUnsupportedData  = '!'<<24 | 'd'<<16 | 'a'<<8 | 't' // kAudioDeviceUnsupportedFormatError
	statusHogged           = '!'<<24 | 'h'<<16 | 'o'<<8 | 'g' // kAudioDevicePermissionsError
	statusFormatNotSupport = -10868                           // kAudioUnitErr_FormatNotSupported
	statusInvalidPropValue = -10851                           // kAudioUnitErr_InvalidPropertyValue
)

// osError is a failed Core Audio, Audio Toolbox or Mach call.
type osError struct {
	call   string
	status int32
}

func (e *osError) Error() string {
	return fmt.Sprintf("coreaudio: %s failed: %s", e.call, describeStatus(e.status))
}

// describeStatus prints an OSStatus the way Apple's headers define it: as a
// four-character code when all four bytes are printable, else as a number.
func describeStatus(status int32) string {
	u := uint32(status)
	b := [4]byte{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return fmt.Sprintf("OSStatus %d", status)
		}
	}
	return fmt.Sprintf("OSStatus '%s' (%d)", b[:], status)
}

// check turns a nonzero status into an *osError.
func check(call string, status int32) error {
	if status == 0 {
		return nil
	}
	return &osError{call: call, status: status}
}

// statusOf returns the OSStatus inside err, if any.
func statusOf(err error) (int32, bool) {
	var oe *osError
	if errors.As(err, &oe) {
		return oe.status, true
	}
	return 0, false
}

// propertyAddress is AudioObjectPropertyAddress.
type propertyAddress struct{ selector, scope, element uint32 }

// OSStatus AudioObjectGetPropertyData(AudioObjectID, const AudioObjectPropertyAddress *,
// UInt32 qualifierSize, const void *qualifier, UInt32 *ioDataSize, void *outData).
func audioObjectGetPropertyData(obj, selector, scope uint32, size *uint32, data unsafe.Pointer) int32 {
	addr := propertyAddress{selector: selector, scope: scope}
	r, _, _ := syscall_syscall6(fnAudioObjectGetPropertyData, uintptr(obj), uintptr(unsafe.Pointer(&addr)), 0, 0,
		uintptr(unsafe.Pointer(size)), uintptr(data))
	return int32(r)
}

// OSStatus AudioObjectGetPropertyDataSize(AudioObjectID, const AudioObjectPropertyAddress *,
// UInt32 qualifierSize, const void *qualifier, UInt32 *outDataSize).
func audioObjectGetPropertyDataSize(obj, selector, scope uint32, size *uint32) int32 {
	addr := propertyAddress{selector: selector, scope: scope}
	r, _, _ := syscall_syscall6(fnAudioObjectGetPropertyDataSize, uintptr(obj), uintptr(unsafe.Pointer(&addr)), 0, 0,
		uintptr(unsafe.Pointer(size)), 0)
	return int32(r)
}

// OSStatus AudioObjectSetPropertyData(AudioObjectID, const AudioObjectPropertyAddress *,
// UInt32 qualifierSize, const void *qualifier, UInt32 dataSize, const void *data).
func audioObjectSetPropertyData(obj, selector, scope, size uint32, data unsafe.Pointer) int32 {
	addr := propertyAddress{selector: selector, scope: scope}
	r, _, _ := syscall_syscall6(fnAudioObjectSetPropertyData, uintptr(obj), uintptr(unsafe.Pointer(&addr)), 0, 0,
		uintptr(size), uintptr(data))
	return int32(r)
}

// componentDescription is AudioComponentDescription.
type componentDescription struct {
	componentType, componentSubType, componentManufacturer uint32
	componentFlags, componentFlagsMask                     uint32
}

// AudioComponent AudioComponentFindNext(AudioComponent after, const AudioComponentDescription *).
func audioComponentFindNext(desc *componentDescription) uintptr {
	r, _, _ := syscall_syscall(fnAudioComponentFindNext, 0, uintptr(unsafe.Pointer(desc)), 0)
	return r
}

// OSStatus AudioComponentInstanceNew(AudioComponent, AudioComponentInstance *).
func audioComponentInstanceNew(component uintptr, instance *uintptr) int32 {
	r, _, _ := syscall_syscall(fnAudioComponentInstanceNew, component, uintptr(unsafe.Pointer(instance)), 0)
	return int32(r)
}

// OSStatus AudioComponentInstanceDispose(AudioComponentInstance).
func audioComponentInstanceDispose(unit uintptr) int32 {
	r, _, _ := syscall_syscall(fnAudioComponentInstanceDispose, unit, 0, 0)
	return int32(r)
}

// OSStatus AudioUnitSetProperty(AudioUnit, AudioUnitPropertyID, AudioUnitScope,
// AudioUnitElement, const void *data, UInt32 dataSize).
func audioUnitSetProperty(unit uintptr, property, scope, element uint32, data unsafe.Pointer, size uint32) int32 {
	r, _, _ := syscall_syscall6(fnAudioUnitSetProperty, unit, uintptr(property), uintptr(scope), uintptr(element),
		uintptr(data), uintptr(size))
	return int32(r)
}

// OSStatus AudioUnitInitialize(AudioUnit).
func audioUnitInitialize(unit uintptr) int32 {
	r, _, _ := syscall_syscall(fnAudioUnitInitialize, unit, 0, 0)
	return int32(r)
}

// OSStatus AudioUnitUninitialize(AudioUnit).
func audioUnitUninitialize(unit uintptr) int32 {
	r, _, _ := syscall_syscall(fnAudioUnitUninitialize, unit, 0, 0)
	return int32(r)
}

// OSStatus AudioOutputUnitStart(AudioUnit).
func audioOutputUnitStart(unit uintptr) int32 {
	r, _, _ := syscall_syscall(fnAudioOutputUnitStart, unit, 0, 0)
	return int32(r)
}

// OSStatus AudioOutputUnitStop(AudioUnit). It returns after the last render
// callback has finished.
func audioOutputUnitStop(unit uintptr) int32 {
	r, _, _ := syscall_syscall(fnAudioOutputUnitStop, unit, 0, 0)
	return int32(r)
}

const kCFStringEncodingUTF8 = 0x08000100

// cfRelease is void CFRelease(CFTypeRef).
func cfRelease(ref uintptr) {
	syscall_syscall(fnCFRelease, ref, 0, 0)
}

// cfStringToString copies a CFStringRef into a Go string. It does not release ref.
func cfStringToString(ref uintptr) (string, bool) {
	if ref == 0 {
		return "", false
	}
	length, _, _ := syscall_syscall(fnCFStringGetLength, ref, 0, 0)
	// CFIndex CFStringGetMaximumSizeForEncoding(CFIndex length, CFStringEncoding): -1 when unrepresentable.
	max, _, _ := syscall_syscall(fnCFStringGetMaximumSizeForEncoding, length, kCFStringEncodingUTF8, 0)
	if int(max) < 0 || int(max) > 1<<20 {
		return "", false
	}
	buf := make([]byte, int(max)+1)
	// Boolean CFStringGetCString(CFStringRef, char *buffer, CFIndex bufferSize, CFStringEncoding).
	ok, _, _ := syscall_syscall6(fnCFStringGetCString, ref, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		kCFStringEncodingUTF8, 0, 0)
	if ok&0xff == 0 {
		return "", false
	}
	for i, c := range buf {
		if c == 0 {
			return string(buf[:i]), true
		}
	}
	return "", false
}

// machTaskSelf is mach_port_t mach_task_self(void).
func machTaskSelf() uint32 {
	r, _, _ := syscall_syscall(fnmach_task_self, 0, 0, 0)
	return uint32(r)
}

// semaphoreCreate is semaphore_create(task, &sem, SYNC_POLICY_FIFO, 0).
func semaphoreCreate(task uint32, sem *uint32) int32 {
	r, _, _ := syscall_syscall6(fnsemaphore_create, uintptr(task), uintptr(unsafe.Pointer(sem)), 0, 0, 0, 0)
	return int32(r)
}

// semaphoreDestroy is semaphore_destroy(task, sem).
func semaphoreDestroy(task, sem uint32) int32 {
	r, _, _ := syscall_syscall(fnsemaphore_destroy, uintptr(task), uintptr(sem), 0)
	return int32(r)
}

// semaphoreSignal is semaphore_signal(sem).
func semaphoreSignal(sem uint32) int32 {
	r, _, _ := syscall_syscall(fnsemaphore_signal, uintptr(sem), 0, 0)
	return int32(r)
}

// semaphoreTimedWait is semaphore_timedwait(sem, mach_timespec_t{sec, nsec}).
// The struct is eight bytes and travels by value in one register, with tv_sec
// in the low word. It returns 0, kernTimedOut, kernAborted or an error.
func semaphoreTimedWait(sem uint32, d time.Duration) int32 {
	if d < 0 {
		d = 0
	}
	sec, nsec := uint64(d/time.Second), uint64(d%time.Second)
	r, _, _ := syscall_syscall(fnsemaphore_timedwait, uintptr(sem), uintptr(sec|nsec<<32), 0)
	return int32(r)
}

// pthreadSigmask is int pthread_sigmask(int how, const sigset_t *set, sigset_t *oldset).
// A sigset_t is one uint32 on macOS, with bit sig-1 standing for signal sig.
func pthreadSigmask(how int, set, old *uint32) int32 {
	r, _, _ := syscall_syscall(fnpthread_sigmask, uintptr(how), uintptr(unsafe.Pointer(set)), uintptr(unsafe.Pointer(old)))
	return int32(r)
}
