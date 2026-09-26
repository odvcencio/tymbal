package tymbaltest

import (
	"bytes"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"m31labs.dev/tymbal"
)

func nativeTestConfig() tymbal.Config {
	return tymbal.Config{SampleRate: 48_000, Period: 256, Periods: 2, InChannels: 1, OutChannels: 1}
}

func nativeTestOptions() NativeOptions {
	return NativeOptions{Duration: 100 * time.Millisecond, MaxDelayPeriods: 4}
}

func nativeTestDevice(t *testing.T, host tymbal.Host) tymbal.Device {
	t.Helper()
	devices, err := host.Devices()
	if err != nil || len(devices) == 0 {
		t.Fatalf("enumeration: %v, devices=%v", err, devices)
	}
	return devices[0]
}

// Drive the real harness lifecycle through the deterministic manual clock.
// Advance waits for Commit, so the runner sees exactly the requested periods.
func runNativeFake(t *testing.T, loopback bool, dropout bool, loadCommand ...string) Report {
	t.Helper()
	host, control := NewFakeHost(FakeConfig{Manual: true, Loopback: loopback, Delay: 2})
	device := nativeTestDevice(t, host)
	cfg, opts := nativeTestConfig(), nativeTestOptions()
	if len(loadCommand) != 0 {
		opts.Load, opts.LoadCommand = []string{"cpu", "gc"}, loadCommand
	}
	m, err := newNativeMetrics(tymbal.Actual{SampleRate: cfg.SampleRate, Period: cfg.Period, InChannels: 1, OutChannels: 1}, opts)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		report Report
		err    error
	}
	resultC := make(chan result, 1)
	go func() {
		report, err := NativeLoopback(host, device, device, cfg, opts)
		resultC <- result{report, err}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		err = control.Advance(1)
		if err == nil {
			break
		}
		select {
		case r := <-resultC:
			t.Fatalf("harness failed before Start: %v", r.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("harness did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if dropout {
		control.InjectDropout(int(m.probeEnd)/cfg.Period + 10)
	}
	if err := control.Advance(int(m.end)/cfg.Period - 1); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-resultC:
		if loopback && r.err != nil {
			t.Fatal(r.err)
		}
		if !loopback && !errors.Is(r.err, errNoCorrelation) {
			t.Fatalf("unconnected capture error = %v, want no correlation", r.err)
		}
		stream, err := tymbal.Open(host, tymbal.Config{Output: &device, OutChannels: 1, SampleRate: cfg.SampleRate, Period: cfg.Period}, ToneCallback(cfg.SampleRate))
		if err != nil {
			t.Fatalf("harness did not release the endpoint: %v", err)
		}
		if err := stream.Close(); err != nil {
			t.Fatal(err)
		}
		return r.report
	case <-time.After(3 * time.Second):
		t.Fatal("harness did not stop")
	}
	return Report{}
}

func TestNativeLoopbackFakeOrchestrationAndReport(t *testing.T) {
	r := runNativeFake(t, true, false)
	cfg := nativeTestConfig()
	if !r.Passed || r.Host != "fake" || r.Input != "fake-default" || r.Output != "fake-default" {
		t.Fatalf("report identity/pass = %+v", r)
	}
	if r.LatencyMeasuredFrames != 2*cfg.Period || r.LatencyReportedFrames != 2*cfg.Period || r.BreaksDetected != 0 || r.DropoutsReported != 0 {
		t.Fatalf("latency/continuity = %+v", r)
	}
	if r.Rate != cfg.SampleRate || r.Period != cfg.Period || r.Periods != cfg.Periods || r.DeadlineAvailable || r.Priority == "" {
		t.Fatalf("actual parameters = %+v", r)
	}
	if math.Abs(r.DurationSeconds-4864.0/48000) > 1e-9 || r.Callbacks != 156 {
		t.Fatalf("complete period sample size: duration=%v callbacks=%d", r.DurationSeconds, r.Callbacks)
	}
	var encoded bytes.Buffer
	if err := WriteReport(&encoded, r); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded.Bytes(), []byte("samples")) {
		t.Fatal("report must not contain capture samples")
	}
	decoded, err := ReadReport(&encoded)
	if err != nil || decoded.LatencyMeasuredFrames != r.LatencyMeasuredFrames || decoded.Callbacks != r.Callbacks || !decoded.Passed {
		t.Fatalf("existing report round trip: %+v, %v", decoded, err)
	}
}

func TestNativeLoopbackFaultIsFailure(t *testing.T) {
	r := runNativeFake(t, true, true)
	if r.Passed || r.DropoutsReported != 1 || r.BreaksDetected != 1 || len(r.Breaks) != 1 {
		t.Fatalf("native run must fail an injected dropout: %+v", r)
	}
}

