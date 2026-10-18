//go:build darwin && (amd64 || arm64)

package coreaudio

import (
	"encoding/binary"
	"errors"
	"math"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
	"m31labs.dev/tymbal/internal/format"
)

type inputTestTimestamp struct {
	sample   float64
	host     uint64
	pad      [40]byte
	flags    uint32
	reserved uint32
}

func callCapture(c *inputCtrl, frames int, ts *inputTestTimestamp) int32 {
	r, _, _ := syscall_syscall6(captureABI0, uintptr(unsafe.Pointer(c)), 0, uintptr(unsafe.Pointer(ts)), 1, uintptr(frames), 0)
	runtime.KeepAlive(ts)
	return int32(r)
}

func testCapture(t *testing.T) *captureStream {
	t.Helper()
	s, err := newCaptureStream(48000, 2, 64, 96)
	if err != nil {
		t.Fatal(err)
	}
	s.c.sigmask = 0
	s.c.render = 0 // injected native staging, never an audio device
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func captureStage(s *captureStream) []byte {
	l := (*nativeInputList)(unsafe.Pointer(s.c.list))
	return unsafe.Slice((*byte)(unsafe.Pointer(l.data)), int(s.c.maxFrames)*s.channels*4)
}

func TestNativeInputPacketWrapOverflowAndClock(t *testing.T) {
	s := testCapture(t)
	c := s.c
	capacity := c.mask + 1
	head := capacity - 17
	atomic.StoreUint64(&c.head, head)
	atomic.StoreUint64(&c.tail, head)
	stage := captureStage(s)
	for i := 0; i < 128*2; i++ {
		binary.LittleEndian.PutUint32(stage[i*4:], uint32(i+1))
	}
	ts := inputTestTimestamp{sample: 12345, host: s.hostOrigin + uint64(2e9)*uint64(s.denom)/uint64(s.numer), flags: 3}
	if status := callCapture(c, 128, &ts); status != 0 {
		t.Fatal(status)
	}
	if got := atomic.LoadUint64(&c.head); got != head+128 {
		t.Fatalf("head%d", got)
	}
	for i := 0; i < 128*2; i++ {
		off := ((head+uint64(i/2))&c.mask)*8 + uint64(i%2*4)
		if v := binary.LittleEndian.Uint32(s.data[off:]); v != uint32(i+1) {
			t.Fatalf("wrapped input%d=%d", i, v)
		}
	}
	clock := s.clock(float64(head + 32))
	if clock.InputPosition != 12377 || clock.InputFrequency != 48000 || math.Abs(float64(clock.InputQPCNano-s.rtOrigin-int64(2*time.Second)-int64(32*time.Second/48000))) > 4 {
		t.Fatalf("clock%+v", clock)
	}
	atomic.StoreUint64(&c.tail, head+128-capacity+4)
	if status := callCapture(c, 8, &ts); status != 0 {
		t.Fatal(status)
	}
	if atomic.LoadUint64(&c.head) != head+128 || atomic.LoadUint64(&c.under) != 1 || atomic.LoadUint64(&c.underFrames) != 8 {
		t.Fatal("overflow overwrote unread input or was not counted")
	}
	callCapture(c, int(c.maxFrames)+1, &ts)
	if int32(atomic.LoadUint64(&c.status)) != -10874 {
		t.Fatal("oversized native callback was not rejected")
	}
}

func TestNativeInputInterruptAndCloseWithoutDevice(t *testing.T) {
	s := testCapture(t)
	if err := s.start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.wait(64) }()
	s.interrupt()
	select {
	case err := <-done:
		if !errors.Is(err, driver.ErrInterrupted) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("input wait did not interrupt")
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeInputDuplexStagingAndGapRecovery(t *testing.T) {
	input := testCapture(t)
	output := newTestStream(t, 2, 64, 128)
	stage := captureStage(input)
	for f := 0; f < 512; f++ {
		for ch := 0; ch < 2; ch++ {
			binary.LittleEndian.PutUint32(stage[(f*2+ch)*4:], math.Float32bits(float32(f)/512))
		}
	}
	ts := inputTestTimestamp{sample: 1000, host: input.hostOrigin, flags: 3}
	callCapture(input.c, 512, &ts)
	p := output.Params()
	p.InChannels = 2
	p.InFormat = format.F32LE
	d := &duplex{input: input, output: output, params: p, in: make([]byte, 64*2*4), maxQueue: 512, reader: newInputReader(input.data, 2, 64, 48000, 48000, 256)}
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	callRender(output.c, make([]byte, 64*2*4), 64, 2)
	if err := d.Wait(); err != nil {
		t.Fatal(err)
	}
	in, out := d.Buffers()
	if len(out) != 512 || len(in) != 512 || math.Float32frombits(binary.LittleEndian.Uint32(in)) != 0.5 {
		t.Fatal("duplex did not retain raw equal-rate input")
	}
	if c := d.ClockSample(); c.InputPosition != 1256 || c.InputFrequency != 48000 {
		t.Fatalf("duplex source clock%+v", c)
	}
	if err := d.Wait(); !errors.Is(err, errWaitCommit) {
		t.Fatal("double Wait accepted")
	}
	if err := d.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := d.Commit(); !errors.Is(err, errState) {
		t.Fatal("double Commit accepted")
	}
	// A stalled host must not leave seconds of delayed input after it recovers.
	ts.sample = 1512
	callCapture(input.c, 4096, &ts)
	callRender(output.c, make([]byte, 64*2*4), 64, 2)
	if err := d.Wait(); !errors.Is(err, driver.ErrXrun) {
		t.Fatalf("delayed queue was not reported: %v", err)
	}
	if err := d.Recover(); err != nil {
		t.Fatal(err)
	}
	if err := d.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := d.Commit(); err != nil {
		t.Fatal(err)
	}
	if d.Dropouts() != 1 {
		t.Fatalf("queue loss not counted: %d", d.Dropouts())
	}
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}
