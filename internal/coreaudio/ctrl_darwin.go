//go:build darwin && (amd64 || arm64)

package coreaudio

// ctrl is the hand-off block between the Go stream thread and the render
// trampoline, and the only state the trampoline can see.
//
// Core Audio calls the trampoline on a real-time thread it owns, a thread the
// Go runtime has never heard of. Nothing the trampoline touches may be Go
// memory the garbage collector could move or free, and it cannot call Go code:
// there is no g on that thread, and entering the runtime through
// runtime.cgocallback would need cgo's machinery (iscgo, an extra M) that a
// CGO_ENABLED=0 binary does not have. So the trampoline is written in
// assembly (render_darwin_*.s) and talks to Go only through this struct and
// the ring behind data, both carved out of one ring.Arena (mmap, outside the
// Go heap), the same arena rule the ALSA backend follows.
//
// One producer and one consumer, as in internal/ring:
//
//   - Go writes frames into the ring and publishes them with an atomic store
//     to head.
//   - The trampoline reads head with acquire semantics, copies frames into
//     Core Audio's buffer, zero-fills whatever is missing, publishes tail with
//     release semantics, then signals the Mach semaphore.
//
// Positions count frames, never wrap, and are reduced with mask. The assembly
// reads every field through go_asm.h, so this declaration is the layout. Keep
// head and tail on separate cache lines.
type ctrl struct {
	head uint64 // frames the Go thread has published, ever; written only by Go
	_    [56]byte
	tail uint64 // frames the trampoline has consumed, ever; written only by the trampoline
	_    [56]byte

	mask    uint64  // ring capacity in frames, minus one; the capacity is a power of two
	fbytes  uint64  // bytes per frame (channels * 4)
	data    uintptr // ring base
	sem     uint64  // Mach semaphore (semaphore_t is 32 bits, in the low word)
	signal  uintptr // semaphore_signal
	memcpy  uintptr // memcpy
	memset  uintptr // memset
	sigmask uintptr // pthread_sigmask, or 0 to leave the render thread's signal mask alone
	sigset  uint64  // signals to block on the render thread (a darwin sigset_t, in the low word)
	masked  uint64  // set by the trampoline once it has blocked sigset

	// Counters, written only by the trampoline and read atomically by Go.
	calls       uint64 // callbacks served
	under       uint64 // callbacks that found fewer frames than Core Audio asked for
	underFrames uint64 // frames of silence substituted for them
	bad         uint64 // callbacks skipped: ioData was not one writable interleaved buffer
}

// render is the AURenderCallback, in assembly. It is never called from Go; its
// address reaches Core Audio through renderABI0. Declared so that go vet can
// check the assembly's frame against it.
func render()

// renderABI0 is the address of render, published by the assembly. A Go func
// value would point at an ABIInternal wrapper, not at the C-ABI entry.
var renderABI0 uintptr

// blockedSignals is what the trampoline blocks on Core Audio's thread: every
// signal that can be delivered to any thread of the process, except the
// synchronous faults, which must still reach their handler to be reported.
// SIGKILL and SIGSTOP cannot be blocked, so they are left out of the set.
//
// A signal sent to the process (SIGCHLD, SIGPIPE, a profiling tick, ^C) may be
// delivered to any thread that does not block it. The Go runtime's handler,
// run on a thread with no g, calls badsignal, which in a binary without cgo
// prints "fatal: bad g in signal handler" and exits. Blocking the signals on
// the threads Core Audio creates leaves them for Go's own threads.
const blockedSignals = ^uint32(0) &^ (1<<(4-1) | // SIGILL
	1<<(5-1) | // SIGTRAP
	1<<(6-1) | // SIGABRT
	1<<(8-1) | // SIGFPE
	1<<(9-1) | // SIGKILL
	1<<(10-1) | // SIGBUS
	1<<(11-1) | // SIGSEGV
	1<<(12-1) | // SIGSYS
	1<<(17-1)) // SIGSTOP