func TestNativeLoopbackSilenceCannotPass(t *testing.T) {
	r := runNativeFake(t, false, false)
	if r.Passed || r.LatencyMeasuredFrames != -1 || r.BreaksDetected != 0 || r.DurationSeconds == 0 || r.Callbacks == 0 {
		t.Fatalf("unconnected capture must yield an honest failed report: %+v", r)
	}
}

func TestNativeLoopbackRejectsEndpointsConfigAndLoad(t *testing.T) {
	host, _ := NewFakeHost(FakeConfig{Manual: true, Loopback: true})
	device := nativeTestDevice(t, host)
	for _, name := range []string{"host", "missing", "stale", "direction", "channels", "rate", "duration", "delay", "load", "load-process"} {
		t.Run(name, func(t *testing.T) {
			h, out, in := host, device, device
			cfg, opts := nativeTestConfig(), nativeTestOptions()
			switch name {
			case "host":
				out.Host = "other"
			case "missing":
				in.ID = ""
			case "stale":
				in.ID = "vanished"
			case "direction":
				h, _ = NewFakeHost(FakeConfig{Devices: []tymbal.Device{{ID: "out", Outputs: 1}}})
				out, in = nativeTestDevice(t, h), nativeTestDevice(t, h)
			case "channels":
				cfg.InChannels = 3
			case "rate":
				cfg.SampleRate = 12345
			case "duration":
				opts.Duration = 0
			case "delay":
				opts.MaxDelayPeriods = nativeMaxDelay
			case "load":
				opts.Load = []string{"disk"}
			case "load-process":
				opts.Load = []string{"gc"}
			}
			_, err := NativeLoopback(h, out, in, cfg, opts)
			if err == nil {
				t.Fatal("unsupported request was accepted")
			}
		})
	}
}

func TestNativeCallbackNoAllocAndCompletePeriods(t *testing.T) {
	cfg, opts := nativeTestConfig(), nativeTestOptions()
	m, err := newNativeMetrics(tymbal.Actual{SampleRate: cfg.SampleRate, Period: cfg.Period, InChannels: 1, OutChannels: 1}, opts)
	if err != nil {
		t.Fatal(err)
	}
	in, out := [][]float32{make([]float32, cfg.Period)}, [][]float32{make([]float32, cfg.Period)}
	run := func() {
		m.processed, m.continuousFrames, m.invalid = 0, 0, false
		m.done.Store(false)
		m.continuity = nativeContinuity{}
		m.continuity.init(cfg.SampleRate)
		for frame := uint64(0); frame < m.end; frame += uint64(cfg.Period) {
			m.callback(tymbal.Time{Frame: frame}, in, out)
		}
	}
	if n := testing.AllocsPerRun(1, run); n != 0 {
		t.Fatalf("active probe and continuity callbacks allocated %v objects", n)
	}
	if m.invalid || !m.done.Load() || m.processed != m.end {
		t.Fatal("run did not complete on a period boundary")
	}
	m.done.Store(false)
	m.callback(tymbal.Time{Frame: m.processed}, [][]float32{in[0][:cfg.Period-1]}, out)
	if !m.invalid || !m.done.Load() {
		t.Fatal("short input period was accepted")
	}
	for _, sample := range out[0] {
		if sample != 0 {
			t.Fatal("invalid callback must clear output")
		}
	}
}

func TestNativeContinuityMatchesExistingDetector(t *testing.T) {
	const rate = 48000
	for _, fault := range []bool{false, true} {
		var c nativeContinuity
		c.init(rate)
		samples := make([]float32, 8192)
		for i := range samples {
			samples[i] = float32(.5 * math.Sin(2*math.Pi*testToneHz*float64(i)/rate))
			if fault && i >= 4096 && i < 4352 {
				samples[i] = 0
			}
			c.sample(uint64(i), float64(samples[i]))
		}
		want := DetectContinuity(samples, rate)
		if c.count != uint64(len(want)) {
			t.Fatalf("fault=%v streaming breaks=%d offline=%v", fault, c.count, want)
		}
		for i := range want {
			if c.breaks[i] != want[i] {
				t.Fatalf("streaming frame=%d offline frame=%d", c.breaks[i].Frame, want[i].Frame)
			}
		}
	}
}

