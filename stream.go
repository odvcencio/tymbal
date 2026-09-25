package tymbal

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"m31labs.dev/tymbal/internal/driver"
	"m31labs.dev/tymbal/internal/format"
	"m31labs.dev/tymbal/internal/rt"
)

type streamState uint8

const (
	stateOpened streamState = iota
	stateRunning
	stateStopping
	stateStopped
	stateFailed
	stateClosed
)

type streamStats struct {
	callbacks    atomic.Uint64
	dropouts     atomic.Uint64
	late         atomic.Uint64
	wakeLate     Histogram
	wakeInterval Histogram
	callbackTime Histogram
	wakeLateMax  atomic.Int64
	callbackMax  atomic.Int64
	allocs       atomic.Uint64
}

// Stream owns a single negotiated audio stream.
type Stream struct {
	mu           sync.Mutex
	state        streamState
	closing      bool
	started      bool
	interrupt    bool
	closeDone    chan struct{}
	closeErr     error
	watchdogStop func()

	device driver.Stream
	cb     Callback
	cfg    Config
	actual Actual

	input   [][]float32
	output  [][]float32
	inData  []float32
	outData []float32

	stopRequested atomic.Bool
	startDone     chan struct{}
	done          chan struct{}
	startErr      error // written before startDone closes
	terminalErr   error // protected by mu; first spontaneous failure only

	stats streamStats
}

// Open validates cfg, negotiates the device stream, and allocates all core
// buffers before any audio callback can run.
func Open(h Host, cfg Config, cb Callback) (*Stream, error) {
	if h.d == nil || cb == nil {
		return nil, ErrState
	}
	directions := Direction(0)
	if cfg.Output != nil {
		directions |= Output
	}
	if cfg.Input != nil {
		directions |= Input
	}
	if directions == 0 || cfg.SampleRate <= 0 || cfg.Period <= 0 || cfg.Periods < 0 {
		return nil, ErrState
	}
	if cfg.Periods == 0 {
		cfg.Periods = 2
	}
	if (cfg.Output == nil && cfg.OutChannels != 0) || (cfg.Input == nil && cfg.InChannels != 0) {
		return nil, ErrState
	}
	if cfg.Output != nil {
		if cfg.OutChannels <= 0 || cfg.OutChannels > cfg.Output.Outputs {
			return nil, ErrFormat
		}
	}
	if cfg.Input != nil {
		if cfg.InChannels <= 0 || cfg.InChannels > cfg.Input.Inputs {
			return nil, ErrFormat
		}
	}
	if (cfg.Output != nil && cfg.Output.Host != h.d.Name()) || (cfg.Input != nil && cfg.Input.Host != h.d.Name()) {
		return nil, ErrState
	}
	if cfg.Output != nil && cfg.Input != nil && cfg.Output.Host != cfg.Input.Host {
		return nil, ErrState
	}

	devices, err := h.d.Devices()
	if err != nil {
		return nil, err
	}
	var outInfo, inInfo *driver.Info
	if cfg.Output != nil {
		info, ok := findDevice(devices, cfg.Output.ID, Output)
		if !ok {
			return nil, fmt.Errorf("%w: output device %q is no longer available", ErrState, cfg.Output.ID)
		}
		outInfo = &info
	}
	if cfg.Input != nil {
		info, ok := findDevice(devices, cfg.Input.ID, Input)
		if !ok {
			return nil, fmt.Errorf("%w: input device %q is no longer available", ErrState, cfg.Input.ID)
		}
		inInfo = &info
	}
	req := driver.Request{
		Output: outInfo, Input: inInfo,
		OutChannels: cfg.OutChannels, InChannels: cfg.InChannels,
		SampleRate: cfg.SampleRate, Period: cfg.Period, Periods: cfg.Periods,
		Exclusive: cfg.Exclusive,
	}
	ds, err := h.d.Open(req)
	if err != nil {
		return nil, err
	}
	p := ds.Params()
	if err := validateParams(p, directions); err != nil {
		_ = ds.Close()
		return nil, err
	}
	if (cfg.Output != nil && p.OutChannels <= 0) || (cfg.Input != nil && p.InChannels <= 0) {
		_ = ds.Close()
		return nil, ErrFormat
	}
	if cfg.Output == nil && p.OutChannels != 0 || cfg.Input == nil && p.InChannels != 0 {
		_ = ds.Close()
		return nil, ErrFormat
	}
	if cfg.Output != nil && p.OutChannels != cfg.OutChannels || cfg.Input != nil && p.InChannels != cfg.InChannels {
		_ = ds.Close()
		return nil, ErrFormat
	}
	inBytes, okIn := bufferSize(p.InChannels, p.Period, format.BytesPerSample(p.InFormat))
	outBytes, okOut := bufferSize(p.OutChannels, p.Period, format.BytesPerSample(p.OutFormat))
	if !okIn || !okOut {
		_ = ds.Close()
		return nil, ErrFormat
	}
	s := &Stream{
		state:  stateOpened,
		device: ds,
		cb:     cb,
		cfg:    cfg,
		actual: Actual{
			SampleRate: p.SampleRate, Period: p.Period, Periods: p.Periods,
			OutChannels: p.OutChannels, InChannels: p.InChannels,
			OutFormat: string(p.OutFormat), InFormat: string(p.InFormat),
			LatencyOut: p.LatencyOut, LatencyIn: p.LatencyIn,
			Priority: p.Priority, HasDeadline: p.HasDeadline,
		},
		closeDone: make(chan struct{}),
	}
	if p.InChannels > 0 {
		s.input = make([][]float32, p.InChannels)
		s.inData = make([]float32, p.InChannels*p.Period)
		for ch := range s.input {
			s.input[ch] = s.inData[ch*p.Period : (ch+1)*p.Period]
		}
	}
	if p.OutChannels > 0 {
		s.output = make([][]float32, p.OutChannels)
		s.outData = make([]float32, p.OutChannels*p.Period)
		for ch := range s.output {
			s.output[ch] = s.outData[ch*p.Period : (ch+1)*p.Period]
		}
	}
	// Force the backing pages into memory before Start. This is a touch, not a
	// guarantee that future stack or page growth cannot occur.
	for i := range s.inData {
		s.inData[i] = 0
	}
	for i := range s.outData {
		s.outData[i] = 0
	}
	_ = inBytes
	_ = outBytes
	return s, nil
}

