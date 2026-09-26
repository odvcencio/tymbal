package tymbaltest

import (
	"errors"
	"fmt"
	"math"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"m31labs.dev/tymbal"
)

// NativeOptions configures a real duplex loopback run. The caller must connect
// output channel zero to input channel zero. Duration covers the continuity
// pass; a separate MLS probe precedes it. MaxDelayPeriods bounds both latency
// search and startup silence (default: at least 250 ms).
type NativeOptions struct {
	Duration        time.Duration
	MaxDelayPeriods int
	Load            []string
	// LoadCommand is an executable and optional prefix arguments for an
	// external load worker accepting -load cpu,gc. It is required with Load;
	// the harness starts, monitors, and reaps it outside the audio callback.
	LoadCommand []string
}

const (
	nativeProbePoints = 512
	nativeMaxDelay    = 1 << 18
	nativeMaxBreaks   = 4096
)

// NativeLoopback measures an enumerated duplex pair without simulating or
// synchronizing endpoints. Backend refusal is returned unchanged. FakeHost is
// accepted for orchestration tests and is always reported as "fake".
//
// Capture is reduced immediately to correlation sums and overlapping harmonic
// window sums; no microphone samples are retained. Passed describes this run's
// signal/timing checks, not a hardware milestone or a one-hour acceptance gate.
// A run error may accompany a populated, failed Report with partial statistics.
// Breaks retains the first 4096 findings; BreaksDetected counts all findings.
func NativeLoopback(host tymbal.Host, out, in tymbal.Device, cfg tymbal.Config, opts NativeOptions) (Report, error) {
	if host.Name() == "" || out.ID == "" || in.ID == "" || out.Host != host.Name() || in.Host != host.Name() {
		return Report{}, fmt.Errorf("%w: an output and input from the selected host are required", tymbal.ErrState)
	}
	devices, err := host.Devices()
	if err != nil {
		return Report{}, err
	}
	out, outOK := findDevice(devices, out.ID)
	in, inOK := findDevice(devices, in.ID)
	if !outOK || !inOK || out.Outputs <= 0 || in.Inputs <= 0 {
		return Report{}, fmt.Errorf("%w: loopback requires enumerated output and input endpoints", tymbal.ErrState)
	}
	if cfg.SampleRate <= 0 || cfg.Period <= 0 || cfg.Periods < 0 || cfg.OutChannels <= 0 || cfg.InChannels <= 0 || opts.Duration <= 0 || opts.MaxDelayPeriods < 0 {
		return Report{}, fmt.Errorf("%w: invalid native loopback configuration", tymbal.ErrState)
	}
	for _, kind := range opts.Load {
		if kind != "cpu" && kind != "gc" {
			return Report{}, fmt.Errorf("%w: %s", ErrLoad, kind)
		}
	}
	if len(opts.Load) > 0 && len(opts.LoadCommand) == 0 {
		return Report{}, fmt.Errorf("%w: native load requires a child-process LoadCommand", ErrLoad)
	}
	cfg.Output, cfg.Input = &out, &in
	var metrics *nativeMetrics // assigned before Start publishes the callback
	stream, err := tymbal.Open(host, cfg, func(t tymbal.Time, input, output [][]float32) {
		metrics.callback(t, input, output)
	})
	if err != nil {
		return Report{}, err
	}
	defer stream.Close()
	actual := stream.Actual()
	metrics, err = newNativeMetrics(actual, opts)
	if err != nil {
		return Report{}, err
	}

	var loadExit <-chan error
	if len(opts.Load) > 0 {
		args := append([]string{}, opts.LoadCommand[1:]...)
		args = append(args, "-load", strings.Join(opts.Load, ","))
		child := exec.Command(opts.LoadCommand[0], args...)
		if err := child.Start(); err != nil {
			return Report{}, fmt.Errorf("native load: %w", err)
		}
		exited := make(chan error, 1)
		go func() { exited <- child.Wait() }()
		loadExit = exited
		defer func() {
			if loadExit != nil {
				_ = child.Process.Kill()
				<-loadExit
			}
		}()
	}
	if err := stream.Start(); err != nil {
		return Report{}, err
	}
	// A stopped device clock must not turn a bounded run into an infinite wait.
	// Duration is measured in complete callback frames, not timer ticks.
	watchdog := time.NewTimer(metrics.runDuration + 5*time.Second)
	defer watchdog.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var runErr error
run:
	for {
		select {
		case err := <-loadExit:
			loadExit = nil // already reaped; the deferred cleanup must not wait again
			runErr = fmt.Errorf("native load worker exited early: %v", err)
			break run
		case <-watchdog.C:
			runErr = fmt.Errorf("native loopback timed out before completing requested frames")
			break run
		case <-ticker.C:
			if err := stream.Err(); err != nil {
				runErr = err
				break run
			}
			if metrics.done.Load() {
				break run
			}
		}
	}
	stopErr := stream.Stop() // joins the callback before reading derived metrics
	var stats tymbal.Stats
	stream.Stats(&stats)
	actual = stream.Actual() // priority is granted during Start
	runErr = errors.Join(runErr, stopErr, stream.Err(), stream.Close())
	record := metrics.report(host.Name(), out.ID, in.ID, actual, stats, opts.Load)
	if metrics.invalid {
		runErr = errors.Join(runErr, fmt.Errorf("native loopback received invalid samples or incomplete callback buffers"))
	}
	if record.LatencyMeasuredFrames < 0 {
		runErr = errors.Join(runErr, errNoCorrelation)
	}
	if !metrics.continuity.active {
		runErr = errors.Join(runErr, fmt.Errorf("native loopback captured no usable continuity signal"))
	}
	if runErr != nil {
		record.Passed = false
	}
	return record, runErr
}

