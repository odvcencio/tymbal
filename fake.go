package tymbal

import (
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"m31labs.dev/tymbal/internal/driver"
	"m31labs.dev/tymbal/internal/format"
)

const fakeRecordCapacity = 1 << 20

// FakeConfig controls the deterministic in-process host used by tests and
// conformance tools.
type FakeConfig struct {
	Manual   bool
	Loopback bool
	Delay    int
	Devices  []Device
}

// FakeControl controls a FakeHost and snapshots its recorded output.
type FakeControl struct{ state *fakeState }

type fakeState struct {
	mu         sync.Mutex
	advanceMu  sync.Mutex
	cfg        FakeConfig
	devices    []driver.Info
	started    bool
	ended      bool
	claimed    bool
	error      error
	requested  uint64
	completed  uint64
	generation uint64
	changed    chan struct{}
	tokens     chan struct{}
	interrupt  chan struct{}
	intOnce    sync.Once

	dropouts map[int]bool
	stalls   map[int]time.Duration

	recorded []float32
	recLen   int
}

type fakeDriver struct{ state *fakeState }

type fakeStream struct {
	state  *fakeState
	params driver.Params
	manual bool
	period time.Duration

	in  []byte
	out []byte

	loopback      []byte
	loopbackValid []bool
	loopbackSlots int
	periodIndex   int
	pendingToken  bool
	faultReported bool
	dropInput     bool
	virtualNano   int64
	dropouts      atomic.Uint64
	autoTicker    *time.Ticker
	closed        bool
}

var errFakeStopped = errors.New("tymbal fake: stream stopped")

// NewFakeHost creates a public fake Host and its deterministic controller.
func NewFakeHost(cfg FakeConfig) (Host, *FakeControl) {
	if cfg.Delay < 0 {
		cfg.Delay = 0
	}
	if cfg.Delay > 4096 {
		cfg.Delay = 4096
	}
	if len(cfg.Devices) == 0 {
		cfg.Devices = []Device{{
			ID: "fake-default", Name: "Tymbal Fake Device",
			Inputs: 2, Outputs: 2, SampleRates: []int{48000},
			MinPeriod: 1, MaxPeriod: 8192,
			Default: Input | Output, Exclusive: true,
		}}
	}
	state := &fakeState{
		cfg: cfg, changed: make(chan struct{}),
		tokens: make(chan struct{}, 64), interrupt: make(chan struct{}),
		dropouts: make(map[int]bool), stalls: make(map[int]time.Duration),
		recorded: make([]float32, fakeRecordCapacity),
	}
	state.devices = make([]driver.Info, len(cfg.Devices))
	for i, d := range cfg.Devices {
		state.devices[i] = driver.Info{
			ID: d.ID, Name: d.Name, Inputs: d.Inputs, Outputs: d.Outputs,
			SampleRates: append([]int(nil), d.SampleRates...),
			MinPeriod:   d.MinPeriod, MaxPeriod: d.MaxPeriod,
			Default: uint8(d.Default), Exclusive: d.Exclusive,
		}
	}
	return Host{d: &fakeDriver{state: state}}, &FakeControl{state: state}
}

// Advance releases exactly periods manual periods and waits until those
// periods have been committed, or the stream stops.
func (c *FakeControl) Advance(periods int) error {
	if c == nil || c.state == nil || periods <= 0 {
		return ErrState
	}
	f := c.state
	f.advanceMu.Lock()
	defer f.advanceMu.Unlock()
	f.mu.Lock()
	if !f.cfg.Manual || !f.started || f.ended {
		f.mu.Unlock()
		return ErrState
	}
	target := f.requested + uint64(periods)
	f.requested = target
	generation := f.generation
	tokens := f.tokens
	interrupt := f.interrupt
	f.mu.Unlock()
	for i := 0; i < periods; i++ {
		select {
		case tokens <- struct{}{}:
		case <-interrupt:
			return ErrState
		}
	}
	for {
		f.mu.Lock()
		if f.generation != generation {
			f.mu.Unlock()
			return ErrState
		}
		if f.completed >= target {
			f.mu.Unlock()
			return nil
		}
		if f.ended {
			err := f.error
			f.mu.Unlock()
			if err != nil {
				return err
			}
			return errFakeStopped
		}
		changed := f.changed
		f.mu.Unlock()
		select {
		case <-changed:
		case <-interrupt:
			f.mu.Lock()
			complete := f.completed >= target
			f.mu.Unlock()
			if complete {
				return nil
			}
			return errFakeStopped
		}
	}
}

