package tymbal

import (
	"errors"
	"sync"
	"testing"
	"time"

	"m31labs.dev/tymbal/internal/rt"
)

func openManualTestStream(t *testing.T, controlCfg FakeConfig, cb Callback) (*Stream, *FakeControl) {
	t.Helper()
	host, control := NewFakeHost(controlCfg)
	devices, err := host.Devices()
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) == 0 {
		t.Fatal("fake host returned no devices")
	}
	device := devices[0]
	input, output := device, device
	cfg := Config{
		Output: &output, Input: &input, OutChannels: 1, InChannels: 1,
		SampleRate: 48000, Period: 4, Periods: 2,
	}
	stream, err := Open(host, cfg, cb)
	if err != nil {
		t.Fatal(err)
	}
	return stream, control
}

func TestFakeManualLoopbackAndDropoutLifecycle(t *testing.T) {
	type observation struct {
		time Time
		in   []float32
	}
	observed := make([]observation, 0, 3)
	cb := func(tim Time, in, out [][]float32) {
		copyIn := append([]float32(nil), in[0]...)
		observed = append(observed, observation{time: tim, in: copyIn})
		for i := range out[0] {
			out[0][i] = float32(tim.Frame+uint64(i)) + 0.25
		}
	}
	s, control := openManualTestStream(t, FakeConfig{Manual: true, Loopback: true, Delay: 1}, cb)
	control.InjectDropout(1)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := control.Advance(3); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	var stats Stats
	s.Stats(&stats)
	if stats.Callbacks != 3 || stats.Dropouts != 1 {
		t.Fatalf("callbacks/dropouts = %d/%d, want 3/1", stats.Callbacks, stats.Dropouts)
	}
	if len(observed) != 3 {
		t.Fatalf("observed %d callbacks, want 3", len(observed))
	}
	if observed[0].time.Frame != 0 || observed[1].time.Frame != 4 || observed[2].time.Frame != 8 {
		t.Fatalf("callback frames = %d, %d, %d", observed[0].time.Frame, observed[1].time.Frame, observed[2].time.Frame)
	}
	if !observed[1].time.Discontinuity || observed[1].time.Dropouts != 1 {
		t.Fatalf("callback after dropout has time %+v", observed[1].time)
	}
	for _, sample := range observed[1].in {
		if sample != 0 {
			t.Fatalf("dropped capture period contained sample %g", sample)
		}
	}
	for i, sample := range observed[2].in {
		want := float32(i+4) + 0.25
		if sample != want {
			t.Fatalf("loopback sample %d = %g, want %g", i, sample, want)
		}
	}
	actual := s.Actual()
	if got := int((actual.LatencyIn*time.Duration(actual.SampleRate) + time.Second/2) / time.Second); got != actual.Period {
		t.Fatalf("reported loopback latency = %d frames, want %d", got, actual.Period)
	}
	recorded := control.Recorded()
	if len(recorded) != 12 || recorded[4] != 4.25 {
		t.Fatalf("recorded output has %d samples or wrong period data: %v", len(recorded), recorded)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if err := s.Start(); !errors.Is(err, ErrState) {
		t.Fatalf("second Start error = %v, want ErrState", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := s.Stop(); !errors.Is(err, ErrState) {
		t.Fatalf("Stop after Close error = %v, want ErrState", err)
	}
}

func TestCloseInterruptsManualWait(t *testing.T) {
	s, _ := openManualTestStream(t, FakeConfig{Manual: true, Loopback: true}, func(Time, [][]float32, [][]float32) {})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt a blocked manual wait")
	}
}

func TestCallbackPanicCommitsSilenceAndReportsFailure(t *testing.T) {
	s, control := openManualTestStream(t, FakeConfig{Manual: true, Loopback: true}, func(_ Time, _, out [][]float32) {
		out[0][0] = 1
		panic("callback failure")
	})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := control.Advance(1); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(s.Err(), ErrCallback) {
		t.Fatalf("stream error = %v, want ErrCallback", s.Err())
	}
	for i, sample := range control.Recorded() {
		if sample != 0 {
			t.Fatalf("panic period sample %d = %g, want silence", i, sample)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentStatsStopAndClose(t *testing.T) {
	s, control := openManualTestStream(t, FakeConfig{Manual: true, Loopback: true}, func(_ Time, _, out [][]float32) {
		for i := range out[0] {
			out[0][i] = 0.5
		}
	})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			var stats Stats
			s.Stats(&stats)
		}
	}()
	go func() {
		defer wg.Done()
		_ = control.Advance(2)
	}()
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWakeIntervalStatsAcrossRecovery(t *testing.T) {
	type observation struct {
		wakeUpper int64
		stats     Stats
	}
	observed := make(chan observation, 3)
	var s *Stream
	s, control := openManualTestStream(t, FakeConfig{Manual: true}, func(tim Time, _, _ [][]float32) {
		wakeUpper := rt.Now()
		// Callback work must be included in the next serviced-wake interval.
		if tim.Frame == 0 {
			time.Sleep(3 * time.Millisecond)
		}
		var stats Stats
		s.Stats(&stats)
		observed <- observation{wakeUpper, stats}
	})
	defer s.Close()
	control.InjectDropout(1)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	var previous observation
	var previousLower int64
	for i := 0; i < 3; i++ {
		wakeLower := rt.Now()
		if err := control.Advance(1); err != nil {
			t.Fatal(err)
		}
		current := <-observed
		var count uint64
		for _, n := range current.stats.WakeInterval.Buckets {
			count += n
		}
		if count != uint64(i) {
			t.Fatalf("period %d: interval count = %d, want %d", i, count, i)
		}
		if i == 0 {
			if current.stats.WakeIntervalMax != 0 {
				t.Fatal("first serviced wake must not record an interval")
			}
		} else {
			// Bracket real serviced wakes without replacing the core clock.
			// The raw maximum must be the previous exact maximum or an
			// observation inside these nanosecond bounds, not a bucket bound.
			lower := time.Duration(wakeLower - previous.wakeUpper)
			upper := time.Duration(current.wakeUpper - previousLower)
			got, prior := current.stats.WakeIntervalMax, previous.stats.WakeIntervalMax
			if got < max(prior, lower) || got > max(prior, upper) {
				t.Fatalf("period %d: max = %v, prior = %v, observed interval within [%v, %v]", i, got, prior, lower, upper)
			}
		}
		previous, previousLower = current, wakeLower
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	var stopped Stats
	s.Stats(&stopped)
	if stopped.WakeIntervalMax != previous.stats.WakeIntervalMax || stopped.WakeInterval.Buckets != previous.stats.WakeInterval.Buckets {
		t.Fatal("stopped wake interval snapshot changed")
	}
	if stopped.Dropouts != 1 || stopped.Late != 0 || stopped.WakeLateMax != 0 || s.Actual().HasDeadline {
		t.Fatalf("wake observations changed dropout/deadline semantics: %+v", stopped)
	}
}
