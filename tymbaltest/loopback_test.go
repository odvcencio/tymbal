package tymbaltest

import (
	"bytes"
	"testing"
	"time"

	"m31labs.dev/tymbal"
)

func TestFakeLoopbackReportsExactLatencyAndNoBreaks(t *testing.T) {
	const period, delay = 128, 3
	report, err := FakeLoopback(tymbal.Config{SampleRate: 48_000, Period: period, Periods: 2}, LoopbackOptions{
		Duration: time.Second, DelayPeriods: delay,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantLatency := period * delay
	if report.LatencyMeasuredFrames != wantLatency || report.LatencyReportedFrames != wantLatency {
		t.Fatalf("measured/reported latency = %d/%d, want %d frames", report.LatencyMeasuredFrames, report.LatencyReportedFrames, wantLatency)
	}
	if report.BreaksDetected != 0 || report.DropoutsReported != 0 || !report.Passed {
		t.Fatalf("clean fake loopback did not pass: %+v", report)
	}
}

func TestFakeLoopbackDeterministicFaultMatrix(t *testing.T) {
	const seed int64 = 0x5eed
	state := uint64(seed)
	next := func() uint64 {
		state = state*6364136223846793005 + 1442695040888963407
		return state
	}
	periods := []int{128, 256}
	for caseIndex := 0; caseIndex < 8; caseIndex++ {
		period := periods[int(next()%uint64(len(periods)))]
		delay := 1 + int(next()%4)
		channels := 1 + int(next()%2)
		faultAt := delay + 4 + int(next()%45)
		report, err := FakeLoopback(tymbal.Config{
			SampleRate: 48_000, Period: period, Periods: 2,
			OutChannels: channels, InChannels: channels,
		}, LoopbackOptions{
			Duration: 500 * time.Millisecond, DelayPeriods: delay,
			InjectDropout: true, InjectDropoutAtPeriod: faultAt, Seed: seed,
		})
		if err != nil {
			t.Fatalf("seed=%d case=%d period=%d delay=%d channels=%d fault=%d: %v", seed, caseIndex, period, delay, channels, faultAt, err)
		}
		if !report.Passed || report.LatencyMeasuredFrames != period*delay || report.BreaksDetected != 1 || report.DropoutsReported != 1 {
			t.Fatalf("seed=%d case=%d period=%d delay=%d channels=%d fault=%d: report=%+v", seed, caseIndex, period, delay, channels, faultAt, report)
		}
	}
}

func TestFakeLoopbackFindsInjectedDropoutAndRoundTripsReport(t *testing.T) {
	report, err := FakeLoopback(tymbal.Config{SampleRate: 48_000, Period: 128, Periods: 2}, LoopbackOptions{
		Duration: time.Second, DelayPeriods: 2,
		InjectDropout: true, InjectDropoutAtPeriod: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.BreaksDetected != 1 || report.DropoutsReported != 1 || !report.Passed {
		t.Fatalf("injected dropout was not detected exactly once: %+v", report)
	}
	var encoded bytes.Buffer
	if err := WriteReport(&encoded, report); err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadReport(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.LatencyMeasuredFrames != report.LatencyMeasuredFrames || decoded.BreaksDetected != 1 || !decoded.Passed {
		t.Fatalf("JSON round trip changed report: %+v", decoded)
	}
}
