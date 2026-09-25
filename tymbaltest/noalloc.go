package tymbaltest

import (
	"testing"

	"m31labs.dev/tymbal"
)

const noAllocIterations = 10_000

// NoAlloc calls cb ten thousand times per measurement and fails if one call
// allocates. Buffer construction and warm-up happen outside the measured run.
func NoAlloc(t testing.TB, cb tymbal.Callback, channelsIn, channelsOut, period int) {
	t.Helper()
	if cb == nil {
		t.Fatal("NoAlloc: nil callback")
	}
	if channelsIn < 0 || channelsOut < 0 || channelsIn+channelsOut == 0 || period <= 0 {
		t.Fatalf("NoAlloc: invalid dimensions: input=%d output=%d period=%d", channelsIn, channelsOut, period)
	}
	in := make([][]float32, channelsIn)
	out := make([][]float32, channelsOut)
	for ch := range in {
		in[ch] = make([]float32, period)
	}
	for ch := range out {
		out[ch] = make([]float32, period)
	}
	var tick uint64
	invoke := func() {
		for i := 0; i < noAllocIterations; i++ {
			cb(tymbal.Time{Frame: tick}, in, out)
			tick += uint64(period)
		}
	}
	allocs := testing.AllocsPerRun(1, invoke)
	if allocs != 0 {
		t.Fatalf("callback allocated %.2f objects per %d calls", allocs, noAllocIterations)
	}
}
