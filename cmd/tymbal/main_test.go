package main

import (
	"os"
	"strings"
	"testing"

	"m31labs.dev/tymbal/tymbaltest"
)

func TestSoakRequiresNativeEndpointsBeforeRunning(t *testing.T) {
	for _, args := range [][]string{nil, {"-host", "fake", "-dur", "1ms"}} {
		if err := runLoopback(args, true); err == nil || !strings.Contains(err.Error(), "explicit native") {
			t.Fatalf("soak %v: got %v, want native endpoint requirement", args, err)
		}
	}
}

func TestNativeLoopbackRejectsSimulationFlags(t *testing.T) {
	for _, option := range []string{"-dropout", "-delay-periods=2", "-dropout-at=1"} {
		if err := loopback([]string{"-host", "alsa", option}); err == nil || !strings.Contains(err.Error(), "only supported with -host fake") {
			t.Fatalf("native %s: got %v, want simulation flag rejection", option, err)
		}
	}
}

func TestLoopbackEndpointSelectionRejectsMissingAndUnknownIDs(t *testing.T) {
	host, _ := tymbaltest.NewFakeHost(tymbaltest.FakeConfig{Manual: true, Loopback: true})
	for _, ids := range [][2]string{{"", ""}, {"absent-out", "absent-in"}} {
		if _, _, err := loopbackEndpoints(host, ids[0], ids[1]); err == nil {
			t.Fatalf("accepted endpoints %v", ids)
		}
	}
}

func TestCLILoopbackWritesMeasuredReport(t *testing.T) {
	path := t.TempDir() + "/result.json"
	if err := loopback([]string{"-dur", "20ms", "-json", path}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	report, err := tymbaltest.ReadReport(f)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.Host != "fake" || report.Callbacks == 0 || report.DurationSeconds < .02 || report.LatencyMeasuredFrames != 2*report.Period {
		t.Fatalf("unexpected CLI conformance report: %+v", report)
	}
}

func TestLoopbackUnavailableHostReturnsError(t *testing.T) {
	err := loopback([]string{"-host", "tymbal-nonexistent-host"})
	if err == nil || !strings.Contains(err.Error(), "is unavailable") {
		t.Fatalf("unavailable host returned %v", err)
	}
}
