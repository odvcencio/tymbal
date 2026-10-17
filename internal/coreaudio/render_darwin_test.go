//go:build darwin && (amd64 || arm64)

package coreaudio

import (
	"bytes"
	"encoding/binary"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
)

// audioBufferList is AudioBufferList with its single AudioBuffer, as AUHAL
// hands one to an interleaved render callback.
type audioBufferList struct {
	numberBuffers  uint32
	_              uint32
	numberChannels uint32
	dataByteSize   uint32
	data           uintptr
}

// callRender runs the real render trampoline on the calling thread through the
// C ABI, the way Core Audio's IO thread does. dst's storage is Go memory, which
// is fine here: the callee only uses it during the call.
func callRender(c *ctrl, dst []byte, frames int, channels uint32) int32 {
	abl := audioBufferList{numberBuffers: 1, numberChannels: channels, dataByteSize: uint32(len(dst))}
	if len(dst) > 0 {
		abl.data = uintptr(unsafe.Pointer(&dst[0]))
	}
	r, _, _ := syscall_syscall6(renderABI0, uintptr(unsafe.Pointer(c)), 0, 0, 0, uintptr(frames), uintptr(unsafe.Pointer(&abl)))
	runtime.KeepAlive(dst)
	return int32(r)
}

// pattern is the byte frame f, channel ch carries in the ring.
func pattern(f uint64, ch int) uint32 { return uint32(f)*8 + uint32(ch) + 1 }

func fillRing(t *testing.T, s *stream, from, to uint64) {
	t.Helper()
	ch := s.frameBytes / bytesPerSample
	for f := from; f < to; f++ {
		off := int((f & s.c.mask) * uint64(s.frameBytes))
		for i := 0; i < ch; i++ {
			binary.LittleEndian.PutUint32(s.data[off+i*bytesPerSample:], pattern(f, i))
		}
	}
}

func expectFrames(t *testing.T, got []byte, ch int, from uint64, n int) {
	t.Helper()
	for k := 0; k < n; k++ {
		for i := 0; i < ch; i++ {
			off := (k*ch + i) * bytesPerSample
			if v := binary.LittleEndian.Uint32(got[off:]); v != pattern(from+uint64(k), i) {
				t.Fatalf("frame %d channel %d: got %#x, want %#x", k, i, v, pattern(from+uint64(k), i))
			}
		}
	}
}

