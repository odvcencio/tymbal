//go:build darwin && (amd64 || arm64)

package tymbal_test

import (
	"math"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"m31labs.dev/tymbal"
)

// Explicitly authorized three-second metadata-only check. The output remains
// silent, and no captured samples are logged, written or retained as a take.
func TestNativePublicDuplexCapture(t *testing.T) {
	if os.Getenv("TYMBAL_NATIVE_CAPTURE") != "1" {
		t.Skip("set TYMBAL_NATIVE_CAPTURE=1 only after microphone authorization is coordinated")
	}
	var host tymbal.Host
	for _, h := range tymbal.Hosts() {
		if h.Name() == "coreaudio" {
			host = h
		}
	}
	if host.Name() == "" {
		t.Fatal("CoreAudio host unavailable")
	}
	input, err := host.Default(tymbal.Input)
	if err != nil {
		t.Fatal(err)
	}
	output, err := host.Default(tymbal.Output)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.SampleRates) == 0 || input.Inputs < 1 || output.Outputs < 1 {
		t.Fatal("default duplex devices unavailable")
	}
	var callbacks, finiteSamples, nonzeroSamples atomic.Uint64
	var bad atomic.Bool
	var previousNano int64
	var sourceFrequency uint64
	s, err := tymbal.Open(host, tymbal.Config{Input: &input, Output: &output, InChannels: min(2, input.Inputs), OutChannels: min(2, output.Outputs), SampleRate: output.SampleRates[0], Period: 128, Periods: 2}, func(tm tymbal.Time, in, out [][]float32) {
		if len(in) < 1 || tm.InputNano <= previousNano || tm.InputFrequency == 0 {
			bad.Store(true)
		}
		previousNano = tm.InputNano
		sourceFrequency = tm.InputFrequency
		for _, ch := range in {
			for _, v := range ch {
				if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
					bad.Store(true)
				} else {
					finiteSamples.Add(1)
					if v != 0 {
						nonzeroSamples.Add(1)
					}
				}
			}
			clear(ch)
		}
		for _, ch := range out {
			clear(ch)
		}
		callbacks.Add(1)
	})
	if err != nil {
		t.Fatalf("duplex Open: %v", err)
	}
	defer s.Close()
	if err = s.Start(); err != nil {
		t.Fatalf("duplex Start: %v", err)
	}
	time.Sleep(3 * time.Second)
	if err = s.Stop(); err != nil {
		t.Fatal(err)
	}
	if err = s.Err(); err != nil {
		t.Fatal(err)
	}
	actual := s.Actual()
	var stats tymbal.Stats
	s.Stats(&stats)
	if callbacks.Load() < 20 || finiteSamples.Load() == 0 || bad.Load() {
		t.Fatalf("invalid capture metadata callbacks=%d samples=%d clock_or_format_error=%t", callbacks.Load(), finiteSamples.Load(), bad.Load())
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("coreaudio duplex: input=%q output=%q distinct_devices=%t rate=%d input_clock_hz=%d period=%d input_channels=%d output_channels=%d input_latency=%s output_latency=%s callbacks=%d finite_samples=%d nonzero_samples=%d dropouts=%d priority=%s callback_max=%s silent_output=true pcm_files=0",
		input.Name, output.Name, input.ID != output.ID, actual.SampleRate, sourceFrequency, actual.Period, actual.InChannels, actual.OutChannels, actual.LatencyIn, actual.LatencyOut, stats.Callbacks, finiteSamples.Load(), nonzeroSamples.Load(), stats.Dropouts, actual.Priority, stats.CallbackMax)
}
