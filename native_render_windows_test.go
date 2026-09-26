//go:build windows

package tymbal_test

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"m31labs.dev/tymbal"
)

func TestNativePublicRender(t *testing.T) {
	if os.Getenv("TYMBAL_REQUIRE_RENDER") != "1" {
		t.Skip("set TYMBAL_REQUIRE_RENDER=1 to require native Windows render")
	}
	testNativePublicRender(t, false, 480)
}

func TestNativePublicExclusiveRender(t *testing.T) {
	if os.Getenv("TYMBAL_REQUIRE_EXCLUSIVE_RENDER") != "1" {
		t.Skip("set TYMBAL_REQUIRE_EXCLUSIVE_RENDER=1 to require native exclusive render")
	}
	testNativePublicRender(t, true, 128)
}

func testNativePublicRender(t *testing.T, exclusive bool, requestedPeriod int) {
	t.Helper()
	var host tymbal.Host
	for _, candidate := range tymbal.Hosts() {
		if candidate.Name() == "wasapi" {
			host = candidate
			break
		}
	}
	output, err := host.Default(tymbal.Output)
	if err != nil {
		t.Fatalf("required WASAPI render endpoint: %v", err)
	}
	if len(output.SampleRates) == 0 || output.Outputs <= 0 {
		t.Fatal("required render endpoint has no nominal format")
	}
	if exclusive && !output.Exclusive {
		t.Fatal("required exclusive render endpoint does not advertise exact format support")
	}
	var callbacks atomic.Uint64
	var invalid atomic.Bool
	actualPeriod := requestedPeriod
	var previousFrame uint64
	stream, err := tymbal.Open(host, tymbal.Config{
		Output: &output, OutChannels: output.Outputs,
		SampleRate: output.SampleRates[0], Period: requestedPeriod, Periods: 2,
		Exclusive: exclusive,
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
		t.Fatalf("open required render stream: %v", err)
	}
	defer stream.Close()
	actual := stream.Actual()
	actualPeriod = actual.Period
	if actual.OutChannels != output.Outputs || actual.InChannels != 0 || actualPeriod <= 0 {
		t.Fatalf("invalid render parameters: %+v", actual)
	}
	if err := stream.Start(); err != nil {
		t.Fatalf("start required render stream: %v", err)
	}
	time.Sleep(time.Second)
	for i := 0; i < 2; i++ {
		if err := stream.Stop(); err != nil {
			t.Fatalf("stop %d: %v", i, err)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := stream.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
	var stats tymbal.Stats
	stream.Stats(&stats)
	if callbacks.Load() < 2 || stats.Callbacks != callbacks.Load() || invalid.Load() {
		t.Fatalf("incomplete render callbacks: observed=%d reported=%d invalid=%t", callbacks.Load(), stats.Callbacks, invalid.Load())
	}
	actual = stream.Actual()
	t.Logf("WASAPI render: exclusive=%t requested_period=%d rate=%d period=%d periods=%d channels=%d format=%s latency_out=%s callbacks=%d dropouts=%d priority=%s",
		exclusive, requestedPeriod, actual.SampleRate, actual.Period, actual.Periods, actual.OutChannels, actual.OutFormat, actual.LatencyOut,
		stats.Callbacks, stats.Dropouts, actual.Priority)
}
