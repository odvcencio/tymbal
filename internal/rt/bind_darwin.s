//go:build darwin && (amd64 || arm64)

#include "textflag.h"

// One trampoline per imported symbol, as in internal/coreaudio/bind_darwin.s,
// crypto/x509/internal/macos and golang.org/x/sys/unix. The body is a bare jump
// to the dynamically imported symbol (see the //go:cgo_import_dynamic lines in
// bind_darwin.go), so the trampoline's address is itself a valid C function
// pointer. The fn* DATA symbols publish each address to Go.

TEXT tymbal_rt_pthread_self_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_rt_pthread_self(SB)
GLOBL	·fnpthread_self(SB), RODATA, $8
DATA	·fnpthread_self(SB)/8, $tymbal_rt_pthread_self_trampoline<>(SB)

TEXT tymbal_rt_pthread_mach_thread_np_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_rt_pthread_mach_thread_np(SB)
GLOBL	·fnpthread_mach_thread_np(SB), RODATA, $8
DATA	·fnpthread_mach_thread_np(SB)/8, $tymbal_rt_pthread_mach_thread_np_trampoline<>(SB)

TEXT tymbal_rt_mach_timebase_info_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_rt_mach_timebase_info(SB)
GLOBL	·fnmach_timebase_info(SB), RODATA, $8
DATA	·fnmach_timebase_info(SB)/8, $tymbal_rt_mach_timebase_info_trampoline<>(SB)

TEXT tymbal_rt_thread_policy_set_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_rt_thread_policy_set(SB)
GLOBL	·fnthread_policy_set(SB), RODATA, $8
DATA	·fnthread_policy_set(SB)/8, $tymbal_rt_thread_policy_set_trampoline<>(SB)

TEXT tymbal_rt_thread_policy_get_trampoline<>(SB),NOSPLIT,$0-0
	JMP	tymbal_rt_thread_policy_get(SB)
GLOBL	·fnthread_policy_get(SB), RODATA, $8
DATA	·fnthread_policy_get(SB)/8, $tymbal_rt_thread_policy_get_trampoline<>(SB)
