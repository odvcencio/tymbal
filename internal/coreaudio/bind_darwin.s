//go:build darwin && (amd64 || arm64)

#include "textflag.h"

// One trampoline per imported symbol, as in crypto/x509/internal/macos
// (corefoundation.s) and golang.org/x/sys/unix (zsyscall_darwin_*.s).
//
// The body is a bare jump to the dynamically imported symbol (see the
// //go:cgo_import_dynamic lines in bind_darwin.go); the linker routes it
// through its own stub and binding slot. Because it is a jump and not a call,
// the C function sees the caller's arguments and returns to the caller, so the
// trampoline's address is itself a valid C function pointer. Go passes it to
// syscall.syscall for calls made from Go, and the render callback calls the
// four it needs (memcpy, memset, semaphore_signal, pthread_sigmask) through
// the pointers stored in the control block.
//
// The fn* DATA symbols publish each trampoline's address to Go.

TEXT tymbal_AudioObjectGetPropertyData_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_AudioObjectGetPropertyData(SB)
GLOBL	·fnAudioObjectGetPropertyData(SB), RODATA, $8
DATA	·fnAudioObjectGetPropertyData(SB)/8, $tymbal_AudioObjectGetPropertyData_trampoline<>(SB)

TEXT tymbal_AudioObjectGetPropertyDataSize_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_AudioObjectGetPropertyDataSize(SB)
GLOBL	·fnAudioObjectGetPropertyDataSize(SB), RODATA, $8
DATA	·fnAudioObjectGetPropertyDataSize(SB)/8, $tymbal_AudioObjectGetPropertyDataSize_trampoline<>(SB)

TEXT tymbal_AudioObjectSetPropertyData_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_AudioObjectSetPropertyData(SB)
GLOBL	·fnAudioObjectSetPropertyData(SB), RODATA, $8
DATA	·fnAudioObjectSetPropertyData(SB)/8, $tymbal_AudioObjectSetPropertyData_trampoline<>(SB)

TEXT tymbal_AudioComponentFindNext_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_AudioComponentFindNext(SB)
GLOBL	·fnAudioComponentFindNext(SB), RODATA, $8
DATA	·fnAudioComponentFindNext(SB)/8, $tymbal_AudioComponentFindNext_trampoline<>(SB)

TEXT tymbal_AudioComponentInstanceNew_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_AudioComponentInstanceNew(SB)
GLOBL	·fnAudioComponentInstanceNew(SB), RODATA, $8
DATA	·fnAudioComponentInstanceNew(SB)/8, $tymbal_AudioComponentInstanceNew_trampoline<>(SB)

TEXT tymbal_AudioComponentInstanceDispose_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_AudioComponentInstanceDispose(SB)
GLOBL	·fnAudioComponentInstanceDispose(SB), RODATA, $8
DATA	·fnAudioComponentInstanceDispose(SB)/8, $tymbal_AudioComponentInstanceDispose_trampoline<>(SB)

TEXT tymbal_AudioUnitSetProperty_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_AudioUnitSetProperty(SB)
GLOBL	·fnAudioUnitSetProperty(SB), RODATA, $8
DATA	·fnAudioUnitSetProperty(SB)/8, $tymbal_AudioUnitSetProperty_trampoline<>(SB)

TEXT tymbal_AudioUnitInitialize_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_AudioUnitInitialize(SB)
GLOBL	·fnAudioUnitInitialize(SB), RODATA, $8
DATA	·fnAudioUnitInitialize(SB)/8, $tymbal_AudioUnitInitialize_trampoline<>(SB)

TEXT tymbal_AudioUnitUninitialize_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_AudioUnitUninitialize(SB)
GLOBL	·fnAudioUnitUninitialize(SB), RODATA, $8
DATA	·fnAudioUnitUninitialize(SB)/8, $tymbal_AudioUnitUninitialize_trampoline<>(SB)

