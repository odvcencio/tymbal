package tymbaltest

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"m31labs.dev/tymbal"
	"m31labs.dev/tymbal/internal/rt"
)

// SilentOptions configures a silent render or duplex continuity run. The
// optional child load command accepts -load cpu,gc, matching NativeLoopback.
type SilentOptions struct {
	Duration    time.Duration
	Load        []string
	LoadCommand []string
}

type silentClockTracker struct {
	frequency uint64
	firstPos  uint64
	lastPos   uint64
	firstQPC  int64
	lastQPC   int64
	samples   uint64
	invalid   bool
	steps     tymbal.Histogram
	stepMax   time.Duration
}

//tymbal:rt
func (c *silentClockTracker) observe(position, frequency uint64, qpc int64) {
	if frequency == 0 || qpc <= 0 || c.samples != 0 && (frequency != c.frequency || position < c.lastPos || qpc < c.lastQPC) {
		c.invalid = true
		return
	}
	if c.samples == 0 {
		c.frequency, c.firstPos, c.firstQPC = frequency, position, qpc
	} else if step := qpc - c.lastQPC; step > 0 {
		d := time.Duration(step)
		c.steps.Record(d)
		if d > c.stepMax {
			c.stepMax = d
		}
	}
	c.lastPos, c.lastQPC = position, qpc
	c.samples++
}

func (c *silentClockTracker) report() ClockReport {
	r := ClockReport{
		Samples: c.samples, Frequency: c.frequency,
		PositionFirst: c.firstPos, PositionLast: c.lastPos,
		QPCFirstNano: c.firstQPC, QPCLastNano: c.lastQPC,
	}
	r.QPCStepUS = timingReport(&c.steps, c.stepMax)
	if c.samples > 1 && c.lastQPC > c.firstQPC && c.frequency != 0 {
		observed := float64(c.lastPos-c.firstPos) * 1e9 / float64(c.lastQPC-c.firstQPC)
		r.PositionRateErrorPPM = (observed/float64(c.frequency) - 1) * 1e6
	}
	return r
}

type silentMetrics struct {
	period, inChannels, outChannels int
	callbacks                       uint64
	processedFrames                 uint64
	discontinuities                 uint64
	nonzeroOutputSamples            uint64
	callbackErrors                  uint64
	outputClock                     silentClockTracker
	inputClock                      silentClockTracker
}

//tymbal:rt
func (m *silentMetrics) callback(t tymbal.Time, input, output [][]float32) {
	if t.Frame != m.processedFrames || len(input) != m.inChannels || len(output) != m.outChannels {
		m.callbackErrors++
	}
	for _, channel := range input {
		if len(channel) != m.period {
			m.callbackErrors++
		}
	}
	for _, channel := range output {
		if len(channel) != m.period {
			m.callbackErrors++
		}
		for i, sample := range channel {
			if sample != 0 {
				m.nonzeroOutputSamples++
				channel[i] = 0
			}
		}
	}
	if t.Discontinuity {
		m.discontinuities++
	}
	if m.outChannels > 0 && t.OutputFrequency != 0 {
		m.outputClock.observe(t.OutputPosition, t.OutputFrequency, t.OutputNano)
	}
	if m.inChannels > 0 && t.InputFrequency != 0 {
		m.inputClock.observe(t.InputPosition, t.InputFrequency, t.InputNano)
	}
	m.callbacks++
	m.processedFrames += uint64(m.period)
}