type nativeCorrelation struct {
	sums, energies []float64
	counts         []uint16
	points         [nativeProbePoints]float32
}

type nativeMetrics struct {
	period, inChannels, outChannels int
	probeStart, probeEnd            uint64
	maxDelay                        int
	end, continuousFrames           uint64
	processed                       uint64
	runDuration                     time.Duration
	probe                           []float32 // generated output only
	correlation                     nativeCorrelation
	sine                            Sine
	continuity                      nativeContinuity
	invalid, discontinuity          bool
	done                            atomic.Bool
}

func newNativeMetrics(actual tymbal.Actual, opts NativeOptions) (*nativeMetrics, error) {
	if actual.SampleRate <= 2*testToneHz || actual.Period <= 0 || actual.Period > nativeMaxDelay || actual.InChannels <= 0 || actual.OutChannels <= 0 {
		return nil, fmt.Errorf("%w: unsupported native signal parameters", tymbal.ErrFormat)
	}
	delayPeriods := opts.MaxDelayPeriods
	if delayPeriods == 0 {
		delayPeriods = int(math.Ceil(float64(actual.SampleRate) / (4 * float64(actual.Period))))
	}
	if delayPeriods <= 0 || delayPeriods > nativeMaxDelay/actual.Period {
		return nil, fmt.Errorf("%w: latency search exceeds %d frames", tymbal.ErrFormat, nativeMaxDelay)
	}
	maxDelay := delayPeriods * actual.Period
	frames := math.Ceil(opts.Duration.Seconds() * float64(actual.SampleRate))
	// Keep frame arithmetic and the watchdog duration representable.
	if frames <= 0 || frames >= float64(math.MaxInt64-nativeMaxDelay*10) || frames > float64(actual.SampleRate)*float64((time.Duration(math.MaxInt64)-10*time.Second)/time.Second) {
		return nil, fmt.Errorf("%w: native duration is out of range", tymbal.ErrState)
	}
	continuousFrames := uint64(math.Ceil(frames/float64(actual.Period))) * uint64(actual.Period)
	minimum := uint64(roundUp(maxDelay+4*actual.Period+continuityWindow, actual.Period))
	if continuousFrames < minimum {
		continuousFrames = minimum
	}
	probe := MLS(float32(math.Pow(10, testToneDB/20)))
	probeStart := uint64(4 * actual.Period)
	probeEnd := uint64(roundUp(int(probeStart)+len(probe)+maxDelay+actual.Period, actual.Period))
	m := &nativeMetrics{
		period: actual.Period, inChannels: actual.InChannels, outChannels: actual.OutChannels,
		probeStart: probeStart, probeEnd: probeEnd, maxDelay: maxDelay,
		end: probeEnd + continuousFrames, probe: probe,
		correlation: nativeCorrelation{
			sums: make([]float64, maxDelay+1), energies: make([]float64, maxDelay+1), counts: make([]uint16, maxDelay+1),
		},
		sine: NewSine(actual.SampleRate, testToneHz, math.Pow(10, testToneDB/20)),
	}
	runNanos := math.Ceil(float64(m.end) / float64(actual.SampleRate) * float64(time.Second))
	if runNanos >= float64(time.Duration(math.MaxInt64)-5*time.Second) {
		return nil, fmt.Errorf("%w: native duration is out of range", tymbal.ErrState)
	}
	m.runDuration = time.Duration(runNanos)
	// Stratified points of the same MLS give bounded work per input frame.
	// Every candidate delay gets the same 512 observations; normalization and
	// a strong-peak threshold reject silence and unrelated captures.
	for i := range m.correlation.points {
		m.correlation.points[i] = probe[i*64]
	}
	m.continuity.init(actual.SampleRate)
	m.continuity.skipFrames = uint64(maxDelay)
	return m, nil
}

