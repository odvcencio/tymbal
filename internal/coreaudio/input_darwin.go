//go:build darwin && (amd64 || arm64)

package coreaudio

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
	"m31labs.dev/tymbal/internal/ring"
	"m31labs.dev/tymbal/internal/rt"
)

// inputCtrl is native memory. The HAL input callback never enters Go. Its
// first member shares the render ctrl layout, with producer/consumer reversed.
type inputCtrl struct {
	ctrl
	unit, render, list                                     uintptr
	maxFrames                                              uint64
	status                                                 uint64
	stampSeq, stampHead, hostTicks, sampleBits, stampFlags uint64
}

type nativeInputList struct {
	numberBuffers, pad, channels, bytes uint32
	data                                uintptr
}

func capture()

var captureABI0 uintptr

type captureStream struct {
	arena                                    *ring.Arena
	c                                        *inputCtrl
	data                                     []byte
	device                                   uint32
	rate, channels, deviceFrames, pipeFrames int
	unit                                     uintptr
	initialized                              bool
	task, sem                                uint32
	mu                                       sync.Mutex
	wakeMu                                   sync.Mutex
	started, stopped, closed, interrupted    atomic.Bool
	hostOrigin                               uint64
	rtOrigin                                 int64
	numer, denom                             uint32
	finalDrops                               uint64
}

func newCaptureStream(rate, channels, deviceFrames, pipeFrames int) (*captureStream, error) {
	if rate < 8000 || rate > 384000 || channels < 1 || channels > maxChannels || deviceFrames < 1 || deviceFrames > maxRingFrames {
		return nil, errRingGeometry
	}
	maxFrames := max(4096, deviceFrames)
	capacity := nextPow2(max(rate, 4*maxFrames))
	bytes := capacity * channels * 4
	if capacity > maxRingFrames || bytes > maxRingBytes {
		return nil, errRingGeometry
	}
	size := int(unsafe.Sizeof(inputCtrl{})) + int(unsafe.Sizeof(nativeInputList{})) + bytes + maxFrames*channels*4 + 256
	arena, err := ring.NewArena(size)
	if err != nil {
		return nil, err
	}
	_ = arena.Lock()
	cm := arena.Take(int(unsafe.Sizeof(inputCtrl{})), 64)
	lm := arena.Take(int(unsafe.Sizeof(nativeInputList{})), 64)
	data := arena.Take(bytes, 64)
	stage := arena.Take(maxFrames*channels*4, 64)
	s := &captureStream{arena: arena, c: (*inputCtrl)(unsafe.Pointer(&cm[0])), data: data, rate: rate, channels: channels, deviceFrames: deviceFrames, pipeFrames: pipeFrames, task: machTaskSelf()}
	if err = check("semaphore_create(input)", semaphoreCreate(s.task, &s.sem)); err != nil {
		_ = arena.Close()
		return nil, err
	}
	c := s.c
	c.mask = uint64(capacity - 1)
	c.fbytes = uint64(channels * 4)
	c.data = uintptr(unsafe.Pointer(&data[0]))
	semaphore := s.sem
	c.sem = uint64(semaphore)
	c.signal = fnsemaphore_signal
	c.memcpy = fnmemcpy
	c.memset = fnmemset
	c.sigmask = fnpthread_sigmask
	c.sigset = uint64(blockedSignals)
	c.maxFrames = uint64(maxFrames)
	c.render = fnAudioUnitRender
	c.list = uintptr(unsafe.Pointer(&lm[0]))
	list := (*nativeInputList)(unsafe.Pointer(&lm[0]))
	list.numberBuffers = 1
	list.channels = uint32(channels)
	list.bytes = uint32(len(stage))
	list.data = uintptr(unsafe.Pointer(&stage[0]))
	s.numer, s.denom = machTimebase()
	s.hostOrigin = machAbsoluteTime()
	s.rtOrigin = rt.Now()
	return s, nil
}

func (s *captureStream) attach(device uint32) error {
	desc := componentDescription{componentType: unitTypeOutput, componentSubType: unitSubTypeHAL, componentManufacturer: unitManufApple}
	component := audioComponentFindNext(&desc)
	if component == 0 {
		return fmt.Errorf("%w: no HAL input audio unit", driver.ErrUnsupported)
	}
	if err := check("AudioComponentInstanceNew(input)", audioComponentInstanceNew(component, &s.unit)); err != nil {
		return err
	}
	s.device = device
	s.c.unit = s.unit
	set := func(property, scope, element, value uint32) error {
		return check("AudioUnitSetProperty(input)", audioUnitSetProperty(s.unit, property, scope, element, unsafe.Pointer(&value), 4))
	}
	// Separate units allow a microphone and speakers with different device IDs.
	if err := set(2003, unitScopeInput, 1, 1); err != nil {
		return err
	} // EnableIO input bus
	if err := set(2003, 2, 0, 0); err != nil {
		return err
	} // disable output bus
	if err := set(propCurrentDev, unitScopeGlobal, 0, device); err != nil {
		return err
	}
	asbd := streamDescription{sampleRate: float64(s.rate), formatID: formatLinearPCM, formatFlags: formatFloatPack, bytesPerPacket: uint32(s.channels * 4), framesPerPacket: 1, bytesPerFrame: uint32(s.channels * 4), channelsPerFrame: uint32(s.channels), bitsPerChannel: 32}
	if err := check("AudioUnitSetProperty(input format)", audioUnitSetProperty(s.unit, propStreamFormat, 2, 1, unsafe.Pointer(&asbd), uint32(unsafe.Sizeof(asbd)))); err != nil {
		return err
	}
	if err := set(14, unitScopeGlobal, 0, uint32(s.c.maxFrames)); err != nil {
		return err
	} // MaximumFramesPerSlice
	// We provide the native staging buffer; AUHAL must not allocate it in RT.
	if err := set(51, 2, 1, 0); err != nil {
		return err
	} // ShouldAllocateBuffer
	cb := renderCallback{proc: captureABI0, refcon: uintptr(unsafe.Pointer(s.c))}
	if err := check("AudioUnitSetProperty(input callback)", audioUnitSetProperty(s.unit, 2005, unitScopeGlobal, 0, unsafe.Pointer(&cb), uint32(unsafe.Sizeof(cb)))); err != nil {
		return err
	}
	if err := check("AudioUnitInitialize(input)", audioUnitInitialize(s.unit)); err != nil {
		return err
	}
	s.initialized = true
	return nil
}

