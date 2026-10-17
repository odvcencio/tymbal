//go:build darwin

package tymbal_test

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"m31labs.dev/tymbal"
)

// TestNativePublicRender drives the Core Audio host through the public API and
// the stream core: Open negotiates a period, Start runs the core's locked
// stream thread, the callback renders into the ring ahead of Core Audio, and
// Stop and Close tear it down. It renders silence, so it makes no sound. The
// audible check is TestSmokeSine in internal/coreaudio.
//
// A machine with no output device skips it.
func TestNativePublicRender(t *testing.T) {
	var host tymbal.Host
	for _, candidate := range tymbal.Hosts() {
		if candidate.Name() == "coreaudio" {
			host = candidate
		}
	}
	if host.Name() == "" {
		t.Skip("this build has no coreaudio host")
	}
	devices, err := host.Devices()
	if err != nil && strings.Contains(err.Error(), "no audio device") { // coreaudio's errNoDevice, which the public API does not export
		t.Skipf("no audio server on this machine: %v", err)
	}
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	hasOutput := false
	for _, d := range devices {
		hasOutput = hasOutput || d.Outputs > 0
	}
	if !hasOutput {
		t.Skip("no audio output device on this machine")
	}
	output, err := host.Default(tymbal.Output)
	if err != nil {
		t.Fatalf("Default output: %v", err)
	}
	if len(output.SampleRates) == 0 || output.Outputs <= 0 || output.Host != "coreaudio" {
		t.Fatalf("default output is not usable: %+v", output)
	}

	const requestedPeriod = 256
	var callbacks atomic.Uint64
	var invalid atomic.Bool
	var previousFrame uint64
	actualPeriod := requestedPeriod
	stream, err := tymbal.Open(host, tymbal.Config{
		Output: &output, OutChannels: output.Outputs,
		SampleRate: output.SampleRates[0], Period: requestedPeriod, Periods: 2,
	}, func(tm tymbal.Time, in, out [][]float32) {
		if len(in) != 0 || len(out) != output.Outputs {
			invalid.Store(true)
		}
		if callbacks.Load() == 0 {
			if tm.Frame != 0 {
				invalid.Store(true)
			}
		} else if tm.Frame != previousFrame+uint64(actualPeriod) {
			invalid.Store(true)
		}
		previousFrame = tm.Frame
		for _, channel := range out {
			if len(channel) != actualPeriod {
				invalid.Store(true)
			}
			clear(channel)
		}
		callbacks.Add(1)
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer stream.Close()
	actual := stream.Actual()
	actualPeriod = actual.Period
	if actual.SampleRate != output.SampleRates[0] || actual.OutChannels != output.Outputs || actual.InChannels != 0 ||
		actual.Period <= 0 || actual.Periods < 2 || actual.OutFormat != "F32LE" || actual.LatencyOut <= 0 {
		t.Fatalf("invalid render parameters: %+v", actual)
	}
	if err := stream.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	for i := 0; i < 2; i++ {
		if err := stream.Stop(); err != nil {
			t.Fatalf("Stop %d: %v", i, err)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := stream.Close(); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}
	var stats tymbal.Stats
	stream.Stats(&stats)
	// Half a second at the granted period is dozens of callbacks even if the
	// scheduler is unkind; 20 only rules out a stream that never ran.
	if callbacks.Load() < 20 || stats.Callbacks != callbacks.Load() || invalid.Load() {
		t.Fatalf("incomplete render callbacks: observed=%d reported=%d invalid=%t", callbacks.Load(), stats.Callbacks, invalid.Load())
	}
	t.Logf("coreaudio render: device=%q rate=%d period=%d periods=%d channels=%d format=%s latency_out=%s callbacks=%d dropouts=%d priority=%s",
		output.Name, actual.SampleRate, actual.Period, actual.Periods, actual.OutChannels, actual.OutFormat, actual.LatencyOut,
		stats.Callbacks, stats.Dropouts, stream.Actual().Priority) // not actual: the stream thread records its priority once Start has run
}