func newTestStream(t *testing.T, channels, period, fill int) *stream {
	t.Helper()
	s, err := newStream(streamConfig{rate: 48000, channels: channels, period: period, fill: fill})
	if err != nil {
		t.Fatalf("newStream: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	s.c.sigmask = 0 // do not mask the test's own thread (see TestRenderBlocksSignals)
	return s
}

func semaphorePending(s *stream) bool { return semaphoreTimedWait(s.sem, 20*time.Millisecond) == 0 }

func TestCtrlLayout(t *testing.T) {
	var c ctrl
	if got := unsafe.Offsetof(c.head); got != 0 {
		t.Errorf("head at %d, want 0", got)
	}
	if got := unsafe.Offsetof(c.tail); got != 64 {
		t.Errorf("tail at %d, want 64: head and tail must sit on separate cache lines", got)
	}
	if got := unsafe.Offsetof(c.mask); got < 128 {
		t.Errorf("mask at %d, want it past both counter lines", got)
	}
	if unsafe.Sizeof(c)%8 != 0 {
		t.Errorf("ctrl size %d is not a multiple of 8", unsafe.Sizeof(c))
	}
	if renderABI0 == 0 || fnsemaphore_signal == 0 || fnmemcpy == 0 || fnmemset == 0 || fnpthread_sigmask == 0 {
		t.Errorf("trampoline addresses not published: render=%#x signal=%#x memcpy=%#x memset=%#x sigmask=%#x",
			renderABI0, fnsemaphore_signal, fnmemcpy, fnmemset, fnpthread_sigmask)
	}
}

func TestRenderCopyWrapAndShortfall(t *testing.T) {
	const channels = 2
	s := newTestStream(t, channels, 64, 256) // ring of 256 frames
	fb := channels * bytesPerSample
	c := s.c

	type step struct {
		name       string
		head, tail uint64 // ring state before the call
		frames     int    // inNumberFrames
		dstFrames  int    // frames of storage Core Audio provides; 0 means frames
		copied     int    // frames expected from the ring
		silent     int    // frames expected zero-filled
	}
	steps := []step{
		{name: "plain", head: 256, tail: 0, frames: 64, copied: 64},
		{name: "wraps", head: 250 + 100, tail: 250, frames: 100, copied: 100},
		{name: "wrap lands exactly on the end", head: 192 + 64, tail: 192, frames: 64, copied: 64},
		{name: "shortfall", head: 1000 + 30, tail: 1000, frames: 100, copied: 30, silent: 70},
		{name: "empty ring", head: 777, tail: 777, frames: 50, silent: 50},
		{name: "shortfall across the wrap", head: 250 + 20, tail: 250, frames: 40, copied: 20, silent: 20},
		{name: "device asks for more than the ring holds", head: 5000 + 256, tail: 5000, frames: 300, dstFrames: 300, copied: 256, silent: 44},
		{name: "storage smaller than the request", head: 4096 + 256, tail: 4096, frames: 200, dstFrames: 120, copied: 120},
		{name: "zero frames", head: 100, tail: 100, frames: 0, dstFrames: 8},
		{name: "odd sizes", head: 3 + 7, tail: 3, frames: 7, copied: 7},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			atomic.StoreUint64(&c.tail, st.tail)
			atomic.StoreUint64(&c.head, st.head)
			fillRing(t, s, st.tail, st.head)
			dstFrames := st.dstFrames
			if dstFrames == 0 {
				dstFrames = st.frames
			}
			// Guard bytes after the destination catch a write past its end.
			buf := bytes.Repeat([]byte{0xAA}, dstFrames*fb+64)
			dst := buf[:dstFrames*fb]
			calls0, under0, underFrames0, bad0 := s.counters()

			status := callRender(c, dst, st.frames, channels)

			if status != 0 {
				t.Fatalf("render returned %d, want 0", status)
			}
			if !semaphorePending(s) {
				t.Fatal("render did not signal the semaphore")
			}
			calls, under, underFrames, bad := s.counters()
			if calls != calls0+1 || bad != bad0 {
				t.Errorf("calls %d->%d bad %d->%d", calls0, calls, bad0, bad)
			}
			if got := atomic.LoadUint64(&c.tail); got != st.tail+uint64(st.copied) {
				t.Errorf("tail = %d, want %d", got, st.tail+uint64(st.copied))
			}
			expectFrames(t, dst, channels, st.tail, st.copied)
			wantFilled := st.copied + st.silent
			if st.dstFrames != 0 && wantFilled > st.dstFrames {
				wantFilled = st.dstFrames
			}
			for i, b := range dst[st.copied*fb : wantFilled*fb] {
				if b != 0 {
					t.Fatalf("shortfall byte %d is %#x, want 0", i, b)
				}
			}
			for i, b := range dst[wantFilled*fb:] {
				if b != 0xAA {
					t.Fatalf("byte %d past the filled region was written (%#x)", wantFilled*fb+i, b)
				}
			}
			for i, b := range buf[len(dst):] {
				if b != 0xAA {
					t.Fatalf("guard byte %d was written (%#x)", i, b)
				}
			}
			wantUnder := uint64(0)
			if st.silent > 0 {
				wantUnder = 1
			}
			if under != under0+wantUnder || underFrames != underFrames0+uint64(st.silent) {
				t.Errorf("under %d->%d, underFrames %d->%d, want +%d and +%d", under0, under, underFrames0, underFrames, wantUnder, st.silent)
			}
		})
	}
}

