//go:build darwin && arm64

#include "textflag.h"
#include "go_asm.h"

// OSStatus render(void *refcon, AudioUnitRenderActionFlags *flags,
//                 const AudioTimeStamp *ts, UInt32 bus, UInt32 nframes,
//                 AudioBufferList *ioData)
//
// The AURenderCallback Core Audio runs on its real-time thread (AUComponent.h).
// Arguments arrive in R0-R5 under the C ABI. R0 is the ctrl block (refcon).
//
// It moves frames from the ring into ioData and returns, so the Go thread can
// refill it:
//
//	if first call and sigmask != 0: block process signals on this thread
//	require one buffer with storage            else bad++, return 0
//	req   = min(nframes, buffer bytes / fbytes)
//	n     = min(req, head - tail)              head loaded with acquire
//	copy n frames from data[tail & mask], in two pieces when the ring wraps
//	zero the other req-n frames                under++, underFrames += req-n
//	tail += n                                  stored with release
//	return semaphore_signal(sem)               a tail call
//
// This is code on a thread the Go runtime does not own, so:
//   - NOSPLIT|NOFRAME: no stack check, which would read g, and g is garbage here.
//   - It never touches R18 (the platform register), R26 (closure context),
//     R27 (the assembler's temporary) or R28 (g), and never refers to a symbol
//     (that would use R27). Everything is read through the ctrl block, whose
//     offsets come from go_asm.h. Plain R0-R17 are scratch; R19-R24 are saved.
//   - The only calls are through pointers in the ctrl block, to memcpy, memset,
//     pthread_sigmask and semaphore_signal. None of them re-enters Go.
//   - SP is 16-byte aligned throughout.
TEXT ·render(SB), NOSPLIT|NOFRAME, $0-0
	STP.W	(R29, R30), -64(RSP)
	MOVD	RSP, R29
	STP	(R19, R20), 16(RSP)
	STP	(R21, R22), 32(RSP)
	STP	(R23, R24), 48(RSP)

	MOVD	R0, R19			// ctrl
	MOVD	R5, R20			// ioData
	MOVWU	R4, R22			// nframes

	// First call: block process signals on this thread (see blockedSignals).
	MOVD	ctrl_masked(R19), R9
	CBNZ	R9, masked
	MOVD	ctrl_sigmask(R19), R9
	CBZ	R9, masked
	MOVD	$1, R10
	MOVD	R10, ctrl_masked(R19)
	MOVD	$1, R0			// SIG_BLOCK
	ADD	$ctrl_sigset, R19, R1
	MOVD	ZR, R2
	BL	(R9)
masked:

	// ioData: AudioBufferList { UInt32 mNumberBuffers; AudioBuffer mBuffers[1] },
	// AudioBuffer { UInt32 mNumberChannels, mDataByteSize; void *mData }.
	MOVWU	(R20), R9		// mNumberBuffers
	CMPW	$1, R9
	BNE	bad
	MOVWU	12(R20), R10		// mBuffers[0].mDataByteSize
	MOVD	16(R20), R20		// mBuffers[0].mData, now the destination
	CBZ	R20, bad
	MOVD	ctrl_fbytes(R19), R11
	CBZ	R11, bad

	// R22 = req = min(nframes, frames that fit in the destination)
	UDIV	R11, R10, R10
	CMP	R10, R22
	CSEL	LO, R22, R10, R22

	// R21 = n = min(req, frames available)
	LDAR	(R19), R12		// head, acquire
	MOVD	ctrl_tail(R19), R24	// tail: only this thread writes it
	SUB	R24, R12, R12
	CMP	R12, R22
	CSEL	LO, R22, R12, R21

	// R23 = first = min(n, frames before the ring wraps)
	MOVD	ctrl_mask(R19), R13
	AND	R13, R24, R14		// idx = tail & mask
	ADD	$1, R13, R13		// capacity
	SUB	R14, R13, R13
	CMP	R13, R21
	CSEL	LO, R21, R13, R23

	CBZ	R23, second
	MOVD	ctrl_data(R19), R15
	MUL	R11, R14, R14
	ADD	R14, R15, R1		// src = data + idx*fbytes
	MOVD	R20, R0			// dst
	MUL	R11, R23, R2		// first*fbytes
	MOVD	ctrl_memcpy(R19), R9
	BL	(R9)

second:
	SUB	R23, R21, R3		// rest = n - first
	CBZ	R3, shortfall
	MOVD	ctrl_fbytes(R19), R11
	MUL	R11, R23, R4
	ADD	R4, R20, R0		// dst + first*fbytes
	MOVD	ctrl_data(R19), R1	// src = ring base
	MUL	R11, R3, R2
	MOVD	ctrl_memcpy(R19), R9
	BL	(R9)

shortfall:
	SUBS	R21, R22, R3		// req - n
	BEQ	publish
	MOVD	ctrl_under(R19), R9
	ADD	$1, R9
	MOVD	R9, ctrl_under(R19)
	MOVD	ctrl_underFrames(R19), R9
	ADD	R3, R9
	MOVD	R9, ctrl_underFrames(R19)
	MOVD	ctrl_fbytes(R19), R11
	MUL	R11, R21, R4
	ADD	R4, R20, R0		// dst + n*fbytes
	MOVD	ZR, R1
	MUL	R11, R3, R2
	MOVD	ctrl_memset(R19), R9
	BL	(R9)

publish:
	ADD	R21, R24, R12		// tail + n
	ADD	$ctrl_tail, R19, R13
	STLR	R12, (R13)		// release: the copy above is done with the ring
	MOVD	ctrl_calls(R19), R9
	ADD	$1, R9
	MOVD	R9, ctrl_calls(R19)
	MOVWU	ctrl_sem(R19), R0
	MOVD	ctrl_signal(R19), R9
	LDP	16(RSP), (R19, R20)
	LDP	32(RSP), (R21, R22)
	LDP	48(RSP), (R23, R24)
	LDP.P	64(RSP), (R29, R30)
	JMP	(R9)			// semaphore_signal(sem) returns to Core Audio

bad:
	MOVD	ctrl_bad(R19), R9
	ADD	$1, R9
	MOVD	R9, ctrl_bad(R19)
	MOVD	ZR, R0
	LDP	16(RSP), (R19, R20)
	LDP	32(RSP), (R21, R22)
	LDP	48(RSP), (R23, R24)
	LDP.P	64(RSP), (R29, R30)
	RET

// renderABI0 is the address of render, for AURenderCallbackStruct. Read from
// Go as a uintptr: the func value of an assembly function points at a wrapper.
GLOBL	·renderABI0(SB), NOPTR|RODATA, $8
DATA	·renderABI0(SB)/8, $·render(SB)
