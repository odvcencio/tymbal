//go:build darwin && (amd64 || arm64)

package coreaudio

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
	"m31labs.dev/tymbal/internal/format"
	"m31labs.dev/tymbal/internal/ring"
	"m31labs.dev/tymbal/internal/rt"
)

const (
	bytesPerSample = 4 // the stream is interleaved Float32 (format.F32LE)

	// waitSlice bounds one semaphore wait. Render callbacks arrive every few
	// milliseconds, so a wait that runs this long is a stalled device; each
	// timeout asks Core Audio whether the device is still there.
	waitSlice = 250 * time.Millisecond

	// Largest geometry newStream accepts: 1M frames of ring (23 s at 44.1 kHz),
	// 64 channels, and 64 MiB of ring in all.
	maxRingFrames = 1 << 20
	maxChannels   = 64
	maxRingBytes  = 64 << 20
)

var (
	errState        = errors.New("coreaudio: invalid stream state")
	errWaitCommit   = errors.New("coreaudio: Wait before committing the previous period")
	errRingGeometry = fmt.Errorf("%w: ring geometry out of range", driver.ErrFormat)
)

// streamConfig is the negotiated geometry newStream builds a stream from.
type streamConfig struct {
	rate       int
	channels   int
	period     int // frames per Go render period
	fill       int // frames kept queued ahead of Core Audio; a multiple of period
	pipeFrames int // frames between the ring and the speaker: device buffer, latencies, safety offset
}

// stream is one output stream: a ring in an arena, a Mach semaphore, and,
// when attached, an AUHAL output unit whose render callback is the assembly
// trampoline. Wait, Buffers, Commit, Clock, Dropouts and Recover run on the
// stream thread. Interrupt may run anywhere. Close must not overlap anything
// but Interrupt.
type stream struct {
	params driver.Params

	arena *ring.Arena
	c     *ctrl  // in arena
	data  []byte // ring storage, in arena: (mask+1) * frameBytes
	out   []byte // one period, staged by the caller and copied into the ring by Commit
	head  uint64 // frames published; Go is the only writer of c.head, so this is its shadow

	frameBytes  int
	periodBytes int
	periodMax   uint64 // period, in frames
	fillMax     uint64 // fill, in frames
	pipeFrames  int
	rate        int

	task uint32 // the task port semaphores belong to
	sem  uint32 // read by Wait on the stream thread; zeroed by Close

	device      uint32  // AudioObjectID, 0 when no unit is attached
	unit        uintptr // AUHAL AudioUnit, 0 when none
	initialized bool    // AudioUnitInitialize succeeded

	wakeMu      sync.Mutex // orders Interrupt's semaphore_signal against Close's semaphore_destroy
	interrupted atomic.Bool

	mu      sync.Mutex // lifecycle: Start, Stop, Close
	started atomic.Bool
	stopped atomic.Bool
	closed  atomic.Bool
	ready   bool // Wait succeeded and Commit has not run

	finalUnder uint64 // Dropouts as of Close, which unmaps the counters
}