func findDevice(devices []driver.Info, id string, dir Direction) (driver.Info, bool) {
	for _, d := range devices {
		if d.ID != id {
			continue
		}
		if dir == Output && d.Outputs == 0 || dir == Input && d.Inputs == 0 {
			return driver.Info{}, false
		}
		return d, true
	}
	return driver.Info{}, false
}

func validateParams(p driver.Params, dirs Direction) error {
	if p.SampleRate <= 0 || p.Period <= 0 || p.Periods <= 0 {
		return ErrFormat
	}
	if dirs&Output != 0 && format.BytesPerSample(p.OutFormat) == 0 || dirs&Input != 0 && format.BytesPerSample(p.InFormat) == 0 {
		return ErrFormat
	}
	if p.OutChannels < 0 || p.InChannels < 0 {
		return ErrFormat
	}
	return nil
}

func bufferSize(channels, frames, bytesPerSample int) (int, bool) {
	if channels == 0 {
		return 0, true
	}
	maxInt := int(^uint(0) >> 1)
	if frames <= 0 || bytesPerSample <= 0 || channels > maxInt/frames {
		return 0, false
	}
	values := channels * frames
	if values > maxInt/bytesPerSample {
		return 0, false
	}
	return values * bytesPerSample, true
}

// Actual returns the negotiated device parameters.
func (s *Stream) Actual() Actual {
	if s == nil {
		return Actual{}
	}
	s.mu.Lock()
	a := s.actual
	s.mu.Unlock()
	return a
}

// Start starts the stream once and returns after the backend has started or
// failed. The callback runs on a locked operating-system thread.
func (s *Stream) Start() error {
	if s == nil {
		return ErrState
	}
	s.mu.Lock()
	if s.state != stateOpened || s.closing {
		s.mu.Unlock()
		return ErrState
	}
	s.state = stateRunning
	s.started = true
	s.watchdogStop = rt.StartWatchdog(time.Second, func(allocs uint64) {
		atomicSaturatingAdd(&s.stats.allocs, allocs)
	})
	s.startDone = make(chan struct{})
	s.done = make(chan struct{})
	startDone := s.startDone
	go s.run()
	s.mu.Unlock()
	<-startDone
	s.mu.Lock()
	err := s.startErr
	s.mu.Unlock()
	return err
}

