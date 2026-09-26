package tymbaltest

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"m31labs.dev/tymbal"
)

func TestReportWakeIntervalTimingAndJSON(t *testing.T) {
	var stats tymbal.Stats
	for _, d := range []time.Duration{500 * time.Nanosecond, 100 * time.Microsecond, 3210123 * time.Nanosecond} {
		stats.WakeInterval.Record(d)
	}
	stats.WakeIntervalMax = 3210123 * time.Nanosecond
	stats.WakeLate.Record(20 * time.Microsecond)
	stats.WakeLateMax = 20125 * time.Nanosecond
	stats.CallbackTime.Record(9 * time.Microsecond)
	stats.CallbackMax = 9125 * time.Nanosecond
	wantTiming := TimingReport{P50: 128, P99: 4096, P999: 4096, Max: 3210.123}
	var wantBuckets [32]uint64
	wantBuckets[0], wantBuckets[7], wantBuckets[12] = 1, 1, 1
	for _, passed := range []bool{false, true} {
		r := Report{Host: "alsa", Passed: passed, DeadlineAvailable: false}
		r.FillTiming(stats)
		if r.WakeIntervalUS != wantTiming || r.WakeIntervalBuckets != wantBuckets {
			t.Fatalf("wake interval timing/buckets = %+v/%v", r.WakeIntervalUS, r.WakeIntervalBuckets)
		}
		if r.WakeLateUS != (TimingReport{32, 32, 32, 20.125}) || r.CallbackUS != (TimingReport{16, 16, 16, 9.125}) {
			t.Fatalf("existing timing changed: late=%+v callback=%+v", r.WakeLateUS, r.CallbackUS)
		}
		if r.Passed != passed || r.DeadlineAvailable {
			t.Fatal("FillTiming changed pass/deadline semantics")
		}
		var encoded bytes.Buffer
		if err := WriteReport(&encoded, r); err != nil {
			t.Fatal(err)
		}
		// Check the portable wire names and the full raw bucket array.
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(encoded.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		var timing TimingReport
		if err := json.Unmarshal(wire["wake_interval_us"], &timing); err != nil || timing != wantTiming {
			t.Fatalf("serialized wake_interval_us = %+v, error = %v", timing, err)
		}
		var buckets []uint64
		if err := json.Unmarshal(wire["wake_interval_buckets"], &buckets); err != nil || len(buckets) != 32 {
			t.Fatalf("serialized wake_interval_buckets = %v, error = %v", buckets, err)
		}
		for i, n := range wantBuckets {
			if buckets[i] != n {
				t.Fatalf("serialized bucket %d = %d, want %d", i, buckets[i], n)
			}
		}
		decoded, err := ReadReport(&encoded)
		if err != nil || decoded.WakeIntervalUS != wantTiming || decoded.WakeIntervalBuckets != wantBuckets || decoded.Passed != passed || decoded.DeadlineAvailable {
			t.Fatalf("report round trip = %+v, error = %v", decoded, err)
		}
	}
}

func TestReadReportWithoutWakeInterval(t *testing.T) {
	// Older reports omit both additive fields; existing failure evidence survives.
	r, err := ReadReport(strings.NewReader(`{"host":"alsa","rate":48000,"period":256,"periods":2,"duration_s":3600,"deadline_available":false,"dropouts_reported":91,"breaks_detected":123,"callback_us":{"max":26881},"passed":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.WakeIntervalUS != (TimingReport{}) || r.WakeIntervalBuckets != ([32]uint64{}) {
		t.Fatal("missing wake interval fields must decode to zero values")
	}
	if r.Host != "alsa" || r.Rate != 48000 || r.Period != 256 || r.Periods != 2 || r.DurationSeconds != 3600 || r.DropoutsReported != 91 || r.BreaksDetected != 123 || r.CallbackUS.Max != 26881 || r.DeadlineAvailable || r.Passed {
		t.Fatalf("old report evidence changed: %+v", r)
	}
}