// NativeSilentSoak runs silent render or shared duplex on the selected host.
// It never writes a nonzero output sample. Input samples are not retained.
func NativeSilentSoak(host tymbal.Host, output tymbal.Device, input *tymbal.Device, cfg tymbal.Config, opts SilentOptions) (Report, error) {
	if host.Name() == "" || output.ID == "" || output.Host != host.Name() || cfg.SampleRate <= 0 || cfg.Period <= 0 || cfg.Periods <= 0 || opts.Duration <= 0 {
		return Report{}, fmt.Errorf("%w: invalid silent soak configuration", tymbal.ErrState)
	}
	if input != nil && (input.ID == "" || input.Host != host.Name()) {
		return Report{}, fmt.Errorf("%w: invalid silent soak input endpoint", tymbal.ErrState)
	}
	for _, kind := range opts.Load {
		if kind != "cpu" && kind != "gc" {
			return Report{}, fmt.Errorf("%w: %s", ErrLoad, kind)
		}
	}
	if len(opts.Load) > 0 && len(opts.LoadCommand) == 0 {
		return Report{}, fmt.Errorf("%w: silent soak load requires a child-process LoadCommand", ErrLoad)
	}
	cfg.Output = &output
	cfg.OutChannels = output.Outputs
	if input != nil {
		cfg.Input = input
		cfg.InChannels = input.Inputs
	}
	open := func(metrics *silentMetrics) (*tymbal.Stream, error) {
		return tymbal.Open(host, cfg, func(t tymbal.Time, in, out [][]float32) {
			metrics.callback(t, in, out)
		})
	}
	metrics := &silentMetrics{inChannels: cfg.InChannels, outChannels: cfg.OutChannels}
	stream, err := open(metrics)
	if err != nil {
		return Report{}, err
	}
	actual := stream.Actual()
	if actual.Period <= 0 || actual.SampleRate <= 0 || actual.Period > math.MaxInt/actual.SampleRate {
		_ = stream.Close()
		return Report{}, tymbal.ErrFormat
	}
	metrics.period = actual.Period

	var worker *silentLoadWorker
	if len(opts.Load) > 0 {
		worker, err = startSilentLoadWorker(opts.LoadCommand, opts.Load)
		if err != nil {
			_ = stream.Close()
			return Report{}, fmt.Errorf("silent soak load: %w", err)
		}
		defer worker.close()
	}
	if runtime.GOOS == "windows" {
		previousProcs := runtime.GOMAXPROCS(1)
		defer runtime.GOMAXPROCS(previousProcs)
	}
	warmThreads()
	var runtimePriority string
	var restorePriority func() error
	if runtime.GOOS == "windows" {
		var priorityErr error
		restorePriority, runtimePriority, priorityErr = rt.RaiseProcessPriority()
		if priorityErr != nil {
			runPriorityErr := priorityErr
			defer func() { _ = restorePriority() }()
			return Report{}, runPriorityErr
		}
		runtimePriority += "; GOMAXPROCS=1"
	} else {
		restorePriority = func() error { return nil }
	}
	defer func() { _ = restorePriority() }()
	if err := stream.Start(); err != nil {
		_ = stream.Close()
		return Report{}, err
	}
	frames := uint64(math.Ceil(opts.Duration.Seconds() * float64(actual.SampleRate)))
	frames = uint64(roundUp(int(frames), actual.Period))
	deadline := time.Now().Add(opts.Duration + 5*time.Second)
	var runErr error
	for metrics.processedFrames < frames {
		if worker != nil && worker.exited.Load() {
			runErr = fmt.Errorf("silent soak load worker exited early: %v", worker.wait())
			break
		}
		if err := stream.Err(); err != nil {
			runErr = err
			break
		}
		if !time.Now().Before(deadline) {
			runErr = fmt.Errorf("silent soak timed out before completing requested frames")
			break
		}
		rt.SleepThread(10 * time.Millisecond)
	}
	stopErr := stream.Stop()
	var stats tymbal.Stats
	stream.Stats(&stats)
	actual = stream.Actual()
	priorityRestoreErr := restorePriority()
	closeErr := stream.Close()
	runErr = errors.Join(runErr, stopErr, stream.Err(), closeErr, priorityRestoreErr)

	mode := "silent-render"
	inputID, inFormat := "", ""
	if input != nil {
		mode = "silent-duplex"
		inputID, inFormat = input.ID, actual.InFormat
	}
	record := Report{
		Host: host.Name(), Mode: mode, Output: output.ID, Input: inputID,
		Rate: actual.SampleRate, Period: actual.Period, Periods: actual.Periods,
		InFormat: inFormat, OutFormat: actual.OutFormat,
		LatencyInUS:     float64(actual.LatencyIn) / float64(time.Microsecond),
		LatencyOutUS:    float64(actual.LatencyOut) / float64(time.Microsecond),
		DurationSeconds: float64(metrics.processedFrames) / float64(actual.SampleRate),
		Load:            append([]string(nil), opts.Load...), Priority: actual.Priority,
		RuntimePriority:   runtimePriority,
		DeadlineAvailable: actual.HasDeadline, Callbacks: stats.Callbacks,
		DropoutsReported: stats.Dropouts, Discontinuities: metrics.discontinuities,
		NonZeroOutputSamples: metrics.nonzeroOutputSamples, CallbackErrors: metrics.callbackErrors,
		WakeLateUS:          timingReport(&stats.WakeLate, stats.WakeLateMax),
		WakeIntervalUS:      timingReport(&stats.WakeInterval, stats.WakeIntervalMax),
		CallbackUS:          timingReport(&stats.CallbackTime, stats.CallbackMax),
		WakeLateBuckets:     stats.WakeLate.Snapshot().Buckets,
		WakeIntervalBuckets: stats.WakeInterval.Snapshot().Buckets,
		CallbackBuckets:     stats.CallbackTime.Snapshot().Buckets,
		Allocs:              stats.AllocsSinceRun, CallbackLoopAllocs: stats.CallbackLoopAllocs,
		Go: runtime.Version(), OS: runtime.GOOS,
		Arch: runtime.GOARCH, CPU: "unreported",
		OutputClock: metrics.outputClock.report(), InputClock: metrics.inputClock.report(),
	}
	if metrics.callbackErrors != 0 {
		runErr = errors.Join(runErr, fmt.Errorf("silent soak observed %d callback geometry or sequence error(s)", metrics.callbackErrors))
	}
	if metrics.outputClock.invalid || metrics.inputClock.invalid {
		runErr = errors.Join(runErr, fmt.Errorf("silent soak observed a non-monotonic or changing device clock sample"))
	}
	record.Passed = runErr == nil && metrics.processedFrames >= frames && stats.Dropouts == 0 &&
		metrics.discontinuities == 0 && metrics.nonzeroOutputSamples == 0 && stats.CallbackLoopAllocs == 0 &&
		(!actual.HasDeadline || metrics.outputClock.samples > 1 && (input == nil || metrics.inputClock.samples > 1))
	if actual.HasDeadline {
		periodUS := float64(actual.Period) / float64(actual.SampleRate) * 1e6
		record.Passed = record.Passed && record.WakeLateUS.P999 < periodUS*0.25 && record.WakeLateUS.Max < periodUS*0.75
	}
	if runErr != nil {
		record.Passed = false
	}
	return record, runErr
}

type silentLoadWorker struct {
	child    *exec.Cmd
	exitRead *os.File
	exited   atomic.Bool
	done     chan struct{}
	waited   bool
}

func startSilentLoadWorker(command, load []string) (*silentLoadWorker, error) {
	args := append([]string(nil), command[1:]...)
	args = append(args, "-load", strings.Join(load, ","))
	child := exec.Command(command[0], args...)
	exitRead, exitWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	child.Stdout = exitWrite
	if err := child.Start(); err != nil {
		_ = exitRead.Close()
		_ = exitWrite.Close()
		return nil, err
	}
	_ = exitWrite.Close()
	w := &silentLoadWorker{child: child, exitRead: exitRead, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		var probe [1]byte
		for {
			if _, err := exitRead.Read(probe[:]); err != nil {
				w.exited.Store(true)
				return
			}
		}
	}()
	return w, nil
}

func (w *silentLoadWorker) wait() error {
	w.waited = true
	return w.child.Wait()
}

func (w *silentLoadWorker) close() {
	if !w.waited {
		_ = w.child.Process.Kill()
		_ = w.child.Wait()
	}
	<-w.done
	_ = w.exitRead.Close()
}