func TestNativeCorrelationRejectsNoiseAndHandlesPolarity(t *testing.T) {
	for _, polarity := range []float64{1, -1, 0} {
		m, err := newNativeMetrics(tymbal.Actual{SampleRate: 48000, Period: 256, InChannels: 1, OutChannels: 1}, nativeTestOptions())
		if err != nil {
			t.Fatal(err)
		}
		const delay = 137
		seed := uint64(42)
		for frame := uint64(0); frame < m.probeEnd; frame++ {
			seed = seed*6364136223846793005 + 1
			sample := (float64(seed>>32)/float64(uint64(1)<<32) - .5) * .001
			if frame >= m.probeStart+delay && frame-m.probeStart-delay < uint64(len(m.probe)) {
				sample += .2 * polarity * float64(m.probe[frame-m.probeStart-delay])
			}
			m.correlate(frame, sample)
		}
		want := delay
		if polarity == 0 {
			want = -1
		}
		if got := m.correlation.latency(); got != want {
			t.Fatalf("polarity=%v latency=%d want=%d", polarity, got, want)
		}
	}
}

// A child of this test binary exits without touching audio. This exercises the
// harness's early-exit monitoring, failed partial report, and process reaping.
func TestNativeLoadChild(t *testing.T) {
	for i, arg := range os.Args {
		if arg == "--" {
			if i+1 < len(os.Args) && os.Args[i+1] == "stay" {
				for {
					time.Sleep(time.Hour)
				}
			}
			os.Exit(0)
		}
	}
}

func nativeTestExecutable(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNativeLoopbackChildLoadLifecycle(t *testing.T) {
	r := runNativeFake(t, true, false, nativeTestExecutable(t), "-test.run=^TestNativeLoadChild$", "--", "stay")
	if !r.Passed || len(r.Load) != 2 || r.Load[0] != "cpu" || r.Load[1] != "gc" {
		t.Fatalf("child load lifecycle/report = %+v", r)
	}
}

func TestNativeLoopbackMonitorsChildLoad(t *testing.T) {
	host, _ := NewFakeHost(FakeConfig{Manual: true, Loopback: true, Delay: 2})
	device := nativeTestDevice(t, host)
	opts := nativeTestOptions()
	opts.Load = []string{"gc"}
	opts.LoadCommand = []string{nativeTestExecutable(t), "-test.run=^TestNativeLoadChild$", "--"}
	r, err := NativeLoopback(host, device, device, nativeTestConfig(), opts)
	if err == nil || !strings.Contains(err.Error(), "load worker exited early") || r.Passed || r.Host != "fake" || len(r.Load) != 1 || r.Load[0] != "gc" {
		t.Fatalf("child exit: report=%+v error=%v", r, err)
	}
}

// Explicitly opt in on a host with a connected loopback pair. A successful
// short run is diagnostic evidence, not hardware/soak acceptance.
func TestNativeLoopbackPlatform(t *testing.T) {
	name := os.Getenv("TYMBAL_NATIVE_HOST")
	if name == "" {
		t.Skip("set TYMBAL_NATIVE_HOST, TYMBAL_NATIVE_OUT and TYMBAL_NATIVE_IN to run a native loopback")
	}
	var host tymbal.Host
	for _, h := range tymbal.Hosts() {
		if h.Name() == name {
			host = h
		}
	}
	if host.Name() == "" {
		t.Fatalf("platform host %q unavailable", name)
	}
	devices, err := host.Devices()
	if err != nil {
		t.Fatal(err)
	}
	out, okOut := findDevice(devices, os.Getenv("TYMBAL_NATIVE_OUT"))
	in, okIn := findDevice(devices, os.Getenv("TYMBAL_NATIVE_IN"))
	if !okOut || !okIn {
		t.Fatal("explicit native output and input IDs must be enumerated")
	}
	cfg, opts := nativeTestConfig(), nativeTestOptions()
	opts.Duration, opts.MaxDelayPeriods = time.Second, 0
	for key, target := range map[string]*int{"TYMBAL_NATIVE_RATE": &cfg.SampleRate, "TYMBAL_NATIVE_PERIOD": &cfg.Period, "TYMBAL_NATIVE_PERIODS": &cfg.Periods} {
		if value := os.Getenv(key); value != "" {
			*target, err = strconv.Atoi(value)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if value := os.Getenv("TYMBAL_NATIVE_DURATION"); value != "" {
		opts.Duration, err = time.ParseDuration(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := NativeLoopback(host, out, in, cfg, opts)
	var encoded bytes.Buffer
	if writeErr := WriteReport(&encoded, r); writeErr != nil {
		t.Fatal(writeErr)
	}
	t.Log(encoded.String())
	if err != nil || !r.Passed {
		t.Fatalf("native diagnostic failed: %v", err)
	}
}
