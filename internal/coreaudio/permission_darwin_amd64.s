//go:build darwin && amd64

#include "textflag.h"
#include "go_asm.h"

// C ABI Block invoke(self, BOOL granted). x86 stores are already release-ordered.
TEXT ·permissionInvoke(SB),NOSPLIT|NOFRAME,$0-0
	MOVQ permissionBlock_result(DI), AX
	ANDL $1, SI
	ADDL $1, SI
	MOVL SI, (AX)
	RET

GLOBL ·fnPermissionInvoke(SB), RODATA, $8
DATA ·fnPermissionInvoke(SB)/8, $·permissionInvoke(SB)

TEXT tymbal_permission_msgSend_trampoline<>(SB),NOSPLIT,$0-0
	JMP tymbal_permission_objc_msgSend(SB)
GLOBL ·fnPermissionMsgSend(SB), RODATA, $8
DATA ·fnPermissionMsgSend(SB)/8, $tymbal_permission_msgSend_trampoline<>(SB)
TEXT tymbal_permission_getClass_trampoline<>(SB),NOSPLIT,$0-0
	JMP tymbal_permission_objc_getClass(SB)
GLOBL ·fnPermissionGetClass(SB), RODATA, $8
DATA ·fnPermissionGetClass(SB)/8, $tymbal_permission_getClass_trampoline<>(SB)
TEXT tymbal_permission_selector_trampoline<>(SB),NOSPLIT,$0-0
	JMP tymbal_permission_sel_registerName(SB)
GLOBL ·fnPermissionSelector(SB), RODATA, $8
DATA ·fnPermissionSelector(SB)/8, $tymbal_permission_selector_trampoline<>(SB)
TEXT tymbal_permission_poolPush_trampoline<>(SB),NOSPLIT,$0-0
	JMP tymbal_permission_poolPush(SB)
GLOBL ·fnPermissionPoolPush(SB), RODATA, $8
DATA ·fnPermissionPoolPush(SB)/8, $tymbal_permission_poolPush_trampoline<>(SB)
TEXT tymbal_permission_poolPop_trampoline<>(SB),NOSPLIT,$0-0
	JMP tymbal_permission_poolPop(SB)
GLOBL ·fnPermissionPoolPop(SB), RODATA, $8
DATA ·fnPermissionPoolPop(SB)/8, $tymbal_permission_poolPop_trampoline<>(SB)
TEXT tymbal_permission_dlsym_trampoline<>(SB),NOSPLIT,$0-0
	JMP tymbal_permission_dlsym(SB)
GLOBL ·fnPermissionDlsym(SB), RODATA, $8
DATA ·fnPermissionDlsym(SB)/8, $tymbal_permission_dlsym_trampoline<>(SB)
TEXT tymbal_permission_AVFoundation_trampoline<>(SB),NOSPLIT,$0-0
	JMP tymbal_permission_AVMakeRect(SB)
GLOBL ·fnPermissionAVFoundation(SB), RODATA, $8
DATA ·fnPermissionAVFoundation(SB)/8, $tymbal_permission_AVFoundation_trampoline<>(SB)