// Stop interrupts a blocked driver wait and waits for the stream thread to
// exit. It is idempotent after the stream has stopped or failed.
func (s *Stream) Stop() error {
	if s == nil {
		return ErrState
	}
	s.mu.Lock()
	if s.closing || s.state == stateClosed {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		return ErrState
	}
	if !s.started {
		s.mu.Unlock()
		return ErrState
	}
	if s.state == stateStopped || s.state == stateFailed {
		s.mu.Unlock()
		return nil
	}
	if s.state != stateRunning && s.state != stateStopping {
		s.mu.Unlock()
		return ErrState
	}
	s.state = stateStopping
	s.stopRequested.Store(true)
	interrupt := !s.interrupt
	s.interrupt = true
	done := s.done
	s.mu.Unlock()
	if interrupt {
		s.device.Interrupt()
	}
	<-done
	return nil
}

// Close interrupts a running stream, waits for its thread, and closes the
// backend. Close is safe to call more than once.
func (s *Stream) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closing {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	if s.state == stateClosed {
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	s.closing = true
	started := s.started
	interrupt := false
	if started && s.state != stateStopped && s.state != stateFailed {
		s.state = stateStopping
		s.stopRequested.Store(true)
		if !s.interrupt {
			s.interrupt = true
			interrupt = true
		}
	}
	done := s.done
	s.mu.Unlock()
	if interrupt {
		s.device.Interrupt()
	}
	if started {
		<-done
	}
	closeErr := s.device.Close()
	s.mu.Lock()
	s.closeErr = closeErr
	s.state = stateClosed
	close(s.closeDone)
	s.mu.Unlock()
	return closeErr
}

// Err reports the first spontaneous failure that stopped the stream.
func (s *Stream) Err() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	err := s.terminalErr
	s.mu.Unlock()
	return err
}

// Stats copies the current counters and timing histograms into dst. Passing
// nil is a no-op.
func (s *Stream) Stats(dst *Stats) {
	if s == nil || dst == nil {
		return
	}
	dst.Callbacks = s.stats.callbacks.Load()
	dst.Dropouts = s.stats.dropouts.Load()
	dst.Late = s.stats.late.Load()
	dst.WakeLate = s.stats.wakeLate.Snapshot()
	dst.WakeInterval = s.stats.wakeInterval.Snapshot()
	dst.CallbackTime = s.stats.callbackTime.Snapshot()
	dst.WakeLateMax = time.Duration(s.stats.wakeLateMax.Load())
	dst.CallbackMax = time.Duration(s.stats.callbackMax.Load())
	dst.AllocsSinceRun = s.stats.allocs.Load() // Process-wide; not attributed to this stream's goroutine.
}

func (s *Stream) run() {
	runtime.LockOSThread()
	rt.PreGrowStack()
	period := time.Duration(int64(s.actual.Period) * int64(time.Second) / int64(s.actual.SampleRate))
	grant := rt.Raise(period)
	s.mu.Lock()
	s.actual.Priority = grant.String()
	s.mu.Unlock()
	if err := s.device.Start(); err != nil {
		s.mu.Lock()
		s.startErr = err
		s.mu.Unlock()
		s.setFailure(err)
		_ = s.device.Stop()
		rt.Lower(grant)
		close(s.startDone)
		s.finishRun()
		return
	}
	close(s.startDone)
	s.loop()
	stopErr := s.device.Stop()
	if stopErr != nil {
		s.setFailure(stopErr)
	}
	rt.Lower(grant)
	s.finishRun()
}

func (s *Stream) finishRun() {
	s.mu.Lock()
	watchdogStop := s.watchdogStop
	s.watchdogStop = nil
	s.mu.Unlock()
	if watchdogStop != nil {
		watchdogStop()
	}
	s.mu.Lock()
	if s.terminalErr != nil {
		s.state = stateFailed
	} else {
		s.state = stateStopped
	}
	close(s.done)
	s.mu.Unlock()
}

func (s *Stream) setFailure(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	if s.terminalErr == nil {
		s.terminalErr = err
	}
	s.mu.Unlock()
}