func TestRenderRejectsUnexpectedBufferLists(t *testing.T) {
	s := newTestStream(t, 2, 64, 256)
	c := s.c
	atomic.StoreUint64(&c.head, 256)
	buf := bytes.Repeat([]byte{0xAA}, 64*8)

	for _, tc := range []struct {
		name string
		abl  audioBufferList
	}{
		{"two buffers", audioBufferList{numberBuffers: 2, numberChannels: 1, dataByteSize: uint32(len(buf)), data: uintptr(unsafe.Pointer(&buf[0]))}},
		{"no buffers", audioBufferList{numberBuffers: 0}},
		{"no storage", audioBufferList{numberBuffers: 1, numberChannels: 2, dataByteSize: 512}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, bad0 := s.counters()
			abl := tc.abl
			r, _, _ := syscall_syscall6(renderABI0, uintptr(unsafe.Pointer(c)), 0, 0, 0, 64, uintptr(unsafe.Pointer(&abl)))
			runtime.KeepAlive(buf)
			if int32(r) != 0 {
				t.Errorf("render returned %d, want 0", int32(r))
			}
			if _, _, _, bad := s.counters(); bad != bad0+1 {
				t.Errorf("bad %d->%d, want +1", bad0, bad)
			}
			if got := atomic.LoadUint64(&c.tail); got != 0 {
				t.Errorf("tail moved to %d on a rejected call", got)
			}
			if semaphoreTimedWait(s.sem, 5*time.Millisecond) == 0 {
				t.Error("a rejected call signalled the semaphore")
			}
		})
	}
	for _, b := range buf {
		if b != 0xAA {
			t.Fatal("a rejected call wrote to the buffer")
		}
	}
}

// A render callback that leaves the thread's signal mask alone would let a
// process-directed signal run Go's handler on Core Audio's thread. Check that
// the first call blocks them, on a thread of our own, and put the mask back.
func TestRenderBlocksSignals(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var before uint32
	if st := pthreadSigmask(1, nil, &before); st != 0 { // SIG_BLOCK with no set only reads
		t.Fatalf("pthread_sigmask read: %d", st)
	}
	defer pthreadSigmask(3, &before, nil) // SIG_SETMASK: restore for the next test on this thread

	s := newTestStream(t, 2, 64, 256)
	s.c.sigmask = fnpthread_sigmask // newTestStream cleared it
	atomic.StoreUint64(&s.c.head, 256)
	dst := make([]byte, 64*8)

	callRender(s.c, dst, 64, 2)
	var after uint32
	if st := pthreadSigmask(1, nil, &after); st != 0 {
		t.Fatalf("pthread_sigmask read: %d", st)
	}
	want := before | blockedSignals
	if after != want {
		t.Errorf("signal mask after first call = %#x, want %#x", after, want)
	}
	for _, sig := range []int{4, 5, 6, 8, 10, 11, 12} { // SIGILL SIGTRAP SIGABRT SIGFPE SIGBUS SIGSEGV SIGSYS
		if before&(1<<(sig-1)) == 0 && after&(1<<(sig-1)) != 0 {
			t.Errorf("synchronous signal %d was blocked", sig)
		}
	}
	if s.c.masked != 1 {
		t.Errorf("masked = %d, want 1", s.c.masked)
	}

	// The mask is applied once: restore it, call again, and it stays restored.
	pthreadSigmask(3, &before, nil)
	callRender(s.c, dst, 64, 2)
	var again uint32
	pthreadSigmask(1, nil, &again)
	if again != before {
		t.Errorf("second call changed the signal mask to %#x, want %#x", again, before)
	}
}