// InjectStall delays the selected callback period in virtual time. A stall
// longer than one period also reports an xrun.
func (c *FakeControl) InjectStall(atPeriod int, d time.Duration) {
	if c == nil || c.state == nil || atPeriod < 0 || d < 0 {
		return
	}
	c.state.mu.Lock()
	c.state.stalls[atPeriod] = d
	c.state.mu.Unlock()
}

// InjectDropout drops loopback capture for the selected callback period and
// reports one recoverable xrun.
func (c *FakeControl) InjectDropout(atPeriod int) {
	if c == nil || c.state == nil || atPeriod < 0 {
		return
	}
	c.state.mu.Lock()
	c.state.dropouts[atPeriod] = true
	c.state.mu.Unlock()
}

// Recorded returns a copy of all output samples recorded so far. Samples are
// interleaved when the stream has multiple output channels.
func (c *FakeControl) Recorded() []float32 {
	if c == nil || c.state == nil {
		return nil
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	return append([]float32(nil), c.state.recorded[:c.state.recLen]...)
}

func (d *fakeDriver) Name() string { return "fake" }

func (d *fakeDriver) Devices() ([]driver.Info, error) {
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	devices := make([]driver.Info, len(d.state.devices))
	for i, info := range d.state.devices {
		devices[i] = info
		devices[i].SampleRates = append([]int(nil), info.SampleRates...)
	}
	return devices, nil
}

func (d *fakeDriver) Default(dir uint8) (driver.Info, error) {
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	for _, info := range d.state.devices {
		if info.Default&dir != 0 {
			if dir == uint8(Output) && info.Outputs > 0 || dir == uint8(Input) && info.Inputs > 0 {
				info.SampleRates = append([]int(nil), info.SampleRates...)
				return info, nil
			}
		}
	}
	return driver.Info{}, ErrUnsupported
}

func (d *fakeDriver) Watch(_ func(driver.Event)) func() { return func() {} }

func (d *fakeDriver) Open(req driver.Request) (driver.Stream, error) {
	if req.SampleRate <= 0 || req.Period <= 0 || req.Periods <= 0 {
		return nil, ErrState
	}
	var output, input *driver.Info
	for i := range d.state.devices {
		info := &d.state.devices[i]
		if req.Output != nil && info.ID == req.Output.ID && info.Outputs > 0 {
			output = info
		}
		if req.Input != nil && info.ID == req.Input.ID && info.Inputs > 0 {
			input = info
		}
	}
	if req.Output != nil && output == nil || req.Input != nil && input == nil {
		return nil, ErrState
	}
	if req.Output == nil && req.OutChannels != 0 || req.Input == nil && req.InChannels != 0 {
		return nil, ErrState
	}
	if output != nil && (req.OutChannels <= 0 || req.OutChannels > output.Outputs) || input != nil && (req.InChannels <= 0 || req.InChannels > input.Inputs) {
		return nil, ErrFormat
	}
	if (output != nil && !supportsRate(output.SampleRates, req.SampleRate)) || (input != nil && !supportsRate(input.SampleRates, req.SampleRate)) {
		return nil, ErrFormat
	}
	if output != nil && (output.MinPeriod > req.Period || output.MaxPeriod > 0 && output.MaxPeriod < req.Period) || input != nil && (input.MinPeriod > req.Period || input.MaxPeriod > 0 && input.MaxPeriod < req.Period) {
		return nil, ErrFormat
	}
	d.state.mu.Lock()
	if d.state.claimed || d.state.started && !d.state.ended {
		d.state.mu.Unlock()
		return nil, ErrBusy
	}
	if d.state.ended {
		d.state.started = false
		d.state.ended = false
		d.state.error = nil
		d.state.requested = 0
		d.state.completed = 0
		d.state.generation++
		d.state.changed = make(chan struct{})
		d.state.tokens = make(chan struct{}, 64)
		d.state.interrupt = make(chan struct{})
		d.state.intOnce = sync.Once{}
		clear(d.state.dropouts)
		clear(d.state.stalls)
	}
	d.state.claimed = true
	d.state.mu.Unlock()
	p := driver.Params{
		SampleRate: req.SampleRate, Period: req.Period, Periods: req.Periods,
		OutChannels: req.OutChannels, InChannels: req.InChannels,
		Priority: "normal",
	}
	if output != nil {
		p.OutFormat = format.F32LE
	}
	if input != nil {
		p.InFormat = format.F32LE
		p.LatencyIn = frameDurationProduct(d.state.cfg.Delay, req.Period, req.SampleRate)
	}
	period := frameDuration(req.Period, req.SampleRate)
	if period <= 0 {
		period = time.Nanosecond
	}
	outSize, ok := bufferSize(req.OutChannels, req.Period, format.BytesPerSample(p.OutFormat))
	if !ok {
		d.state.mu.Lock()
		d.state.claimed = false
		d.state.mu.Unlock()
		return nil, ErrFormat
	}
	inSize, ok := bufferSize(req.InChannels, req.Period, format.BytesPerSample(p.InFormat))
	if !ok {
		d.state.mu.Lock()
		d.state.claimed = false
		d.state.mu.Unlock()
		return nil, ErrFormat
	}
	slots := d.state.cfg.Delay + 2
	if slots < 2 {
		slots = 2
	}
	fs := &fakeStream{
		state: d.state, params: p, manual: d.state.cfg.Manual, period: period,
		in: make([]byte, inSize), out: make([]byte, outSize),
		loopbackSlots: slots, loopbackValid: make([]bool, slots),
		virtualNano: int64(p.LatencyIn) + int64(period) + 1,
	}
	if req.Output != nil && req.Input != nil && d.state.cfg.Loopback && output != nil && input != nil {
		fs.loopback = make([]byte, slots*outSize)
	}
	if !fs.manual {
		fs.autoTicker = time.NewTicker(period)
		fs.autoTicker.Stop()
	}
	return fs, nil
}

func supportsRate(rates []int, rate int) bool {
	if len(rates) == 0 {
		return true
	}
	for _, r := range rates {
		if r == rate {
			return true
		}
	}
	return false
}

func frameDuration(frames, rate int) time.Duration {
	if frames <= 0 || rate <= 0 {
		return 0
	}
	// Round to the nearest nanosecond so converting the reported duration back
	// to frames does not lose one frame for rates such as 48 kHz.
	return roundedDuration(float64(frames) / float64(rate) * float64(time.Second))
}

func frameDurationProduct(a, b, rate int) time.Duration {
	if a <= 0 || b <= 0 || rate <= 0 {
		return 0
	}
	return roundedDuration(float64(a) * float64(b) / float64(rate) * float64(time.Second))
}

func roundedDuration(nanos float64) time.Duration {
	if nanos >= float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(math.Round(nanos))
}

func (s *fakeStream) Params() driver.Params { return s.params }

func (s *fakeStream) Start() error {
	s.state.mu.Lock()
	if s.closed || s.state.ended || s.state.started {
		s.state.mu.Unlock()
		return ErrState
	}
	s.state.started = true
	s.state.notifyLocked()
	s.state.mu.Unlock()
	if !s.manual {
		s.autoTicker.Reset(s.period)
	}
	return nil
}

func (s *fakeStream) Wait() error {
	if !s.pendingToken {
		if s.manual {
			select {
			case <-s.state.tokens:
				s.pendingToken = true
			case <-s.state.interrupt:
				return driver.ErrInterrupted
			}
		} else {
			select {
			case <-s.autoTicker.C:
				s.pendingToken = true
			case <-s.state.interrupt:
				return driver.ErrInterrupted
			}
		}
	}
	if !s.faultReported {
		s.state.mu.Lock()
		stall, stalled := s.state.stalls[s.periodIndex]
		dropout := s.state.dropouts[s.periodIndex]
		delete(s.state.stalls, s.periodIndex)
		delete(s.state.dropouts, s.periodIndex)
		s.state.mu.Unlock()
		if stalled {
			s.virtualNano += int64(stall)
		}
		if dropout || stalled && stall > s.period {
			atomicSaturatingAdd(&s.dropouts, 1)
			s.dropInput = true
			s.faultReported = true
			return driver.ErrXrun
		}
	}
	s.faultReported = false
	s.fillInput()
	return nil
}

func (s *fakeStream) fillInput() {
	for i := range s.in {
		s.in[i] = 0
	}
	if s.dropInput || len(s.loopback) == 0 || len(s.in) == 0 || s.params.OutChannels == 0 {
		s.dropInput = false
		return
	}
	sourcePeriod := s.periodIndex - s.state.cfg.Delay
	if sourcePeriod < 0 {
		return
	}
	slot := sourcePeriod % s.loopbackSlots
	if !s.loopbackValid[slot] {
		return
	}
	source := s.loopback[slot*len(s.out) : (slot+1)*len(s.out)]
	frames := s.params.Period
	for frame := 0; frame < frames; frame++ {
		for ch := 0; ch < s.params.InChannels; ch++ {
			if ch >= s.params.OutChannels {
				continue
			}
			src := (frame*s.params.OutChannels + ch) * 4
			dst := (frame*s.params.InChannels + ch) * 4
			copy(s.in[dst:dst+4], source[src:src+4])
		}
	}
}

func (s *fakeStream) Interrupt() {
	s.state.intOnce.Do(func() { close(s.state.interrupt) })
}

func (s *fakeStream) Buffers() (in, out []byte) { return s.in, s.out }

func (s *fakeStream) Commit() error {
	if s.closed || !s.pendingToken {
		return ErrState
	}
	if len(s.out) > 0 {
		slot := s.periodIndex % s.loopbackSlots
		if len(s.loopback) > 0 {
			copy(s.loopback[slot*len(s.out):(slot+1)*len(s.out)], s.out)
			s.loopbackValid[slot] = true
		}
		s.state.mu.Lock()
		for off := 0; off+4 <= len(s.out) && s.state.recLen < len(s.state.recorded); off += 4 {
			s.state.recorded[s.state.recLen] = math.Float32frombits(binary.LittleEndian.Uint32(s.out[off : off+4]))
			s.state.recLen++
		}
		s.state.mu.Unlock()
	}
	s.pendingToken = false
	s.dropInput = false
	s.periodIndex++
	s.virtualNano += int64(s.period)
	s.state.mu.Lock()
	s.state.completed++
	s.state.notifyLocked()
	s.state.mu.Unlock()
	return nil
}

func (s *fakeStream) Clock() (outNano, inNano int64) {
	outNano = s.virtualNano
	if s.params.InChannels > 0 {
		inNano = outNano - int64(s.params.LatencyIn)
	}
	return outNano, inNano
}

// FakeHost has a virtual clock but no OS device schedule against which to
// measure wake or commit lateness.
func (s *fakeStream) Deadlines() (wakeNano, commitNano int64) { return 0, 0 }

func (s *fakeStream) Dropouts() uint64 { return s.dropouts.Load() }

func (s *fakeStream) Recover() error { return nil }

func (s *fakeStream) Stop() error {
	s.Interrupt()
	if s.autoTicker != nil {
		s.autoTicker.Stop()
	}
	s.state.mu.Lock()
	if !s.state.ended {
		s.state.ended = true
		s.state.notifyLocked()
	}
	s.state.mu.Unlock()
	return nil
}

func (s *fakeStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.autoTicker != nil {
		s.autoTicker.Stop()
	}
	s.state.mu.Lock()
	s.state.claimed = false
	if !s.state.ended {
		s.state.ended = true
		s.state.notifyLocked()
	}
	s.state.mu.Unlock()
	return nil
}

func (s *fakeState) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

var _ driver.Driver = (*fakeDriver)(nil)
var _ driver.Stream = (*fakeStream)(nil)
