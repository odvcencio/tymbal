//go:build darwin && arm64

#include "textflag.h"
#include "go_asm.h"

// HAL input notification. AudioUnitRender fills native staging memory; the
// complete packet is copied to the SPSC ring. This thread never enters Go.
TEXT ·capture(SB), NOSPLIT|NOFRAME, $0-0
	STP.W (R29,R30), -80(RSP)
	MOVD RSP,R29
	STP (R19,R20),16(RSP)
	STP (R21,R22),32(RSP)
	STP (R23,R24),48(RSP)
	MOVD R1,64(RSP)
	MOVD R0,R19
	MOVD R2,R21
	MOVWU R4,R22
	MOVD ctrl_masked(R19),R9
	CBNZ R9,capture_masked
	MOVD ctrl_sigmask(R19),R9
	CBZ R9,capture_masked
	MOVD $1,R10
	MOVD R10,ctrl_masked(R19)
	MOVD $1,R0
	ADD $ctrl_sigset,R19,R1
	MOVD ZR,R2
	BL (R9)
capture_masked:
	MOVD inputCtrl_maxFrames(R19),R9
	CMP R22,R9
	BLO capture_large
	MOVD inputCtrl_list(R19),R20
	MOVD ctrl_fbytes(R19),R9
	MUL R9,R22,R10
	MOVW R10,12(R20)
	MOVD inputCtrl_render(R19),R9
	CBZ R9,capture_rendered
	MOVD inputCtrl_unit(R19),R0
	MOVD 64(RSP),R1
	MOVD R21,R2
	MOVD $1,R3
	MOVD R22,R4
	MOVD R20,R5
	BL (R9)
	CBNZ R0,capture_error
capture_rendered:
	MOVD 16(R20),R20
	MOVD ctrl_head(R19),R23
	ADD $ctrl_tail,R19,R9
	LDAR (R9),R10
	SUB R10,R23,R10
	MOVD ctrl_mask(R19),R11
	ADD $1,R11,R12
	SUB R10,R12,R12
	CMP R22,R12
	BLO capture_overflow
	AND R11,R23,R13
	ADD $1,R11,R11
	SUB R13,R11,R11
	CMP R11,R22
	CSEL LO,R22,R11,R24
	CBZ R24,capture_second
	MOVD ctrl_fbytes(R19),R9
	MUL R9,R13,R13
	MOVD ctrl_data(R19),R0
	ADD R13,R0,R0
	MOVD R20,R1
	MUL R9,R24,R2
	MOVD ctrl_memcpy(R19),R9
	BL (R9)
capture_second:
	SUB R24,R22,R10
	CBZ R10,capture_stamp
	MOVD ctrl_fbytes(R19),R9
	MUL R9,R24,R11
	ADD R11,R20,R1
	MOVD ctrl_data(R19),R0
	MUL R9,R10,R2
	MOVD ctrl_memcpy(R19),R9
	BL (R9)
capture_stamp:
	MOVD inputCtrl_stampSeq(R19),R9
	ADD $1,R9
	MOVD R9,inputCtrl_stampSeq(R19)
	DMB $0xe
	MOVD R23,inputCtrl_stampHead(R19)
	CBZ R21,capture_no_stamp
	MOVD (R21),R10
	MOVD R10,inputCtrl_sampleBits(R19)
	MOVD 8(R21),R10
	MOVD R10,inputCtrl_hostTicks(R19)
	MOVWU 56(R21),R10
	MOVD R10,inputCtrl_stampFlags(R19)
	B capture_stamped
capture_no_stamp:
	MOVD ZR,inputCtrl_stampFlags(R19)
capture_stamped:
	ADD $1,R9
	ADD $inputCtrl_stampSeq,R19,R10
	STLR R9,(R10)
	ADD R22,R23,R23
	STLR R23,(R19)
	B capture_signal
capture_large:
	MOVD $-10874,R0
capture_error:
	MOVD R0,inputCtrl_status(R19)
	B capture_overflow
capture_overflow:
	MOVD ctrl_under(R19),R9
	ADD $1,R9
	MOVD R9,ctrl_under(R19)
	MOVD ctrl_underFrames(R19),R9
	ADD R22,R9
	MOVD R9,ctrl_underFrames(R19)
capture_signal:
	MOVD ctrl_calls(R19),R9
	ADD $1,R9
	MOVD R9,ctrl_calls(R19)
	MOVWU ctrl_sem(R19),R0
	MOVD ctrl_signal(R19),R9
	LDP 16(RSP),(R19,R20)
	LDP 32(RSP),(R21,R22)
	LDP 48(RSP),(R23,R24)
	LDP.P 80(RSP),(R29,R30)
	JMP (R9)

GLOBL ·captureABI0(SB), NOPTR|RODATA, $8
DATA ·captureABI0(SB)/8, $·capture(SB)
