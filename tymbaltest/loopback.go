package tymbaltest

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"m31labs.dev/tymbal"
)

var (
	ErrNoFakeDevice = errors.New("tymbaltest: fake host has no usable loopback device")
	ErrLoad         = errors.New("tymbaltest: unsupported load generator")
)

// LoopbackOptions configures the deterministic virtual loopback run.
type LoopbackOptions struct {
	Duration              time.Duration
	Load                  []string
	DelayPeriods          int
	MaxDelayPeriods       int
	InjectDropout         bool
	InjectDropoutAtPeriod int
	Seed                  int64
}

// Loopback creates a manual FakeHost for out/in, measures a maximum-length
// sequence's round-trip delay, then checks a phase-continuous sine capture.
// It reports virtual stream statistics and never represents a hardware soak.
func Loopback(out, in tymbal.Device, cfg tymbal.Config, opts LoopbackOptions) (Report, error) {
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 48_000
	}
	if cfg.Period <= 0 {
		cfg.Period = 128
	}
	if cfg.Periods < 0 {
		return Report{}, fmt.Errorf("tymbaltest: negative buffer depth %d", cfg.Periods)
	}
	if cfg.OutChannels <= 0 {
		cfg.OutChannels = 1
	}
	if cfg.InChannels <= 0 {
		cfg.InChannels = 1
	}
	if out.ID == "" || in.ID == "" {
		return Report{}, fmt.Errorf("tymbaltest: output and input device IDs are required")
	}
	if opts.Duration <= 0 {
		opts.Duration = time.Second
	}
	delayPeriods := opts.DelayPeriods
	if delayPeriods <= 0 {
		delayPeriods = 2
	}
	maxDelayPeriods := opts.MaxDelayPeriods
	if maxDelayPeriods <= delayPeriods {
		maxDelayPeriods = delayPeriods + 2
	}
	if opts.Seed == 0 {
		opts.Seed = 1
	}
	stopLoad, err := startLoad(opts.Load)
	if err != nil {
		return Report{}, err
	}
	defer stopLoad()

	// Use separate virtual hosts for the two passes so the latency probe is
	// surrounded by silence and starts from a clean loopback clock.
	probe := MLS(float32(math.Pow(10, testToneDB/20)))
	probeStart := cfg.Period * 4
	maxDelay := cfg.Period * maxDelayPeriods
	probeFrames := roundUp(probeStart+len(probe)+maxDelay+cfg.Period, cfg.Period)
	probeCapture := make([]float32, probeFrames)
	probeCallback := func(t tymbal.Time, input, output [][]float32) {
		clearOutput(output)
		frame := int(t.Frame)
		if len(output) != 0 {
			for i := range output[0] {
				pos := frame + i - probeStart
				if pos >= 0 && pos < len(probe) {
					output[0][i] = probe[pos]
				}
			}
		}
		captureInput(probeCapture, frame, input)
	}
	_, latencyActual, err := runManualFake(out, in, cfg, delayPeriods, probeFrames/cfg.Period, probeCallback, nil)
	if err != nil {
		return Report{}, fmt.Errorf("latency pass: %w", err)
	}
	measuredLatency, err := MeasureLatencyFrames(probeCapture, probe, probeStart, maxDelay)
	if err != nil {
		return Report{}, err
	}

	continuousFrames := roundUp(int(math.Ceil(float64(cfg.SampleRate)*opts.Duration.Seconds())), cfg.Period)
	minFrames := (delayPeriods+4)*cfg.Period + continuityWindow
	if continuousFrames < minFrames {
		continuousFrames = roundUp(minFrames, cfg.Period)
	}
	if opts.InjectDropout && opts.InjectDropoutAtPeriod <= 0 {
		opts.InjectDropoutAtPeriod = delayPeriods + 24
		tailPeriods := (continuityWindow + cfg.Period - 1) / cfg.Period
		if lastPeriod := continuousFrames/cfg.Period - 1 - tailPeriods; opts.InjectDropoutAtPeriod > lastPeriod {
			opts.InjectDropoutAtPeriod = lastPeriod
		}
	}
	capture := make([]float32, continuousFrames)
	gen := NewSine(cfg.SampleRate, testToneHz, math.Pow(10, testToneDB/20))
	callback := func(t tymbal.Time, input, output [][]float32) {
		gen.Render(t, nil, output)
		captureInput(capture, int(t.Frame), input)
	}
	var injectAt *int
	if opts.InjectDropout {
		at := opts.InjectDropoutAtPeriod
		if at >= continuousFrames/cfg.Period {
			return Report{}, fmt.Errorf("tymbaltest: dropout period %d exceeds run length", at)
		}
		injectAt = &at
	}
	stats, actual, runErr := runManualFake(out, in, cfg, delayPeriods, continuousFrames/cfg.Period, callback, injectAt)
	if runErr != nil {
		return Report{}, fmt.Errorf("continuity pass: %w", runErr)
	}
	breaks := DetectContinuity(capture, actual.SampleRate)
	reportedLatency := int(math.Round(float64(latencyActual.LatencyOut+latencyActual.LatencyIn) * float64(actual.SampleRate) / float64(time.Second)))
	expectedBreaks := 0
	expectedDropouts := uint64(0)
	if opts.InjectDropout {
		expectedBreaks = 1
		expectedDropouts = 1
	}
	passed := measuredLatency == reportedLatency && len(breaks) == expectedBreaks && stats.Dropouts == expectedDropouts
	report := Report{
		Host:                  "fake",
		Output:                out.ID,
		Input:                 in.ID,
		Rate:                  actual.SampleRate,
		Period:                actual.Period,
		Periods:               actual.Periods,
		DurationSeconds:       float64(continuousFrames) / float64(actual.SampleRate),
		Load:                  append([]string{}, opts.Load...),
		Priority:              actual.Priority,
		DeadlineAvailable:     actual.HasDeadline,
		Callbacks:             stats.Callbacks,
		Late:                  stats.Late,
		DropoutsReported:      stats.Dropouts,
		BreaksDetected:        uint64(len(breaks)),
		Breaks:                breaks,
		LatencyMeasuredFrames: measuredLatency,
		LatencyReportedFrames: reportedLatency,
		Allocs:                stats.AllocsSinceRun,
		Go:                    runtime.Version(),
		OS:                    runtime.GOOS,
		Arch:                  runtime.GOARCH,
		CPU:                   "unreported",
		Seed:                  opts.Seed,
		Passed:                passed,
	}
	report.FillTiming(stats)
	return report, nil
}