func (s *Stream) loop() {
	periodDur := time.Duration(int64(s.actual.Period) * int64(time.Second) / int64(s.actual.SampleRate))
	periodNanos := int64(periodDur)
	lastDropouts := s.device.Dropouts()
	var frame uint64
	var previousWake int64
	var deadline int64
	deadlineSet := false
	discontinuity := false
	for {
		if s.stopRequested.Load() {
			return
		}
		waitErr := s.device.Wait()
		currentDropouts := s.device.Dropouts()
		if currentDropouts >= lastDropouts {
			if delta := currentDropouts - lastDropouts; delta != 0 {
				atomicSaturatingAdd(&s.stats.dropouts, delta)
				discontinuity = true
			}
		} else if currentDropouts != 0 {
			atomicSaturatingAdd(&s.stats.dropouts, currentDropouts)
			discontinuity = true
		}
		lastDropouts = currentDropouts
		if errors.Is(waitErr, driver.ErrInterrupted) && s.stopRequested.Load() {
			return
		}
		if errors.Is(waitErr, driver.ErrXrun) {
			if err := s.device.Recover(); err != nil {
				s.setFailure(err)
				return
			}
			discontinuity = true
			continue
		}
		if errors.Is(waitErr, driver.ErrLost) {
			s.setFailure(ErrDeviceLost)
			return
		}
		if waitErr != nil {
			s.setFailure(waitErr)
			return
		}
		if s.stopRequested.Load() {
			return
		}
		woke := rt.Now()
		if previousWake != 0 {
			s.stats.wakeInterval.Record(time.Duration(woke - previousWake))
		}
		previousWake = woke
		if s.actual.HasDeadline {
			if !deadlineSet {
				deadline = woke + periodNanos
				deadlineSet = true
			} else {
				d := woke - deadline
				if d > 0 {
					s.stats.wakeLate.Record(time.Duration(d))
					atomicSaturatingMax(&s.stats.wakeLateMax, d)
				}
			}
		}
		inRaw, outRaw := s.device.Buffers()
		if len(s.input) > 0 {
			if err := format.DecodeInterleaved(s.input, inRaw, format.Format(s.actual.InFormat)); err != nil {
				s.setFailure(err)
				return
			}
		}
		for ch := range s.output {
			for i := range s.output[ch] {
				s.output[ch][i] = 0
			}
		}
		outNano, inNano := s.device.Clock()
		dropouts := s.stats.dropouts.Load()
		if dropouts > math.MaxUint32 {
			dropouts = math.MaxUint32
		}
		t := Time{
			Frame: frame, OutputNano: outNano, InputNano: inNano,
			Dropouts: uint32(dropouts), Discontinuity: discontinuity,
		}
		atomicSaturatingAdd(&s.stats.callbacks, 1)
		callbackStart := rt.Now()
		panicked := callCallback(s.cb, t, s.input, s.output)
		callbackEnd := rt.Now()
		callbackDur := callbackEnd - callbackStart
		if callbackDur < 0 {
			callbackDur = 0
		}
		s.stats.callbackTime.Record(time.Duration(callbackDur))
		atomicSaturatingMax(&s.stats.callbackMax, callbackDur)
		frame += uint64(s.actual.Period)
		if panicked {
			for ch := range s.output {
				for i := range s.output[ch] {
					s.output[ch][i] = 0
				}
			}
			if len(s.output) > 0 {
				_ = format.EncodeInterleaved(outRaw, s.output, format.Format(s.actual.OutFormat))
			}
			_ = s.device.Commit()
			s.setFailure(ErrCallback)
			return
		}
		if len(s.output) > 0 {
			if err := format.EncodeInterleaved(outRaw, s.output, format.Format(s.actual.OutFormat)); err != nil {
				s.setFailure(err)
				return
			}
		}
		commitErr := s.device.Commit()
		if errors.Is(commitErr, driver.ErrXrun) {
			current := s.device.Dropouts()
			if current > lastDropouts {
				atomicSaturatingAdd(&s.stats.dropouts, current-lastDropouts)
				lastDropouts = current
			}
			if err := s.device.Recover(); err != nil {
				s.setFailure(err)
				return
			}
			discontinuity = true
			continue
		}
		if commitErr != nil {
			s.setFailure(commitErr)
			return
		}
		if s.actual.HasDeadline {
			committed := rt.Now()
			if committed > deadline {
				atomicSaturatingAdd(&s.stats.late, 1)
			}
			deadline += periodNanos
		}
		discontinuity = false
	}
}

func callCallback(cb Callback, t Time, in, out [][]float32) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	cb(t, in, out)
	return false
}

func atomicSaturatingAdd(v *atomic.Uint64, delta uint64) {
	for {
		old := v.Load()
		if old == math.MaxUint64 {
			return
		}
		next := old + delta
		if next < old {
			next = math.MaxUint64
		}
		if v.CompareAndSwap(old, next) {
			return
		}
	}
}

func atomicSaturatingMax(v *atomic.Int64, candidate int64) {
	for {
		old := v.Load()
		if candidate <= old || v.CompareAndSwap(old, candidate) {
			return
		}
	}
}
