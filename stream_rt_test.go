package tymbal

import (
	"testing"

	"m31labs.dev/tymbal/internal/driver"
	"m31labs.dev/tymbal/internal/format"
)

type fixedPeriodDriver struct {
	stream *Stream
	waits  int
	out    [256 * 4]byte
}

func (*fixedPeriodDriver) Params() driver.Params { return driver.Params{} }
func (*fixedPeriodDriver) Start() error          { return nil }
func (d *fixedPeriodDriver) Wait() error {
	d.waits++
	if d.waits > 32 {
		d.stream.stopRequested.Store(true)
	}
	return nil
}
func (*fixedPeriodDriver) Interrupt()                  {}
func (d *fixedPeriodDriver) Buffers() ([]byte, []byte) { return nil, d.out[:] }
func (*fixedPeriodDriver) Commit() error               { return nil }
func (*fixedPeriodDriver) Clock() (int64, int64)       { return 0, 0 }
func (*fixedPeriodDriver) Deadlines() (int64, int64)   { return 0, 0 }
func (*fixedPeriodDriver) Dropouts() uint64            { return 0 }
func (*fixedPeriodDriver) Recover() error              { return nil }
func (*fixedPeriodDriver) Stop() error                 { return nil }
func (*fixedPeriodDriver) Close() error                { return nil }

func TestCoreLoopNoAlloc(t *testing.T) {
	var samples [256]float32
	driver := &fixedPeriodDriver{}
	stream := &Stream{
		device: driver,
		cb: func(_ Time, _, out [][]float32) {
			for i := range out[0] {
				out[0][i] = 0.25
			}
		},
		actual: Actual{SampleRate: 48_000, Period: 256, OutChannels: 1, OutFormat: string(format.F32LE)},
		output: [][]float32{samples[:]},
	}
	driver.stream = stream
	allocs := testing.AllocsPerRun(100, func() {
		driver.waits = 0
		stream.stopRequested.Store(false)
		stream.loop()
	})
	if allocs != 0 {
		t.Fatalf("core loop allocated %.2f times per 32 callbacks", allocs)
	}
	if driver.waits != 33 {
		t.Fatalf("driver waits = %d, want 33", driver.waits)
	}
}