// FakeLoopback runs Loopback against the first suitable device from a default
// FakeHost. It is the ready-to-run hardware-free conformance entry point.
func FakeLoopback(cfg tymbal.Config, opts LoopbackOptions) (Report, error) {
	host, _ := NewFakeHost(FakeConfig{Manual: true, Loopback: true, Delay: positiveOr(opts.DelayPeriods, 2)})
	devices, err := host.Devices()
	if err != nil {
		return Report{}, err
	}
	for _, dev := range devices {
		if dev.Inputs > 0 && dev.Outputs > 0 {
			return Loopback(dev, dev, cfg, opts)
		}
	}
	var output, input *tymbal.Device
	for i := range devices {
		if output == nil && devices[i].Outputs > 0 {
			output = &devices[i]
		}
		if input == nil && devices[i].Inputs > 0 {
			input = &devices[i]
		}
	}
	if output != nil && input != nil {
		return Loopback(*output, *input, cfg, opts)
	}
	return Report{}, ErrNoFakeDevice
}

func runManualFake(out, in tymbal.Device, cfg tymbal.Config, delay, periods int, cb tymbal.Callback, dropoutAt *int) (tymbal.Stats, tymbal.Actual, error) {
	fakeDevices := []tymbal.Device{out}
	if in.ID != out.ID {
		fakeDevices = append(fakeDevices, in)
	}
	host, control := NewFakeHost(FakeConfig{
		Manual: true, Loopback: true, Delay: delay,
		Devices: fakeDevices,
	})
	devices, err := host.Devices()
	if err != nil {
		return tymbal.Stats{}, tymbal.Actual{}, err
	}
	resolvedOut, ok := findDevice(devices, out.ID)
	if !ok {
		return tymbal.Stats{}, tymbal.Actual{}, fmt.Errorf("tymbaltest: fake output %q was not enumerated", out.ID)
	}
	resolvedIn, ok := findDevice(devices, in.ID)
	if !ok {
		return tymbal.Stats{}, tymbal.Actual{}, fmt.Errorf("tymbaltest: fake input %q was not enumerated", in.ID)
	}
	cfg.Output, cfg.Input = &resolvedOut, &resolvedIn
	stream, err := tymbal.Open(host, cfg, cb)
	if err != nil {
		return tymbal.Stats{}, tymbal.Actual{}, err
	}
	actual := stream.Actual()
	if err := stream.Start(); err != nil {
		_ = stream.Close()
		return tymbal.Stats{}, actual, err
	}
	if dropoutAt != nil {
		control.InjectDropout(*dropoutAt)
	}
	advanceErr := control.Advance(periods)
	stopErr := stream.Stop()
	var stats tymbal.Stats
	stream.Stats(&stats)
	closeErr := stream.Close()
	if advanceErr != nil {
		return stats, actual, advanceErr
	}
	if stopErr != nil {
		return stats, actual, stopErr
	}
	if closeErr != nil {
		return stats, actual, closeErr
	}
	if err := stream.Err(); err != nil {
		return stats, actual, err
	}
	return stats, actual, nil
}

func findDevice(devices []tymbal.Device, id string) (tymbal.Device, bool) {
	for _, d := range devices {
		if d.ID == id {
			return d, true
		}
	}
	return tymbal.Device{}, false
}

func clearOutput(output [][]float32) {
	for ch := range output {
		clear(output[ch])
	}
}

func captureInput(dst []float32, frame int, input [][]float32) {
	if frame < 0 || len(input) == 0 {
		return
	}
	for i, v := range input[0] {
		pos := frame + i
		if pos >= len(dst) {
			break
		}
		dst[pos] = v
	}
}

func roundUp(value, multiple int) int {
	if multiple <= 0 {
		return value
	}
	return ((value + multiple - 1) / multiple) * multiple
}

func positiveOr(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

var loadSink atomic.Uint64

func startLoad(load []string) (func(), error) {
	if len(load) == 0 {
		return func() {}, nil
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, kind := range load {
		switch kind {
		case "cpu":
			for i := 0; i < runtime.NumCPU(); i++ {
				wg.Add(1)
				go func(seed uint64) {
					defer wg.Done()
					x := seed + 1
					for {
						for j := 0; j < 4096; j++ {
							x = x*6364136223846793005 + 1
						}
						loadSink.Store(x)
						select {
						case <-done:
							return
						default:
						}
					}
				}(uint64(i))
			}
		case "gc":
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					chunk := make([]byte, 32*1024)
					loadSink.Store(uint64(chunk[0]))
					select {
					case <-done:
						return
					default:
					}
				}
			}()
		default:
			close(done)
			wg.Wait()
			return nil, fmt.Errorf("%w: %s", ErrLoad, kind)
		}
	}
	return func() {
		close(done)
		wg.Wait()
	}, nil
}