// TestProtocolWithStandInDevice runs the whole producer/consumer protocol, the
// Go stream thread against the real assembly trampoline, without an audio
// device. A goroutine pinned to its own OS thread plays Core Audio: it calls
// render with irregular frame counts. The Go side runs the same Wait, Buffers
// and Commit loop the stream core does. Whatever the scheduler does to
// timing, nothing may be lost, duplicated or reordered: the consumer's output
// must be the silence from Start followed by the exact sequence produced.
func TestProtocolWithStandInDevice(t *testing.T) {
	const (
		channels = 2
		period   = 64
		fill     = 256
		periods  = 3000
	)
	s := newTestStream(t, channels, period, fill)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fb := channels * bytesPerSample
	total := uint64(periods * period)

	var wg sync.WaitGroup
	var heard []byte
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		runtime.LockOSThread() // a thread of its own, as Core Audio's IO thread is; exits with the goroutine
		sizes := []int{64, 40, 100, 64, 7, 128, 64, 33}
		dst := make([]byte, 256*fb)
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			n := sizes[i%len(sizes)]
			_, _, before, _ := s.counters()
			callRender(s.c, dst[:n*fb], n, channels)
			_, _, after, _ := s.counters()
			heard = append(heard, dst[:(n-int(after-before))*fb]...)
			time.Sleep(50 * time.Microsecond)
		}
	}()

	var produced uint64
	for p := 0; p < periods; p++ {
		if err := s.Wait(); err != nil {
			t.Fatalf("Wait at period %d: %v", p, err)
		}
		_, out := s.Buffers()
		for f := 0; f < period; f++ {
			for ch := 0; ch < channels; ch++ {
				binary.LittleEndian.PutUint32(out[(f*channels+ch)*bytesPerSample:], math.Float32bits(float32(produced+uint64(f))*0.5+float32(ch)))
			}
		}
		if err := s.Commit(); err != nil {
			t.Fatalf("Commit at period %d: %v", p, err)
		}
		produced += period
	}
	// Let the stand-in drain what was committed, then stop it.
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadUint64(&s.c.tail) < fill+total && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(done)
	wg.Wait()

	// Anything past the committed frames is the trampoline's own silence.
	want := make([]byte, (fill+int(total))*fb)
	for f := uint64(0); f < total; f++ {
		for ch := 0; ch < channels; ch++ {
			binary.LittleEndian.PutUint32(want[(int(f)+fill)*fb+ch*bytesPerSample:], math.Float32bits(float32(f)*0.5+float32(ch)))
		}
	}
	if len(heard) < len(want) {
		t.Fatalf("stand-in device heard %d bytes, want at least %d", len(heard), len(want))
	}
	if !bytes.Equal(heard[:len(want)], want) {
		for i := range want {
			if heard[i] != want[i] {
				t.Fatalf("stream diverges at byte %d (frame %d): got %#x, want %#x", i, i/fb, heard[i], want[i])
			}
		}
	}
	calls, under, underFrames, bad := s.counters()
	t.Logf("%d render calls, %d periods produced, %d callbacks short by %d frames in total", calls, periods, under, underFrames)
	if bad != 0 {
		t.Errorf("bad = %d", bad)
	}
	if got := s.Dropouts(); got != under {
		t.Errorf("Dropouts() = %d, want %d", got, under)
	}
}

