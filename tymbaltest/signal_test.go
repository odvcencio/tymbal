package tymbaltest

import (
	"math"
	"testing"

	"m31labs.dev/tymbal"
)

func TestMeasureLatencyFrames(t *testing.T) {
	probe := MLS(0.5)
	const probeStart, delay = 777, 451
	capture := make([]float32, probeStart+len(probe)+delay+512)
	copy(capture[probeStart+delay:], probe)
	got, err := MeasureLatencyFrames(capture, probe, probeStart, delay+64)
	if err != nil {
		t.Fatal(err)
	}
	if got != delay {
		t.Fatalf("latency = %d frames, want %d", got, delay)
	}
}

func TestDetectContinuity(t *testing.T) {
	const rate = 48000
	gen := NewSine(rate, testToneHz, math.Pow(10, testToneDB/20))
	const frames = rate
	samples := make([]float32, frames)
	out := [][]float32{samples}
	gen.Render(tymbal.Time{}, nil, out)
	if breaks := DetectContinuity(samples, rate); len(breaks) != 0 {
		t.Fatalf("clean sine produced breaks: %+v", breaks)
	}
	for i := 10_000; i < 10_128; i++ {
		samples[i] = 0
	}
	breaks := DetectContinuity(samples, rate)
	if len(breaks) != 1 {
		t.Fatalf("one missing period produced %d breaks: %+v", len(breaks), breaks)
	}
	if d := int(breaks[0].Frame) - 10_000; d < -continuityWindow || d > continuityWindow {
		t.Fatalf("break at frame %d, want near 10000", breaks[0].Frame)
	}
}

func TestSineCallbackNoAlloc(t *testing.T) {
	gen := NewSine(48000, 997, math.Pow(10, testToneDB/20))
	NoAlloc(t, gen.Callback, 1, 2, 128)
}

func TestMLSSamplesUseBothPolarities(t *testing.T) {
	seq := MLS(0.5)
	if len(seq) != (1<<15)-1 {
		t.Fatalf("sequence length = %d", len(seq))
	}
	positive, negative := 0, 0
	for _, sample := range seq {
		switch sample {
		case 0.5:
			positive++
		case -0.5:
			negative++
		default:
			t.Fatalf("unexpected MLS sample %g", sample)
		}
	}
	if positive == 0 || negative == 0 {
		t.Fatalf("MLS polarity counts = +%d/-%d", positive, negative)
	}
}
