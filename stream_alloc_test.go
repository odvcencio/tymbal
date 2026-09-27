package tymbal

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"m31labs.dev/tymbal/internal/driver"
	"m31labs.dev/tymbal/internal/format"
	"m31labs.dev/tymbal/internal/rt"
)

// allocFreeDriver is a backend whose stream methods never allocate. Wait
// serves a fixed number of periods, then sleeps until Interrupt. It waits in a
// system call, as a real backend does, rather than on a channel, whose runtime
// bookkeeping can allocate.
type allocFreeDriver struct{ stream *allocFreeStream }

func (*allocFreeDriver) Name() string { return "allocfree" }
func (*allocFreeDriver) Devices() ([]driver.Info, error) {
	return []driver.Info{{ID: "allocfree", Name: "allocfree", Outputs: 1}}, nil
}
func (d *allocFreeDriver) Default(uint8) (driver.Info, error) {
	devices, _ := d.Devices()
	return devices[0], nil
}
func (*allocFreeDriver) Watch(func(driver.Event)) func()              { return func() {} }
func (d *allocFreeDriver) Open(driver.Request) (driver.Stream, error) { return d.stream, nil }

type allocFreeStream struct {
	periods     int
	served      atomic.Bool // set after the last served period
	interrupted atomic.Bool
	out         [64 * 4]byte
}

func newAllocFreeStream(periods int) *allocFreeStream {
	return &allocFreeStream{periods: periods}
}

func (*allocFreeStream) Params() driver.Params {
	return driver.Params{SampleRate: 48_000, Period: 64, Periods: 2, OutChannels: 1, OutFormat: format.F32LE}
}
func (*allocFreeStream) Start() error { return nil }
func (s *allocFreeStream) Wait() error {
	if s.periods > 0 {
		s.periods--
		return nil
	}
	s.served.Store(true)
	for !s.interrupted.Load() {
		rt.SleepThread(time.Millisecond)
	}
	return driver.ErrInterrupted
}
func (s *allocFreeStream) Interrupt()                { s.interrupted.Store(true) }
func (s *allocFreeStream) Buffers() ([]byte, []byte) { return nil, s.out[:] }
func (*allocFreeStream) Commit() error               { return nil }
func (*allocFreeStream) Clock() (int64, int64)       { return 0, 0 }
func (*allocFreeStream) Deadlines() (int64, int64)   { return 0, 0 }
func (*allocFreeStream) Dropouts() uint64            { return 0 }
func (*allocFreeStream) Recover() error              { return nil }
func (*allocFreeStream) Stop() error                 { return nil }
func (*allocFreeStream) Close() error                { return nil }

var streamAllocSink *[4]uint64

// warmTestThreads makes the runtime create spare threads now. Each test
// stream's locked thread exits with it, and a replacement thread created
// inside the counted window would allocate there.
func warmTestThreads() {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt.SleepThread(10 * time.Millisecond)
		}()
	}
	wg.Wait()
}

func runAllocFreeStream(t *testing.T, periods int, cb Callback) Stats {
	t.Helper()
	warmTestThreads()
	backend := newAllocFreeStream(periods)
	host := Host{d: &allocFreeDriver{stream: backend}}
	device, err := host.Default(Output)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(host, Config{Output: &device, OutChannels: 1, SampleRate: 48_000, Period: 64, Periods: 2}, cb)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	for !backend.served.Load() {
		rt.SleepThread(time.Millisecond)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	var stats Stats
	s.Stats(&stats)
	if stats.Callbacks != uint64(periods) {
		t.Fatalf("callbacks = %d, want %d", stats.Callbacks, periods)
	}
	return stats
}

// A run shorter than the watchdog interval must still report the loop's
// allocations once Stop returns.
func TestAllocsSinceRunCountsLoopAllocationsInShortRun(t *testing.T) {
	const periods = 32
	stats := runAllocFreeStream(t, periods, func(Time, [][]float32, [][]float32) {
		streamAllocSink = new([4]uint64)
	})
	if stats.AllocsSinceRun < periods {
		t.Fatalf("AllocsSinceRun = %d after %d allocating callbacks, want at least %d", stats.AllocsSinceRun, periods, periods)
	}
}

// Startup allocation (thread priority, the watchdog itself) happens before the
// loop and must not be charged to an allocation-free loop.
func TestAllocsSinceRunIsZeroForAllocationFreeLoop(t *testing.T) {
	stats := runAllocFreeStream(t, 32, func(_ Time, _, out [][]float32) {
		for i := range out[0] {
			out[0][i] = 0.25
		}
	})
	if stats.AllocsSinceRun != 0 {
		t.Fatalf("AllocsSinceRun = %d for an allocation-free loop, want 0", stats.AllocsSinceRun)
	}
}
