//go:build darwin && amd64

#include "textflag.h"
#include "go_asm.h"

// Same native-only input protocol as capture_darwin_arm64.s.
TEXT ·capture(SB), NOSPLIT|NOFRAME, $0-0
	SUBQ $88,SP
	MOVQ BP,80(SP)
	LEAQ 80(SP),BP
	MOVQ BX,0(SP)
	MOVQ R12,8(SP)
	MOVQ R13,16(SP)
	MOVQ R14,24(SP)
	MOVQ R15,32(SP)
	MOVQ SI,56(SP)
	MOVQ DI,BX
	MOVQ DX,R12
	MOVL R8,R14
	MOVQ ctrl_masked(BX),AX
	TESTQ AX,AX
	JNE capture_masked
	MOVQ ctrl_sigmask(BX),AX
	TESTQ AX,AX
	JEQ capture_masked
	MOVQ $1,ctrl_masked(BX)
	MOVL $1,DI
	LEAQ ctrl_sigset(BX),SI
	XORL DX,DX
	CALL AX
capture_masked:
	MOVQ inputCtrl_maxFrames(BX),AX
	CMPQ AX,R14
	JCS capture_large
	MOVQ inputCtrl_list(BX),R13
	MOVQ ctrl_fbytes(BX),AX
	IMULQ R14,AX
	MOVL AX,12(R13)
	MOVQ inputCtrl_render(BX),AX
	TESTQ AX,AX
	JEQ capture_rendered
	MOVQ inputCtrl_unit(BX),DI
	MOVQ 56(SP),SI
	MOVQ R12,DX
	MOVL $1,CX
	MOVQ R14,R8
	MOVQ R13,R9
	CALL AX
	TESTL AX,AX
	JNE capture_error
capture_rendered:
	MOVQ 16(R13),R13
	MOVQ ctrl_head(BX),R15
	MOVQ R15,DX
	SUBQ ctrl_tail(BX),DX
	MOVQ ctrl_mask(BX),AX
	INCQ AX
	SUBQ DX,AX
	CMPQ AX,R14
	JCS capture_overflow
	MOVQ ctrl_mask(BX),AX
	MOVQ R15,DX
	ANDQ AX,DX
	INCQ AX
	SUBQ DX,AX
	CMPQ AX,R14
	JCC capture_first_ready
	MOVQ AX,40(SP)
	JMP capture_copy_first
capture_first_ready:
	MOVQ R14,40(SP)
capture_copy_first:
	MOVQ ctrl_fbytes(BX),AX
	IMULQ AX,DX
	ADDQ ctrl_data(BX),DX
	MOVQ DX,DI
	MOVQ R13,SI
	MOVQ 40(SP),DX
	IMULQ AX,DX
	MOVQ ctrl_memcpy(BX),AX
	CALL AX
	MOVQ R14,DX
	SUBQ 40(SP),DX
	JEQ capture_stamp
	MOVQ ctrl_fbytes(BX),AX
	IMULQ AX,DX
	MOVQ 40(SP),CX
	IMULQ AX,CX
	LEAQ (R13)(CX*1),SI
	MOVQ ctrl_data(BX),DI
	MOVQ ctrl_memcpy(BX),AX
	CALL AX
capture_stamp:
	INCQ inputCtrl_stampSeq(BX)
	MOVQ R15,inputCtrl_stampHead(BX)
	TESTQ R12,R12
	JEQ capture_no_stamp
	MOVQ (R12),AX
	MOVQ AX,inputCtrl_sampleBits(BX)
	MOVQ 8(R12),AX
	MOVQ AX,inputCtrl_hostTicks(BX)
	MOVL 56(R12),AX
	MOVQ AX,inputCtrl_stampFlags(BX)
	JMP capture_stamped
capture_no_stamp:
	MOVQ $0,inputCtrl_stampFlags(BX)
capture_stamped:
	INCQ inputCtrl_stampSeq(BX)
	ADDQ R14,R15
	MOVQ R15,ctrl_head(BX)
	JMP capture_signal
capture_large:
	MOVQ $-10874,AX
capture_error:
	MOVQ AX,inputCtrl_status(BX)
capture_overflow:
	INCQ ctrl_under(BX)
	ADDQ R14,ctrl_underFrames(BX)
capture_signal:
	INCQ ctrl_calls(BX)
	MOVL ctrl_sem(BX),DI
	MOVQ ctrl_signal(BX),AX
	MOVQ 0(SP),BX
	MOVQ 8(SP),R12
	MOVQ 16(SP),R13
	MOVQ 24(SP),R14
	MOVQ 32(SP),R15
	MOVQ 80(SP),BP
	ADDQ $88,SP
	JMP AX

GLOBL ·captureABI0(SB), NOPTR|RODATA, $8
DATA ·captureABI0(SB)/8, $·capture(SB)