func (m *nativeMetrics) callback(t tymbal.Time, input, output [][]float32) {
	clearOutput(output)
	if m.done.Load() {
		return
	}
	if len(input) != m.inChannels || len(output) != m.outChannels || t.Frame != m.processed {
		m.invalid = true
		m.done.Store(true)
		return
	}
	for _, ch := range input {
		if len(ch) != m.period {
			m.invalid = true
			m.done.Store(true)
			return
		}
	}
	for _, ch := range output {
		if len(ch) != m.period {
			m.invalid = true
			m.done.Store(true)
			return
		}
	}
	if t.Discontinuity {
		m.discontinuity = true
	}
	if t.Frame >= m.probeEnd {
		m.sine.Render(t, nil, output)
	}
	for i, sample := range input[0] {
		frame := t.Frame + uint64(i)
		v := float64(sample)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			m.invalid = true
			v = 0
		}
		if frame < m.probeEnd {
			if frame >= m.probeStart && frame-m.probeStart < uint64(len(m.probe)) {
				output[0][i] = m.probe[frame-m.probeStart]
			}
			m.correlate(frame, v)
		} else {
			m.continuity.sample(frame-m.probeEnd, v)
			m.continuousFrames++
		}
	}
	m.processed += uint64(m.period)
	if m.processed >= m.end {
		m.done.Store(true)
	}
}

func (m *nativeMetrics) correlate(frame uint64, sample float64) {
	if frame < m.probeStart {
		return
	}
	pos := int(frame - m.probeStart)
	lo := 0
	if pos > m.maxDelay {
		lo = (pos - m.maxDelay + 63) / 64
	}
	hi := pos / 64
	if hi >= nativeProbePoints {
		hi = nativeProbePoints - 1
	}
	for i := lo; i <= hi; i++ {
		delay := pos - i*64
		m.correlation.sums[delay] += sample * float64(m.correlation.points[i])
		m.correlation.energies[delay] += sample * sample
		m.correlation.counts[delay]++
	}
}

func (c *nativeCorrelation) latency() int {
	peak, delay := 0.8, -1
	probeEnergy := float64(c.points[0]) * float64(c.points[0]) * nativeProbePoints
	for i, sum := range c.sums {
		if c.counts[i] != nativeProbePoints || c.energies[i] < 1e-12 {
			continue
		}
		score := math.Abs(sum) / math.Sqrt(probeEnergy*c.energies[i])
		if score > peak {
			peak, delay = score, i
		}
	}
	return delay
}

// Four overlapping harmonic sums replace a raw-sample ring. The window/hop,
// phase threshold, 6 dB amplitude threshold and finding merge match the fake
// detector. The level reference is the largest window observed so far, so the
// streaming detector does not require storing a whole soak's derived windows.
type nativeContinuity struct {
	cos, sin   [continuityWindow]float64
	re, im     [continuityWindow / continuityHop]float64
	maxAmp     float64
	prevPhase  float64
	active     bool
	windows    uint64
	count      uint64
	lastBreak  uint64
	breaks     [nativeMaxBreaks]Break
	stored     int
	skipFrames uint64
}

