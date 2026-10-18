//go:build darwin && amd64

#include "textflag.h"
#include "go_asm.h"

// OSStatus render(void *refcon, AudioUnitRenderActionFlags *flags,
//                 const AudioTimeStamp *ts, UInt32 bus, UInt32 nframes,
//                 AudioBufferList *ioData)
//
// The AURenderCallback Core Audio runs on its real-time thread (AUComponent.h).
// Under the System V ABI the arguments arrive in DI (the ctrl block, as refcon),
// SI, DX, CX, R8 (nframes) and R9 (ioData). The algorithm is the one in
// render_darwin_arm64.s; read the comment there.
//
// This is code on a thread the Go runtime does not own, so:
//   - NOSPLIT|NOFRAME: no stack check, which would read g.
//   - It never refers to a symbol and never reads g (R14 is only a callee-saved
//     register here, restored before returning). Everything is read through the
//     ctrl block, whose offsets come from go_asm.h.
//   - BX, BP, R12-R15 are callee-saved under the C ABI and are saved. Entry SP
//     is 8 mod 16 and the 56-byte frame makes it 0, so SP is 16-byte aligned
//     at each call, as the C ABI requires. The frame is built with SUBQ and
//     MOVQ rather than PUSHQ/POPQ because the assembler checks PUSH/POP balance
//     in program order, and this function has two exits.
//   - x86 orders loads and stores strongly enough that plain MOVs give the
//     acquire load of head and the release store of tail.
TEXT ·render(SB), NOSPLIT|NOFRAME, $0-0
	SUBQ	$56, SP
	MOVQ	BP, 48(SP)
	LEAQ	48(SP), BP
	MOVQ	BX, 0(SP)
	MOVQ	R12, 8(SP)
	MOVQ	R13, 16(SP)
	MOVQ	R14, 24(SP)
	MOVQ	R15, 32(SP)

	MOVQ	DI, BX			// ctrl
	MOVQ	R9, R12			// ioData
	MOVL	R8, R14			// nframes

	// First call: block process signals on this thread (see blockedSignals).
	MOVQ	ctrl_masked(BX), AX
	TESTQ	AX, AX
	JNE	masked
	MOVQ	ctrl_sigmask(BX), AX
	TESTQ	AX, AX
	JEQ	masked
	MOVQ	$1, ctrl_masked(BX)
	MOVL	$1, DI			// SIG_BLOCK
	LEAQ	ctrl_sigset(BX), SI
	XORL	DX, DX
	CALL	AX
masked:

	// ioData: AudioBufferList { UInt32 mNumberBuffers; AudioBuffer mBuffers[1] },
	// AudioBuffer { UInt32 mNumberChannels, mDataByteSize; void *mData }.
	CMPL	(R12), $1		// mNumberBuffers
	JNE	bad
	MOVL	12(R12), AX		// mBuffers[0].mDataByteSize
	MOVQ	16(R12), R12		// mBuffers[0].mData, now the destination
	TESTQ	R12, R12
	JEQ	bad
	MOVQ	ctrl_fbytes(BX), CX
	TESTQ	CX, CX
	JEQ	bad

	// R14 = req = min(nframes, frames that fit in the destination)
	XORL	DX, DX
	DIVQ	CX
	CMPQ	AX, R14
	JCC	reqok
	MOVQ	AX, R14
reqok:

	// R13 = n = min(req, frames available)
	MOVQ	(BX), AX		// head
	MOVQ	ctrl_tail(BX), R8	// tail: only this thread writes it
	SUBQ	R8, AX
	MOVQ	R14, R13
	CMPQ	AX, R13
	JCC	nok
	MOVQ	AX, R13
nok:

	// R15 = first = min(n, frames before the ring wraps)
	MOVQ	ctrl_mask(BX), CX
	MOVQ	R8, DX
	ANDQ	CX, DX			// idx = tail & mask
	INCQ	CX			// capacity
	SUBQ	DX, CX
	MOVQ	R13, R15
	CMPQ	CX, R15
	JCC	firstok
	MOVQ	CX, R15
firstok:

	TESTQ	R15, R15
	JEQ	second
	MOVQ	ctrl_fbytes(BX), AX
	IMULQ	AX, DX
	ADDQ	ctrl_data(BX), DX
	MOVQ	DX, SI			// src = data + idx*fbytes
	MOVQ	R12, DI			// dst
	MOVQ	R15, DX
	IMULQ	AX, DX			// first*fbytes
	MOVQ	ctrl_memcpy(BX), AX
	CALL	AX

second:
	MOVQ	R13, AX
	SUBQ	R15, AX			// rest = n - first
	JEQ	shortfall
	MOVQ	ctrl_fbytes(BX), CX
	MOVQ	R15, DX
	IMULQ	CX, DX
	LEAQ	(R12)(DX*1), DI		// dst + first*fbytes
	MOVQ	ctrl_data(BX), SI	// src = ring base
	IMULQ	CX, AX
	MOVQ	AX, DX			// rest*fbytes
	MOVQ	ctrl_memcpy(BX), AX
	CALL	AX

shortfall:
	MOVQ	R14, AX
	SUBQ	R13, AX			// req - n
	JEQ	publish
	INCQ	ctrl_under(BX)
	ADDQ	AX, ctrl_underFrames(BX)
	MOVQ	ctrl_fbytes(BX), CX
	MOVQ	R13, DX
	IMULQ	CX, DX
	LEAQ	(R12)(DX*1), DI		// dst + n*fbytes
	XORL	SI, SI
	IMULQ	CX, AX
	MOVQ	AX, DX			// (req-n)*fbytes
	MOVQ	ctrl_memset(BX), AX
	CALL	AX

publish:
	MOVQ	ctrl_tail(BX), AX
	ADDQ	R13, AX
	MOVQ	AX, ctrl_tail(BX)	// the copy above is done with the ring
	INCQ	ctrl_calls(BX)
	MOVL	ctrl_sem(BX), DI
	MOVQ	ctrl_signal(BX), AX
	MOVQ	0(SP), BX
	MOVQ	8(SP), R12
	MOVQ	16(SP), R13
	MOVQ	24(SP), R14
	MOVQ	32(SP), R15
	MOVQ	48(SP), BP
	ADDQ	$56, SP
	JMP	AX			// semaphore_signal(sem) returns to Core Audio

bad:
	INCQ	ctrl_bad(BX)
	XORL	AX, AX
	MOVQ	0(SP), BX
	MOVQ	8(SP), R12
	MOVQ	16(SP), R13
	MOVQ	24(SP), R14
	MOVQ	32(SP), R15
	MOVQ	48(SP), BP
	ADDQ	$56, SP
	RET

// renderABI0 is the address of render, for AURenderCallbackStruct. Read from
// Go as a uintptr: the func value of an assembly function points at a wrapper.
GLOBL	·renderABI0(SB), NOPTR|RODATA, $8
DATA	·renderABI0(SB)/8, $·render(SB)