var _ driver.Stream = (*stream)(nil)

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// newStream builds the ring, the control block and the semaphore. It starts
// nothing and talks to no audio device, so the whole producer/consumer
// protocol can run against a stand-in for Core Audio (see render_darwin_test.go).
func newStream(cfg streamConfig) (*stream, error) {
	if cfg.rate <= 0 || cfg.channels <= 0 || cfg.channels > maxChannels ||
		cfg.period <= 0 || cfg.fill < cfg.period || cfg.fill%cfg.period != 0 || cfg.pipeFrames < 0 {
		return nil, errRingGeometry
	}
	capacity := nextPow2(cfg.fill)
	frameBytes := cfg.channels * bytesPerSample
	if capacity > maxRingFrames || capacity*frameBytes > maxRingBytes {
		return nil, errRingGeometry
	}
	ctrlSize := int(unsafe.Sizeof(ctrl{}))
	arena, err := ring.NewArena(ctrlSize + 64 + capacity*frameBytes + 64)
	if err != nil {
		return nil, fmt.Errorf("coreaudio: ring arena: %w", err)
	}
	// The render thread must not take a page fault. Wiring is best effort: the
	// pages were touched by NewArena, and a locked-memory limit only costs
	// the guarantee, not the stream.
	_ = arena.Lock()
	ctrlMem := arena.Take(ctrlSize, 64)
	data := arena.Take(capacity*frameBytes, 64)

	s := &stream{
		arena:       arena,
		c:           (*ctrl)(unsafe.Pointer(&ctrlMem[0])),
		data:        data,
		out:         make([]byte, cfg.period*frameBytes),
		frameBytes:  frameBytes,
		periodBytes: cfg.period * frameBytes,
		periodMax:   uint64(cfg.period),
		fillMax:     uint64(cfg.fill),
		pipeFrames:  cfg.pipeFrames,
		rate:        cfg.rate,
		task:        machTaskSelf(),
	}
	if err := check("semaphore_create", semaphoreCreate(s.task, &s.sem)); err != nil {
		_ = arena.Close()
		return nil, err
	}
	c := s.c
	c.mask = uint64(capacity - 1)
	c.fbytes = uint64(frameBytes)
	c.data = uintptr(unsafe.Pointer(&data[0]))
	c.sem = uint64(s.sem)
	c.signal = fnsemaphore_signal
	c.memcpy = fnmemcpy
	c.memset = fnmemset
	c.sigmask = fnpthread_sigmask
	c.sigset = uint64(blockedSignals)

	s.params = driver.Params{
		SampleRate: cfg.rate, Period: cfg.period, Periods: cfg.fill / cfg.period,
		OutChannels: cfg.channels, OutFormat: format.F32LE,
		LatencyOut: time.Duration(int64(cfg.fill+cfg.pipeFrames) * int64(time.Second) / int64(cfg.rate)),
	}
	return s, nil
}

// streamDescription is AudioStreamBasicDescription.
type streamDescription struct {
	sampleRate                                                                              float64
	formatID, formatFlags, bytesPerPacket, framesPerPacket, bytesPerFrame, channelsPerFrame uint32
	bitsPerChannel, reserved                                                                uint32
}

// renderCallback is AURenderCallbackStruct.
type renderCallback struct{ proc, refcon uintptr }

const (
	unitTypeOutput   = 'a'<<24 | 'u'<<16 | 'o'<<8 | 'u' // kAudioUnitType_Output
	unitSubTypeHAL   = 'a'<<24 | 'h'<<16 | 'a'<<8 | 'l' // kAudioUnitSubType_HALOutput
	unitManufApple   = 'a'<<24 | 'p'<<16 | 'p'<<8 | 'l' // kAudioUnitManufacturer_Apple
	formatLinearPCM  = 'l'<<24 | 'p'<<16 | 'c'<<8 | 'm' // kAudioFormatLinearPCM
	formatFloatPack  = 0x1 | 0x8                        // kAudioFormatFlagIsFloat | kAudioFormatFlagIsPacked: interleaved
	propCurrentDev   = 2000                             // kAudioOutputUnitProperty_CurrentDevice
	propStreamFormat = 8                                // kAudioUnitProperty_StreamFormat
	propSetRender    = 23                               // kAudioUnitProperty_SetRenderCallback
	unitScopeGlobal  = 0
	unitScopeInput   = 1
)

