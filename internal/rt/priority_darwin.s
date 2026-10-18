//go:build darwin && (amd64 || arm64)

#include "textflag.h"

TEXT tymbal_rt_mach_thread_self_trampoline<>(SB),NOSPLIT,$0-0
	JMP tymbal_rt_mach_thread_self(SB)
GLOBL ·fnRTThreadSelf(SB),RODATA,$8
DATA ·fnRTThreadSelf(SB)/8,$tymbal_rt_mach_thread_self_trampoline<>(SB)
TEXT tymbal_rt_mach_task_self_trampoline<>(SB),NOSPLIT,$0-0
	JMP tymbal_rt_mach_task_self(SB)
GLOBL ·fnRTTaskSelf(SB),RODATA,$8
DATA ·fnRTTaskSelf(SB)/8,$tymbal_rt_mach_task_self_trampoline<>(SB)
TEXT tymbal_rt_mach_port_deallocate_trampoline<>(SB),NOSPLIT,$0-0
	JMP tymbal_rt_mach_port_deallocate(SB)
GLOBL ·fnRTPortDeallocate(SB),RODATA,$8
DATA ·fnRTPortDeallocate(SB)/8,$tymbal_rt_mach_port_deallocate_trampoline<>(SB)
TEXT tymbal_rt_mach_timebase_info_trampoline<>(SB),NOSPLIT,$0-0
	JMP tymbal_rt_mach_timebase_info(SB)
GLOBL ·fnRTTimebase(SB),RODATA,$8
DATA ·fnRTTimebase(SB)/8,$tymbal_rt_mach_timebase_info_trampoline<>(SB)
TEXT tymbal_rt_thread_policy_get_trampoline<>(SB),NOSPLIT,$0-0
	JMP tymbal_rt_thread_policy_get(SB)
GLOBL ·fnRTPolicyGet(SB),RODATA,$8
DATA ·fnRTPolicyGet(SB)/8,$tymbal_rt_thread_policy_get_trampoline<>(SB)
TEXT tymbal_rt_thread_policy_set_trampoline<>(SB),NOSPLIT,$0-0
	JMP tymbal_rt_thread_policy_set(SB)
GLOBL ·fnRTPolicySet(SB),RODATA,$8
DATA ·fnRTPolicySet(SB)/8,$tymbal_rt_thread_policy_set_trampoline<>(SB)