func (s *captureStream) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() || s.stopped.Load() || s.started.Load() {
		return errState
	}
	if s.unit != 0 {
		if err := check("AudioOutputUnitStart(input)", audioOutputUnitStart(s.unit)); err != nil {
			return classify(err)
		}
	}
	s.started.Store(true)
	return nil
}

func (s *captureStream) wait(required uint64) error {
	for {
		if s.interrupted.Load() {
			return driver.ErrInterrupted
		}
		if s.closed.Load() || s.stopped.Load() {
			return errState
		}
		if status := atomic.LoadUint64(&s.c.status); status != 0 {
			return classify(check("AudioUnitRender(input)", int32(status)))
		}
		if atomic.LoadUint64(&s.c.head) >= required {
			return nil
		}
		switch kr := semaphoreTimedWait(s.sem, waitSlice); kr {
		case 0, kernAborted:
		case kernTimedOut:
			if s.device != 0 {
				var alive uint32
				size := uint32(4)
				status := audioObjectGetPropertyData(s.device, selDeviceIsAlive, scopeGlobal, &size, unsafe.Pointer(&alive))
				if status == statusBadDevice || status == statusBadObject || status == 0 && alive == 0 {
					return driver.ErrLost
				}
			}
			return fmt.Errorf("%w: CoreAudio input did not deliver frames within %s", driver.ErrLost, waitSlice)
		default:
			return fmt.Errorf("coreaudio: input semaphore wait failed: %d", kr)
		}
	}
}

func (s *captureStream) interrupt() {
	if s == nil || s.interrupted.Swap(true) {
		return
	}
	s.wakeMu.Lock()
	if !s.closed.Load() && s.sem != 0 {
		semaphoreSignal(s.sem)
	}
	s.wakeMu.Unlock()
}
func (s *captureStream) stop() error { s.mu.Lock(); defer s.mu.Unlock(); return s.stopLocked() }
func (s *captureStream) stopLocked() error {
	if s.stopped.Swap(true) || !s.started.Load() || s.unit == 0 {
		return nil
	}
	return classify(check("AudioOutputUnitStop(input)", audioOutputUnitStop(s.unit)))
}
func (s *captureStream) close() error {
	if s == nil {
		return nil
	}
	s.interrupt()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil
	}
	first := s.stopLocked()
	if s.unit != 0 {
		if s.initialized {
			if err := check("AudioUnitUninitialize(input)", audioUnitUninitialize(s.unit)); err != nil && first == nil {
				first = err
			}
		}
		if err := check("AudioComponentInstanceDispose(input)", audioComponentInstanceDispose(s.unit)); err != nil && first == nil {
			first = err
		}
		s.unit = 0
	}
	s.finalDrops = atomic.LoadUint64(&s.c.under)
	s.wakeMu.Lock()
	s.closed.Store(true)
	sem := s.sem
	s.sem = 0
	s.wakeMu.Unlock()
	if sem != 0 {
		if err := check("semaphore_destroy(input)", semaphoreDestroy(s.task, sem)); err != nil && first == nil {
			first = err
		}
	}
	if err := s.arena.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

// clock maps the HAL's first-frame timestamp to Tymbal's monotonic domain.
// No wall-clock timestamp or fabricated input clock is substituted.
func (s *captureStream) clock(phase float64) driver.ClockSample {
	for tries := 0; tries < 8; tries++ {
		seq := atomic.LoadUint64(&s.c.stampSeq)
		if seq&1 != 0 {
			continue
		}
		head := atomic.LoadUint64(&s.c.stampHead)
		ticks := atomic.LoadUint64(&s.c.hostTicks)
		sample := math.Float64frombits(atomic.LoadUint64(&s.c.sampleBits))
		flags := atomic.LoadUint64(&s.c.stampFlags)
		if atomic.LoadUint64(&s.c.stampSeq) != seq {
			continue
		}
		var c driver.ClockSample
		delta := phase - float64(head)
		if flags&2 != 0 && ticks > 0 && s.denom != 0 {
			c.InputQPCNano = s.rtOrigin + int64(float64(int64(ticks-s.hostOrigin))*float64(s.numer)/float64(s.denom)) + int64(delta*float64(time.Second)/float64(s.rate))
		}
		if flags&1 != 0 && sample+delta >= 0 {
			c.InputPosition = uint64(sample + delta)
			c.InputFrequency = uint64(s.rate)
		}
		return c
	}
	return driver.ClockSample{}
}