// attachUnit creates the AUHAL output unit for device, gives it an interleaved
// Float32 input format at the stream's rate, installs the trampoline as its
// render callback and initializes it. It does not start it.
func (s *stream) attachUnit(device uint32) error {
	desc := componentDescription{
		componentType:         unitTypeOutput,
		componentSubType:      unitSubTypeHAL,
		componentManufacturer: unitManufApple,
	}
	comp := audioComponentFindNext(&desc)
	if comp == 0 {
		return fmt.Errorf("%w: no HAL output audio unit", driver.ErrUnsupported)
	}
	var unit uintptr
	if err := check("AudioComponentInstanceNew", audioComponentInstanceNew(comp, &unit)); err != nil {
		return err
	}
	s.unit, s.device = unit, device

	dev := device
	if err := check("AudioUnitSetProperty(CurrentDevice)",
		audioUnitSetProperty(unit, propCurrentDev, unitScopeGlobal, 0, unsafe.Pointer(&dev), 4)); err != nil {
		return err
	}
	asbd := streamDescription{
		sampleRate:       float64(s.rate),
		formatID:         formatLinearPCM,
		formatFlags:      formatFloatPack,
		bytesPerPacket:   uint32(s.frameBytes),
		framesPerPacket:  1,
		bytesPerFrame:    uint32(s.frameBytes),
		channelsPerFrame: uint32(s.params.OutChannels),
		bitsPerChannel:   bytesPerSample * 8,
	}
	if err := check("AudioUnitSetProperty(StreamFormat)",
		audioUnitSetProperty(unit, propStreamFormat, unitScopeInput, 0, unsafe.Pointer(&asbd), uint32(unsafe.Sizeof(asbd)))); err != nil {
		return err
	}
	cb := renderCallback{proc: renderABI0, refcon: uintptr(unsafe.Pointer(s.c))}
	if err := check("AudioUnitSetProperty(SetRenderCallback)",
		audioUnitSetProperty(unit, propSetRender, unitScopeInput, 0, unsafe.Pointer(&cb), uint32(unsafe.Sizeof(cb)))); err != nil {
		return err
	}
	if err := check("AudioUnitInitialize", audioUnitInitialize(unit)); err != nil {
		return err
	}
	s.initialized = true
	return nil
}

func (s *stream) Params() driver.Params { return s.params }

// Start queues Fill frames of silence, so the first render callbacks find
// data, then starts the unit. The caller's first periods are heard after it.
func (s *stream) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() || s.started.Load() || s.stopped.Load() {
		return errState
	}
	// The ring is still all zeros: this is silence.
	s.head = s.fillMax
	atomic.StoreUint64(&s.c.head, s.head)
	if s.unit != 0 {
		if err := check("AudioOutputUnitStart", audioOutputUnitStart(s.unit)); err != nil {
			s.stopped.Store(true)
			return classify(err)
		}
	}
	s.started.Store(true)
	return nil
}

// Wait blocks until the ring has room for one more period, which the render
// callback makes by consuming frames and signalling the semaphore.
//
//tymbal:rt
func (s *stream) Wait() error {
	if !s.started.Load() || s.stopped.Load() || s.closed.Load() {
		return errState
	}
	if s.ready {
		return errWaitCommit
	}
	for {
		if s.interrupted.Load() {
			return driver.ErrInterrupted
		}
		if s.fillMax-(s.head-atomic.LoadUint64(&s.c.tail)) >= s.periodMax {
			s.ready = true
			return nil
		}
		switch kr := semaphoreTimedWait(s.sem, waitSlice); kr {
		case 0, kernAborted:
			// Signalled by the render callback or by Interrupt, or a signal
			// interrupted the wait: look at the ring again.
		case kernTimedOut:
			if s.deviceGone() {
				return driver.ErrLost
			}
		default:
			return fmt.Errorf("coreaudio: semaphore_timedwait failed: kern_return_t %d", kr)
		}
	}
}

// deviceGone asks Core Audio whether the attached device has disappeared.
func (s *stream) deviceGone() bool {
	if s.device == 0 {
		return false
	}
	var alive uint32
	size := uint32(4)
	status := audioObjectGetPropertyData(s.device, selDeviceIsAlive, scopeGlobal, &size, unsafe.Pointer(&alive))
	switch status {
	case 0:
		return alive == 0
	case statusBadDevice, statusBadObject:
		return true
	}
	return false
}

// Interrupt wakes a blocked Wait, which then returns driver.ErrInterrupted.
func (s *stream) Interrupt() {
	if s == nil || s.interrupted.Swap(true) {
		return
	}
	s.wakeMu.Lock()
	if !s.closed.Load() && s.sem != 0 {
		semaphoreSignal(s.sem)
	}
	s.wakeMu.Unlock()
}

// Buffers returns the one-period staging buffer. There is no input.
//
//tymbal:rt
func (s *stream) Buffers() (in, out []byte) { return nil, s.out }

