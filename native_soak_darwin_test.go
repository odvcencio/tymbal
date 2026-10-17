//go:build darwin

package tymbal_test

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"m31labs.dev/tymbal"
)

// soakInt reads a positive integer from the environment, or returns def.
func soakInt(t *testing.T, key string, def int) int {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		t.Fatalf("%s=%q: want a positive integer", key, raw)
	}
	return v
}

// TestNativeRenderSoak is the Core Audio underrun gate for small periods. It
// runs for as long as it is told to, so it is opt in:
//
//	TYMBAL_NATIVE_DURATION=60s go test -run TestNativeRenderSoak -v .
//
// TYMBAL_NATIVE_PERIOD (default 128) and TYMBAL_NATIVE_PERIODS (default 2) set
// the geometry, as in the native loopback test. It renders silence through the
// public API, so the stream core's locked thread raises its own priority just
// as an application's would, and it fails on any underrun: a render callback
// that found the ring short and played silence in its place.
//
// Real-time priority keeps the stream thread's wakes on time, but cannot stop
// the occasional multi-millisecond stall on a busy machine. Two periods of ring
// give the thread about two periods to recover from one, and
// TYMBAL_NATIVE_PERIODS=3 gives it another.
//
// A machine with no output device skips it.
func TestNativeRenderSoak(t *testing.T) {
	raw := os.Getenv("TYMBAL_NATIVE_DURATION")
	if raw == "" {
		t.Skip("set TYMBAL_NATIVE_DURATION (for example 60s) to run the Core Audio render soak")
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		t.Fatalf("TYMBAL_NATIVE_DURATION=%q: want a positive duration", raw)
	}
	period := soakInt(t, "TYMBAL_NATIVE_PERIOD", 128)
	periods := soakInt(t, "TYMBAL_NATIVE_PERIODS", 2)

	var host tymbal.Host
	for _, candidate := range tymbal.Hosts() {
		if candidate.Name() == "coreaudio" {
			host = candidate
		}
	}
	if host.Name() == "" {
		t.Skip("this build has no coreaudio host")
	}
	output, err := host.Default(tymbal.Output)
	if err != nil && strings.Contains(err.Error(), "no audio device") { // coreaudio's errNoDevice, which the public API does not export
		t.Skipf("no audio output device: %v", err)
	}
	if err != nil {
		t.Fatalf("Default output: %v", err)
	}
	if len(output.SampleRates) == 0 || output.Outputs <= 0 {
		t.Skipf("default output %q has no usable format: %+v", output.Name, output)
	}

	stream, err := tymbal.Open(host, tymbal.Config{
		Output: &output, OutChannels: min(2, output.Outputs),
		SampleRate: output.SampleRates[0], Period: period, Periods: periods,
	}, func(tymbal.Time, [][]float32, [][]float32) {}) // the core hands the callback zeroed buffers: silence
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer stream.Close()
	if err := stream.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Read the parameters after Start: the stream thread records the priority
	// it was granted as it starts.
	actual := stream.Actual()
	t.Logf("soak %s: device=%q rate=%d period=%d periods=%d channels=%d latency_out=%s priority=%q",
		duration, output.Name, actual.SampleRate, actual.Period, actual.Periods, actual.OutChannels, actual.LatencyOut, actual.Priority)

	var gcBefore runtime.MemStats
	runtime.ReadMemStats(&gcBefore)
	start := time.Now()
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	var seen uint64
	for nextProgress := 10 * time.Second; ; {
		select {
		case <-deadline.C:
		case <-tick.C:
			if err := stream.Err(); err != nil {
				t.Fatalf("stream failed after %s: %v", time.Since(start).Round(time.Millisecond), err)
			}
			var now tymbal.Stats
			stream.Stats(&now)
			elapsed := time.Since(start)
			if now.Dropouts != seen {
				// When an underrun lands tells a start-up transient from noise.
				t.Logf("underrun at %s: callback %d, dropouts %d -> %d, longest wake interval so far %s",
					elapsed.Round(time.Millisecond), now.Callbacks, seen, now.Dropouts, now.WakeIntervalMax)
				seen = now.Dropouts
			}
			if elapsed >= nextProgress {
				t.Logf("%3ds: callbacks=%d dropouts=%d", int(nextProgress/time.Second), now.Callbacks, now.Dropouts)
				nextProgress += 10 * time.Second
			}
			continue
		}
		break
	}
	if err := stream.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	var stats tymbal.Stats
	stream.Stats(&stats)
	// A stop-the-world GC pause stalls the real-time thread whatever its
	// priority, so say whether the collector ran.
	var gcAfter runtime.MemStats
	runtime.ReadMemStats(&gcAfter)
	t.Logf("result: callbacks=%d dropouts=%d wake_interval p50=%s p99=%s p99.9=%s max=%s callback_max=%s gc_cycles=%d",
		stats.Callbacks, stats.Dropouts,
		stats.WakeInterval.Percentile(0.5), stats.WakeInterval.Percentile(0.99), stats.WakeInterval.Percentile(0.999),
		stats.WakeIntervalMax, stats.CallbackMax, gcAfter.NumGC-gcBefore.NumGC)

	// A stream that never ran has no underruns to count.
	periodLength := time.Duration(int64(actual.Period) * int64(time.Second) / int64(actual.SampleRate))
	if want := uint64(duration / periodLength); stats.Callbacks < want*9/10 {
		t.Errorf("only %d callbacks in %s of %s periods, want about %d", stats.Callbacks, duration, periodLength, want)
	}
	if stats.Dropouts != 0 {
		t.Errorf("%d underruns in %s at period %d", stats.Dropouts, duration, actual.Period)
	}
	if actual.Priority != "time-constraint" {
		t.Errorf("stream thread priority = %q, want time-constraint", actual.Priority)
	}
}