// Interrupt must wake a Wait that is blocked on the semaphore, and Close must
// be safe afterwards and idempotent.
func TestInterruptWakesWaitAndCloseIsIdempotent(t *testing.T) {
	s, err := newStream(streamConfig{rate: 48000, channels: 2, period: 64, fill: 128})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		result <- s.Wait() // the ring is full and nothing consumes it: blocks
	}()
	select {
	case err := <-result:
		t.Fatalf("Wait returned %v before Interrupt", err)
	case <-time.After(50 * time.Millisecond):
	}
	s.Interrupt()
	select {
	case err := <-result:
		if err != driver.ErrInterrupted {
			t.Fatalf("Wait returned %v, want ErrInterrupted", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Interrupt did not wake Wait")
	}
	if err := s.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	s.Interrupt() // after Close: a no-op, and must not touch the destroyed semaphore
}

func TestNewStreamRejectsBadGeometry(t *testing.T) {
	for name, cfg := range map[string]streamConfig{
		"zero rate":           {rate: 0, channels: 2, period: 64, fill: 128},
		"zero channels":       {rate: 48000, channels: 0, period: 64, fill: 128},
		"too many channels":   {rate: 48000, channels: maxChannels + 1, period: 64, fill: 128},
		"zero period":         {rate: 48000, channels: 2, period: 0, fill: 128},
		"fill below period":   {rate: 48000, channels: 2, period: 64, fill: 32},
		"fill not a period":   {rate: 48000, channels: 2, period: 64, fill: 100},
		"negative pipeline":   {rate: 48000, channels: 2, period: 64, fill: 128, pipeFrames: -1},
		"ring too large":      {rate: 48000, channels: 2, period: 1 << 19, fill: 3 << 19},
		"fill overflows ring": {rate: 48000, channels: 2, period: 1 << 20, fill: 1 << 21},
		"ring over 64 MiB":    {rate: 48000, channels: 64, period: 1 << 18, fill: 1 << 19},
	} {
		if s, err := newStream(cfg); err == nil {
			_ = s.Close()
			t.Errorf("%s: newStream accepted %+v", name, cfg)
		}
	}
}

// Calls out of order fail cleanly, and never touch the audio unit or the ring.
func TestLifecycleErrors(t *testing.T) {
	s := newTestStream(t, 2, 64, 128)

	if err := s.Wait(); err == nil {
		t.Error("Wait before Start succeeded")
	}
	if err := s.Commit(); err == nil {
		t.Error("Commit before Wait succeeded")
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Start(); err == nil {
		t.Error("second Start succeeded")
	}
	// Free one period: pretend the device consumed 64 frames.
	atomic.StoreUint64(&s.c.tail, 64)
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := s.Wait(); err == nil {
		t.Error("second Wait before Commit succeeded")
	}
	if err := s.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := s.Commit(); err == nil {
		t.Error("second Commit succeeded")
	}
	if in, out := s.Buffers(); in != nil || len(out) != 64*2*bytesPerSample {
		t.Errorf("Buffers() = %d, %d bytes", len(in), len(out))
	}
	if wake, commit := s.Deadlines(); wake != 0 || commit != 0 {
		t.Errorf("Deadlines() = %d, %d, want none", wake, commit)
	}
	if err := s.Recover(); err != nil {
		t.Errorf("Recover: %v", err)
	}
	if out, in := s.Clock(); out <= 0 || in != 0 {
		t.Errorf("Clock() = %d, %d", out, in)
	}
	if p := s.Params(); p.Period != 64 || p.Periods != 2 || p.OutChannels != 2 || p.OutFormat != "F32LE" || p.HasDeadline {
		t.Errorf("Params() = %+v", p)
	}
	if err := s.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if err := s.Start(); err == nil {
		t.Error("Start after Stop succeeded")
	}
	if err := s.Wait(); err == nil {
		t.Error("Wait after Stop succeeded")
	}
}

// The per-period calls on the stream thread must not allocate: Wait (including
// its blocking path, which crosses into libSystem), Buffers, Commit, Clock,
// Deadlines, Dropouts and Recover. A stand-in device thread consumes the ring so
// that Wait really blocks and wakes.
func TestStreamThreadCallsDoNotAllocate(t *testing.T) {
	s := newTestStream(t, 2, 64, 256)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runtime.LockOSThread()
		dst := make([]byte, 64*2*bytesPerSample)
		for {
			select {
			case <-stop:
				return
			default:
			}
			callRender(s.c, dst, 64, 2)
			time.Sleep(200 * time.Microsecond)
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	period := func() {
		if err := s.Wait(); err != nil {
			panic(err)
		}
		_, out := s.Buffers()
		out[0]++
		if err := s.Commit(); err != nil {
			panic(err)
		}
		s.Clock()
		s.Deadlines()
		s.Dropouts()
		s.Recover()
	}
	for i := 0; i < 200; i++ { // warm up: first-use costs belong outside the measurement
		period()
	}
	if allocs := testing.AllocsPerRun(1000, period); allocs != 0 {
		t.Errorf("a stream-thread period allocated %.2f objects", allocs)
	}
}