// Commit copies the staged period into the ring, in two pieces when it wraps,
// and publishes it to the render callback.
//
//tymbal:rt
func (s *stream) Commit() error {
	if !s.ready || s.closed.Load() {
		return errState
	}
	s.ready = false
	fb := uint64(s.frameBytes)
	capacity := s.c.mask + 1
	idx := s.head & s.c.mask
	first := capacity - idx
	if first > s.periodMax {
		first = s.periodMax
	}
	split := int(first * fb)
	copy(s.data[int(idx*fb):], s.out[:split])
	if split < s.periodBytes {
		copy(s.data, s.out[split:])
	}
	s.head += s.periodMax
	atomic.StoreUint64(&s.c.head, s.head)
	return nil
}

// Clock estimates when the first frame of the period about to be committed
// will be heard: the frames still queued ahead of it in the ring, plus the
// device pipeline, from now. It does not know how far into the current IO
// cycle Core Audio is, so it can be off by up to one device buffer.
//
//tymbal:rt
func (s *stream) Clock() (outNano, inNano int64) {
	if s.closed.Load() {
		return 0, 0
	}
	ahead := int64(s.head-atomic.LoadUint64(&s.c.tail)) + int64(s.pipeFrames)
	return rt.Now() + ahead*int64(time.Second)/int64(s.rate), 0
}

// Deadlines reports none: the render callback does not timestamp its wakes.
//
//tymbal:rt
func (s *stream) Deadlines() (wakeNano, commitNano int64) { return 0, 0 }

// Dropouts counts render callbacks that found less data than Core Audio asked
// for and filled the difference with silence.
//
//tymbal:rt
func (s *stream) Dropouts() uint64 {
	if s.closed.Load() {
		return s.finalUnder
	}
	return atomic.LoadUint64(&s.c.under)
}

// Recover does nothing. After an underrun the ring is simply empty, and the
// stream thread refills it a period at a time; Wait does not block while there
// is room.
//
//tymbal:rt
func (s *stream) Recover() error { return nil }

// Stop stops the unit. When AudioOutputUnitStop returns the render callback has
// finished its last call and will not run again.
//
//tymbal:rt
func (s *stream) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopLocked()
}

func (s *stream) stopLocked() error {
	if s.stopped.Swap(true) || !s.started.Load() || s.unit == 0 {
		return nil
	}
	return classify(check("AudioOutputUnitStop", audioOutputUnitStop(s.unit)))
}

// Close stops the unit if it is running, disposes it, destroys the semaphore
// and unmaps the ring. Nothing may call a stream method after it, and it must
// not run while Wait or Commit does. It is safe to call twice.
func (s *stream) Close() error {
	if s == nil {
		return nil
	}
	s.Interrupt()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil
	}
	first := s.stopLocked()
	if s.unit != 0 {
		if s.initialized {
			if err := check("AudioUnitUninitialize", audioUnitUninitialize(s.unit)); err != nil && first == nil {
				first = err
			}
			s.initialized = false
		}
		if err := check("AudioComponentInstanceDispose", audioComponentInstanceDispose(s.unit)); err != nil && first == nil {
			first = err
		}
		s.unit = 0
	}
	s.finalUnder = atomic.LoadUint64(&s.c.under)
	s.wakeMu.Lock()
	s.closed.Store(true)
	sem := s.sem
	s.sem = 0
	s.wakeMu.Unlock()
	if sem != 0 {
		if err := check("semaphore_destroy", semaphoreDestroy(s.task, sem)); err != nil && first == nil {
			first = err
		}
	}
	if err := s.arena.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

// counters snapshots the trampoline's counters.
func (s *stream) counters() (calls, under, underFrames, bad uint64) {
	c := s.c
	return atomic.LoadUint64(&c.calls), atomic.LoadUint64(&c.under),
		atomic.LoadUint64(&c.underFrames), atomic.LoadUint64(&c.bad)
}

// classify maps the OSStatus codes that mean something to a caller onto the
// driver's sentinel errors, keeping the original error in the message.
func classify(err error) error {
	status, ok := statusOf(err)
	if !ok {
		return err
	}
	switch status {
	case statusBadDevice, statusBadObject:
		return fmt.Errorf("%w: %v", driver.ErrLost, err)
	case statusHogged:
		return fmt.Errorf("%w: %v", driver.ErrBusy, err)
	case statusUnsupportedData, statusFormatNotSupport, statusInvalidPropValue:
		return fmt.Errorf("%w: %v", driver.ErrFormat, err)
	}
	return err
}