TEXT tymbal_AudioOutputUnitStart_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_AudioOutputUnitStart(SB)
GLOBL	·fnAudioOutputUnitStart(SB), RODATA, $8
DATA	·fnAudioOutputUnitStart(SB)/8, $tymbal_AudioOutputUnitStart_trampoline<>(SB)

TEXT tymbal_AudioOutputUnitStop_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_AudioOutputUnitStop(SB)
GLOBL	·fnAudioOutputUnitStop(SB), RODATA, $8
DATA	·fnAudioOutputUnitStop(SB)/8, $tymbal_AudioOutputUnitStop_trampoline<>(SB)

TEXT tymbal_CFRelease_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_CFRelease(SB)
GLOBL	·fnCFRelease(SB), RODATA, $8
DATA	·fnCFRelease(SB)/8, $tymbal_CFRelease_trampoline<>(SB)

TEXT tymbal_CFStringGetLength_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_CFStringGetLength(SB)
GLOBL	·fnCFStringGetLength(SB), RODATA, $8
DATA	·fnCFStringGetLength(SB)/8, $tymbal_CFStringGetLength_trampoline<>(SB)

TEXT tymbal_CFStringGetMaximumSizeForEncoding_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_CFStringGetMaximumSizeForEncoding(SB)
GLOBL	·fnCFStringGetMaximumSizeForEncoding(SB), RODATA, $8
DATA	·fnCFStringGetMaximumSizeForEncoding(SB)/8, $tymbal_CFStringGetMaximumSizeForEncoding_trampoline<>(SB)

TEXT tymbal_CFStringGetCString_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_CFStringGetCString(SB)
GLOBL	·fnCFStringGetCString(SB), RODATA, $8
DATA	·fnCFStringGetCString(SB)/8, $tymbal_CFStringGetCString_trampoline<>(SB)

TEXT tymbal_mach_task_self_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_mach_task_self(SB)
GLOBL	·fnmach_task_self(SB), RODATA, $8
DATA	·fnmach_task_self(SB)/8, $tymbal_mach_task_self_trampoline<>(SB)

TEXT tymbal_semaphore_create_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_semaphore_create(SB)
GLOBL	·fnsemaphore_create(SB), RODATA, $8
DATA	·fnsemaphore_create(SB)/8, $tymbal_semaphore_create_trampoline<>(SB)

TEXT tymbal_semaphore_destroy_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_semaphore_destroy(SB)
GLOBL	·fnsemaphore_destroy(SB), RODATA, $8
DATA	·fnsemaphore_destroy(SB)/8, $tymbal_semaphore_destroy_trampoline<>(SB)

TEXT tymbal_semaphore_signal_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_semaphore_signal(SB)
GLOBL	·fnsemaphore_signal(SB), RODATA, $8
DATA	·fnsemaphore_signal(SB)/8, $tymbal_semaphore_signal_trampoline<>(SB)

TEXT tymbal_semaphore_timedwait_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_semaphore_timedwait(SB)
GLOBL	·fnsemaphore_timedwait(SB), RODATA, $8
DATA	·fnsemaphore_timedwait(SB)/8, $tymbal_semaphore_timedwait_trampoline<>(SB)

TEXT tymbal_pthread_sigmask_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_pthread_sigmask(SB)
GLOBL	·fnpthread_sigmask(SB), RODATA, $8
DATA	·fnpthread_sigmask(SB)/8, $tymbal_pthread_sigmask_trampoline<>(SB)

TEXT tymbal_memcpy_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_memcpy(SB)
GLOBL	·fnmemcpy(SB), RODATA, $8
DATA	·fnmemcpy(SB)/8, $tymbal_memcpy_trampoline<>(SB)

TEXT tymbal_memset_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_memset(SB)
GLOBL	·fnmemset(SB), RODATA, $8
DATA	·fnmemset(SB)/8, $tymbal_memset_trampoline<>(SB)