func (c *nativeContinuity) init(rate int) {
	omega := 2 * math.Pi * testToneHz / float64(rate)
	for i := range c.cos {
		c.cos[i], c.sin[i] = math.Cos(omega*float64(i)), math.Sin(omega*float64(i))
	}
}

func (c *nativeContinuity) sample(frame uint64, value float64) {
	window := frame / continuityHop
	for back := uint64(0); back < continuityWindow/continuityHop && back <= window; back++ {
		w := window - back
		i := int(frame - w*continuityHop)
		slot := w % (continuityWindow / continuityHop)
		c.re[slot] += value * c.cos[i]
		c.im[slot] -= value * c.sin[i]
		if i == continuityWindow-1 {
			c.finish(w*continuityHop, c.re[slot], c.im[slot])
			c.re[slot], c.im[slot] = 0, 0
		}
	}
}

func (c *nativeContinuity) finish(frame uint64, re, im float64) {
	if frame < c.skipFrames {
		return
	}
	c.windows++
	amp := 2 * math.Hypot(re, im) / continuityWindow
	phase := math.Atan2(im, re)
	if amp > c.maxAmp {
		c.maxAmp = amp
	}
	if c.maxAmp < 1e-6 {
		return
	}
	if !c.active {
		if amp >= c.maxAmp*0.5 {
			c.active, c.prevPhase = true, phase
		}
		return
	}
	// The phase advance is the angle at continuityHop in the precomputed table.
	expected := math.Atan2(c.sin[continuityHop], c.cos[continuityHop])
	bad := amp < c.maxAmp*math.Pow(10, -6.0/20.0)
	if amp >= c.maxAmp*0.5 && math.Abs(wrapPhase(phase-c.prevPhase-expected)) > 0.1 {
		bad = true
	}
	if bad && (c.count == 0 || frame-c.lastBreak > continuityWindow) {
		c.count++
		c.lastBreak = frame
		if c.stored < len(c.breaks) {
			c.breaks[c.stored] = Break{Frame: frame}
			c.stored++
		}
	}
	c.prevPhase = phase
}

func (m *nativeMetrics) report(host, out, in string, actual tymbal.Actual, stats tymbal.Stats, load []string) Report {
	r := Report{
		Host: host, Output: out, Input: in, Rate: actual.SampleRate, Period: actual.Period, Periods: actual.Periods,
		DurationSeconds: float64(m.continuousFrames) / float64(actual.SampleRate), Load: append([]string{}, load...),
		Priority: actual.Priority, DeadlineAvailable: actual.HasDeadline,
		Callbacks: stats.Callbacks, Late: stats.Late, DropoutsReported: stats.Dropouts,
		BreaksDetected: m.continuity.count, Breaks: append([]Break(nil), m.continuity.breaks[:m.continuity.stored]...),
		LatencyMeasuredFrames: m.correlation.latency(),
		LatencyReportedFrames: int(math.Round((actual.LatencyOut.Seconds() + actual.LatencyIn.Seconds()) * float64(actual.SampleRate))),
		Allocs:                stats.AllocsSinceRun, Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, CPU: "unreported",
	}
	r.FillTiming(stats)
	// Physical round trip need not equal OS-reported buffering. Keep both raw
	// measurements; device-specific latency tolerance requires repeated runs.
	r.Passed = m.processed >= m.end && !m.invalid && !m.discontinuity && r.LatencyMeasuredFrames >= 0 && m.continuity.active && m.continuity.windows >= 2 && r.BreaksDetected == 0 && r.DropoutsReported == 0
	if actual.HasDeadline {
		periodUS := float64(actual.Period) / float64(actual.SampleRate) * 1e6
		r.Passed = r.Passed && r.WakeLateUS.P999 < periodUS*0.25 && r.WakeLateUS.Max < periodUS*0.75
	}
	return r
}
