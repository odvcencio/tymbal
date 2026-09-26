//go:build windows

package wasapi

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
	"m31labs.dev/tymbal/internal/format"
	"m31labs.dev/tymbal/internal/rt"
)

const (
	audclntCaptureBufferFlagDataDiscontinuity = 0x00000001
	audclntCaptureBufferFlagTimestampError    = 0x00000004
)

var procQueryPerformanceCounter = kernel32.NewProc("QueryPerformanceCounter")
var procQueryPerformanceFrequency = kernel32.NewProc("QueryPerformanceFrequency")

type captureProbe struct {
	formatBytes  []byte
	format       format.Format
	rate         int
	channels     int
	period       int
	periods      int
	bufferFrames uint32
	latency      time.Duration
	blockAlign   int
}

// captureStager adapts WASAPI's variable packet sizes to Tymbal's fixed
// callback period. Its backing slices are allocated during Open and reused on
// the locked stream thread.
type captureStager struct {
	data, input []byte
	blockAlign  int
	period      int
	rate        int
	capacity    int
	frames      int
	ready       bool
	dropouts    uint64

	stageDevicePosition      uint64
	stageDevicePositionValid bool
	expectedDevicePosition   uint64
	expectedDevicePositionOK bool
	stageQPCPosition         uint64
	stageQPCPositionValid    bool
	activeQPCPosition        uint64
	activeQPCPositionValid   bool
}

// captureStream contains plain negotiated data until Start. Start creates
// fresh COM interfaces on the locked stream thread; Stop releases them there.
type captureStream struct {
	deviceID string
	params   driver.Params
	wave     []byte
	stager   captureStager

	enumerator     uintptr
	device         uintptr
	client         uintptr
	capture        uintptr
	audioEvent     uintptr
	interruptEvent uintptr
	waitHandles    [2]uintptr
	waitProc       uintptr
	bufferFrames   uint32

	packetNextFrames  uint32
	packetNative      uintptr
	packetFrames      uint32
	packetFlags       uint32
	packetDevicePos   uint64
	packetQPCPosition uint64

	qpcOffsetNano  int64
	qpcOffsetValid bool
	comInitialized bool
	clientStarted  bool
	started        bool
	stopped        bool

	interruptRequested atomic.Bool
	interruptMu        sync.Mutex
}

var _ driver.Stream = (*captureStream)(nil)

func openCaptureStream(req driver.Request) (driver.Stream, error) {
	if req.Input == nil || req.Output != nil || req.Exclusive {
		return nil, driver.ErrUnsupported
	}
	if req.Input.ID == "" || req.InChannels <= 0 || req.SampleRate <= 0 || req.Period <= 0 || req.Periods <= 0 {
		return nil, driver.ErrFormat
	}

	probe, err := withSTA(func() (captureProbe, error) { return probeCapture(req) })
	if err != nil {
		return nil, err
	}
	params := driver.Params{
		SampleRate: probe.rate, Period: probe.period, Periods: probe.periods,
		InChannels: probe.channels, InFormat: probe.format,
		LatencyIn: probe.latency,
	}
	stager, err := newCaptureStager(probe.bufferFrames, probe.period, probe.blockAlign, probe.rate)
	if err != nil {
		return nil, err
	}
	return &captureStream{
		deviceID:     req.Input.ID,
		params:       params,
		wave:         probe.formatBytes,
		stager:       *stager,
		bufferFrames: probe.bufferFrames,
	}, nil
}

func probeCapture(req driver.Request) (captureProbe, error) {
	enumerator, err := createEnumerator()
	if err != nil {
		return captureProbe{}, err
	}
	defer release(enumerator)

	device, err := getDevice(enumerator, req.Input.ID)
	if err != nil {
		return captureProbe{}, err
	}
	defer release(device)

	client, err := activateAudioClient3(device)
	if err != nil {
		return captureProbe{}, err
	}
	defer release(client)

	mixFormat, err := getMixFormat(client)
	if err != nil {
		return captureProbe{}, err
	}
	defer procCoTaskMemFree.Call(mixFormat)

	waveBytes, sampleFormat, channels, rate, err := describeWaveFormat(mixFormat)
	if err != nil {
		return captureProbe{}, err
	}
	if rate != req.SampleRate || channels != req.InChannels {
		return captureProbe{}, fmt.Errorf("%w: WASAPI shared mode uses the endpoint mix format (%d Hz, %d channels)", driver.ErrFormat, rate, channels)
	}
	blockAlign := int((*waveFormatEx)(unsafe.Pointer(mixFormat)).blockAlign)
	if blockAlign <= 0 {
		return captureProbe{}, fmt.Errorf("%w: invalid capture block alignment", driver.ErrFormat)
	}
	periods, err := getSharedPeriodRange(client, mixFormat)
	if err != nil {
		return captureProbe{}, err
	}
	period := nearestAllowedPeriod(uint64(req.Period), periods)
	if period == 0 {
		return captureProbe{}, fmt.Errorf("%w: WASAPI returned no valid shared period in %d..%d", driver.ErrFormat, periods.minFrames, periods.maxFrames)
	}
	event, err := createEvent()
	if err != nil {
		return captureProbe{}, err
	}
	defer closeEvent(event)
	if err := initializeShared(client, mixFormat, period); err != nil {
		return captureProbe{}, classifyInitializeError(err)
	}
	if err := setAudioEvent(client, event); err != nil {
		return captureProbe{}, err
	}
	bufferFrames, err := getCaptureBufferSize(client)
	if err != nil {
		return captureProbe{}, err
	}
	currentFormat, currentPeriod, err := getCurrentSharedEnginePeriod(client)
	if err != nil {
		return captureProbe{}, err
	}
	defer procCoTaskMemFree.Call(currentFormat)
	if currentPeriod != period {
		return captureProbe{}, fmt.Errorf("%w: WASAPI granted period %d, selected %d", driver.ErrFormat, currentPeriod, period)
	}
	currentBytes, _, currentChannels, currentRate, err := describeWaveFormat(currentFormat)
	if err != nil {
		return captureProbe{}, err
	}
	if currentRate != rate || currentChannels != channels || !equalBytes(currentBytes, waveBytes) {
		return captureProbe{}, fmt.Errorf("%w: WASAPI shared engine format changed during negotiation", driver.ErrFormat)
	}
	periodCount, latency, err := captureBufferGeometry(bufferFrames, int(period), rate)
	if err != nil {
		return captureProbe{}, err
	}
	return captureProbe{
		formatBytes:  waveBytes,
		format:       sampleFormat,
		rate:         rate,
		channels:     channels,
		period:       int(period),
		periods:      periodCount,
		bufferFrames: bufferFrames,
		latency:      latency,
		blockAlign:   blockAlign,
	}, nil
}

func newCaptureStager(bufferFrames uint32, period, blockAlign, rate int) (*captureStager, error) {
	maxInt := int(^uint(0) >> 1)
	if bufferFrames == 0 || period <= 0 || blockAlign <= 0 || rate <= 0 || uint64(bufferFrames) > uint64(maxInt-period) {
		return nil, fmt.Errorf("%w: invalid capture staging geometry", driver.ErrFormat)
	}
	capacity := int(bufferFrames) + period
	if capacity > maxInt/blockAlign || period > maxInt/blockAlign {
		return nil, fmt.Errorf("%w: capture staging buffer size overflows", driver.ErrFormat)
	}
	return &captureStager{
		data:       make([]byte, capacity*blockAlign),
		input:      make([]byte, period*blockAlign),
		blockAlign: blockAlign,
		period:     period,
		rate:       rate,
		capacity:   capacity,
	}, nil
}

func (s *captureStream) Params() driver.Params { return s.params }

func (s *captureStream) Start() error {
	if s.started || s.stopped {
		return fmt.Errorf("tymbal wasapi: stream already started or stopped")
	}
	if err := procWaitForMultipleObjects.Find(); err != nil {
		s.stopped = true
		return fmt.Errorf("tymbal wasapi: load WaitForMultipleObjects: %w", err)
	}
	s.waitProc = procWaitForMultipleObjects.Addr()
	if err := initializeSTA(); err != nil {
		s.stopped = true
		return err
	}
	s.comInitialized = true
	if err := s.openOnStreamThread(); err != nil {
		cleanupErr := s.cleanupOnStreamThread()
		s.stopped = true
		return errors.Join(err, cleanupErr)
	}
	s.qpcOffsetNano, s.qpcOffsetValid = captureQPCOffset()
	if err := checkHRESULT("IAudioClient.Start", comCall0(s.client, audioClientStart)); err != nil {
		cleanupErr := s.cleanupOnStreamThread()
		s.stopped = true
		return errWithCleanup(err, cleanupErr)
	}
	s.clientStarted = true
	s.started = true
	if s.interruptRequested.Load() {
		s.interruptMu.Lock()
		if s.interruptEvent != 0 {
			_ = signalEvent(s.interruptEvent)
		}
		s.interruptMu.Unlock()
	}
	return nil
}

func (s *captureStream) openOnStreamThread() error {
	enumerator, err := createEnumerator()
	if err != nil {
		return err
	}
	s.enumerator = enumerator
	device, err := getDevice(enumerator, s.deviceID)
	if err != nil {
		return err
	}
	s.device = device
	client, err := activateAudioClient3(device)
	if err != nil {
		return err
	}
	s.client = client

	mixFormat, err := getMixFormat(client)
	if err != nil {
		return err
	}
	defer procCoTaskMemFree.Call(mixFormat)
	waveBytes, sampleFormat, channels, rate, err := describeWaveFormat(mixFormat)
	if err != nil {
		return err
	}
	if rate != s.params.SampleRate || channels != s.params.InChannels || sampleFormat != s.params.InFormat || !equalBytes(waveBytes, s.wave) {
		return fmt.Errorf("%w: endpoint mix format changed after Open", driver.ErrFormat)
	}
	periods, err := getSharedPeriodRange(client, mixFormat)
	if err != nil {
		return err
	}
	if !periodAllowed(uint32(s.params.Period), periods) {
		return fmt.Errorf("%w: requested period is no longer supported", driver.ErrFormat)
	}
	if err := createCaptureEvents(s); err != nil {
		return err
	}
	if err := initializeShared(client, mixFormat, uint32(s.params.Period)); err != nil {
		return classifyInitializeError(err)
	}
	if err := setAudioEvent(client, s.audioEvent); err != nil {
		return err
	}
	bufferFrames, err := getCaptureBufferSize(client)
	if err != nil {
		return err
	}
	if bufferFrames != s.bufferFrames {
		return fmt.Errorf("%w: WASAPI capture buffer changed from %d to %d frames after Open", driver.ErrFormat, s.bufferFrames, bufferFrames)
	}
	periodCount, latency, err := captureBufferGeometry(bufferFrames, s.params.Period, s.params.SampleRate)
	if err != nil {
		return err
	}
	if periodCount != s.params.Periods || latency != s.params.LatencyIn {
		return fmt.Errorf("%w: WASAPI capture buffer geometry changed after Open", driver.ErrFormat)
	}
	currentFormat, currentPeriod, err := getCurrentSharedEnginePeriod(client)
	if err != nil {
		return err
	}
	defer procCoTaskMemFree.Call(currentFormat)
	if currentPeriod != uint32(s.params.Period) {
		return fmt.Errorf("%w: WASAPI granted period %d, requested %d", driver.ErrFormat, currentPeriod, s.params.Period)
	}
	currentBytes, _, currentChannels, currentRate, err := describeWaveFormat(currentFormat)
	if err != nil {
		return err
	}
	if currentRate != s.params.SampleRate || currentChannels != s.params.InChannels || !equalBytes(currentBytes, s.wave) {
		return fmt.Errorf("%w: WASAPI shared engine format changed after Open", driver.ErrFormat)
	}
	capture, err := getCaptureClient(client)
	if err != nil {
		return err
	}
	s.capture = capture
	return nil
}

func createCaptureEvents(s *captureStream) error {
	audio, err := createEvent()
	if err != nil {
		return err
	}
	interrupt, err := createEvent()
	if err != nil {
		_ = closeEvent(audio)
		return err
	}
	s.audioEvent = audio
	s.interruptMu.Lock()
	s.interruptEvent = interrupt
	s.interruptMu.Unlock()
	s.waitHandles = [2]uintptr{audio, interrupt}
	if s.interruptRequested.Load() {
		_ = signalEvent(interrupt)
	}
	return nil
}

func (s *captureStream) Wait() error {
	if !s.started || s.stopped {
		return fmt.Errorf("tymbal wasapi: wait outside a running capture stream")
	}
	if s.stager.ready {
		return fmt.Errorf("tymbal wasapi: wait before committing the previous capture period")
	}
	if s.interruptRequested.Load() {
		return driver.ErrInterrupted
	}
	if s.stager.preparePeriod() {
		return nil
	}
	if err := s.drainCapturePackets(); err != nil {
		return err
	}
	if s.stager.preparePeriod() {
		return nil
	}
	for {
		result, _, callErr := syscall.SyscallN(s.waitProc,
			2,
			uintptr(unsafe.Pointer(&s.waitHandles[0])),
			0,
			infinite,
		)
		runtime.KeepAlive(s)
		if result == waitObject0+1 || s.interruptRequested.Load() {
			return driver.ErrInterrupted
		}
		if result == waitFailed {
			if callErr == 0 {
				callErr = syscall.EINVAL
			}
			return fmt.Errorf("tymbal wasapi: wait for capture event: %w", callErr)
		}
		if result != waitObject0 {
			return fmt.Errorf("tymbal wasapi: unexpected capture wait result 0x%08X", uint32(result))
		}
		if err := s.drainCapturePackets(); err != nil {
			return err
		}
		if s.stager.preparePeriod() {
			return nil
		}
	}
}

func (s *captureStream) drainCapturePackets() error {
	for {
		s.packetNextFrames = 0
		hr := comCall1(s.capture, audioCaptureClientGetNextPacketSize, uintptr(unsafe.Pointer(&s.packetNextFrames)))
		runtime.KeepAlive(s)
		if err := captureHRESULT("IAudioCaptureClient.GetNextPacketSize", hr); err != nil {
			return err
		}
		if s.packetNextFrames == 0 {
			return nil
		}

		s.packetNative = 0
		s.packetFrames = 0
		s.packetFlags = 0
		s.packetDevicePos = 0
		s.packetQPCPosition = 0
		hr = comCall5(s.capture, audioCaptureClientGetBuffer,
			uintptr(unsafe.Pointer(&s.packetNative)),
			uintptr(unsafe.Pointer(&s.packetFrames)),
			uintptr(unsafe.Pointer(&s.packetFlags)),
			uintptr(unsafe.Pointer(&s.packetDevicePos)),
			uintptr(unsafe.Pointer(&s.packetQPCPosition)))
		runtime.KeepAlive(s)
		if err := captureHRESULT("IAudioCaptureClient.GetBuffer", hr); err != nil {
			return err
		}
		if s.packetFrames == 0 {
			// AUDCLNT_S_BUFFER_EMPTY does not require ReleaseBuffer.
			return nil
		}

		var packet []byte
		packetErr := error(nil)
		if s.packetFlags&audclntBufferFlagSilent == 0 {
			byteCount, sizeErr := capturePacketByteCount(s.packetFrames, s.stager.blockAlign)
			if sizeErr != nil {
				packetErr = sizeErr
			} else if s.packetNative == 0 {
				packetErr = errors.New("tymbal wasapi: non-silent capture packet returned nil data")
			} else {
				packet = unsafe.Slice((*byte)(unsafe.Pointer(s.packetNative)), byteCount)
			}
		}
		if packetErr == nil {
			packetErr = s.stager.appendPacket(packet, int(s.packetFrames), s.packetFlags, s.packetDevicePos, s.packetQPCPosition)
		}
		releaseErr := captureHRESULT("IAudioCaptureClient.ReleaseBuffer", comCall1(s.capture, audioCaptureClientReleaseBuffer, uintptr(s.packetFrames)))
		runtime.KeepAlive(s)
		s.packetNative = 0
		if packetErr != nil || releaseErr != nil {
			return errors.Join(packetErr, releaseErr)
		}
		if s.stager.frames >= s.stager.period {
			return nil
		}
	}
}

func (s *captureStream) Interrupt() {
	s.interruptRequested.Store(true)
	s.interruptMu.Lock()
	if s.interruptEvent != 0 {
		_ = signalEvent(s.interruptEvent)
	}
	s.interruptMu.Unlock()
}

func (s *captureStream) Buffers() (in, out []byte) {
	if s.stager.ready {
		return s.stager.input, nil
	}
	return nil, nil
}

func (s *captureStream) Commit() error {
	if !s.started || s.stopped || !s.stager.ready {
		return fmt.Errorf("tymbal wasapi: commit without a complete capture period")
	}
	s.stager.commitPeriod()
	return nil
}

func (s *captureStream) Clock() (outNano, inNano int64) {
	if !s.stager.activeQPCPositionValid || !s.qpcOffsetValid || s.stager.activeQPCPosition > uint64(math.MaxInt64/100) {
		return 0, 0
	}
	qpcNano := int64(s.stager.activeQPCPosition * 100)
	if s.qpcOffsetNano > 0 && qpcNano > math.MaxInt64-s.qpcOffsetNano ||
		s.qpcOffsetNano < 0 && qpcNano < math.MinInt64-s.qpcOffsetNano {
		return 0, 0
	}
	return 0, qpcNano + s.qpcOffsetNano
}

func (s *captureStream) Deadlines() (wakeNano, commitNano int64) { return 0, 0 }

func (s *captureStream) Dropouts() uint64 { return s.stager.dropouts }

func (s *captureStream) Recover() error {
	if !s.started || s.stopped {
		return fmt.Errorf("tymbal wasapi: recover outside a running capture stream")
	}
	s.stager.reset(true)
	return nil
}

func (s *captureStream) Stop() error {
	if s.stopped {
		return nil
	}
	stopErr := s.cleanupOnStreamThread()
	s.stopped = true
	return stopErr
}

func (s *captureStream) cleanupOnStreamThread() error {
	var firstErr error
	if s.clientStarted && s.client != 0 {
		if err := checkHRESULT("IAudioClient.Stop", comCall0(s.client, audioClientStop)); err != nil {
			firstErr = err
		}
		s.clientStarted = false
	}
	release(s.capture)
	s.capture = 0
	release(s.client)
	s.client = 0
	release(s.device)
	s.device = 0
	release(s.enumerator)
	s.enumerator = 0
	if s.audioEvent != 0 {
		if err := closeEvent(s.audioEvent); err != nil && firstErr == nil {
			firstErr = err
		}
		s.audioEvent = 0
	}
	s.interruptMu.Lock()
	interrupt := s.interruptEvent
	s.interruptEvent = 0
	s.waitHandles = [2]uintptr{}
	if interrupt != 0 {
		if err := closeEvent(interrupt); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.interruptMu.Unlock()
	if s.comInitialized {
		uninitializeCOM()
		s.comInitialized = false
	}
	s.started = false
	s.stager.ready = false
	return firstErr
}

// Close is safe on the caller thread because all COM interfaces and event
// handles are owned and released by Stop on the locked stream thread.
func (s *captureStream) Close() error { return nil }

func getCaptureBufferSize(client uintptr) (uint32, error) {
	var frames uint32
	hr := comCall1(client, audioClientGetBufferSize, uintptr(unsafe.Pointer(&frames)))
	runtime.KeepAlive(&frames)
	if err := checkHRESULT("IAudioClient.GetBufferSize", hr); err != nil {
		return 0, err
	}
	if frames == 0 {
		return 0, fmt.Errorf("%w: WASAPI reported an empty capture buffer", driver.ErrFormat)
	}
	return frames, nil
}

func captureBufferGeometry(bufferFrames uint32, period, rate int) (int, time.Duration, error) {
	if bufferFrames == 0 || period <= 0 || rate <= 0 {
		return 0, 0, fmt.Errorf("%w: invalid WASAPI capture buffer geometry", driver.ErrFormat)
	}
	periodFrames := uint64(period)
	buffer := uint64(bufferFrames)
	periodCount := (buffer + periodFrames - 1) / periodFrames
	if periodCount == 0 || periodCount > uint64(int(^uint(0)>>1)) {
		return 0, 0, fmt.Errorf("%w: WASAPI capture buffer geometry overflows", driver.ErrFormat)
	}
	latency := time.Duration(buffer * uint64(time.Second) / uint64(rate))
	return int(periodCount), latency, nil
}

func getCaptureClient(client uintptr) (uintptr, error) {
	var capture uintptr
	hr := comCall2(client, audioClientGetService,
		uintptr(unsafe.Pointer(&iidAudioCaptureClient)), uintptr(unsafe.Pointer(&capture)))
	runtime.KeepAlive(&iidAudioCaptureClient)
	runtime.KeepAlive(&capture)
	if err := checkHRESULT("IAudioClient.GetService(IAudioCaptureClient)", hr); err != nil {
		release(capture)
		return 0, err
	}
	if capture == 0 {
		return 0, errors.New("tymbal wasapi: GetService returned nil capture client")
	}
	return capture, nil
}

func captureHRESULT(op string, hr uintptr) error {
	err := checkHRESULT(op, hr)
	if err == nil {
		return nil
	}
	var hrErr *hresultError
	if errors.As(err, &hrErr) {
		switch hrErr.code {
		case audclntErrOutOfOrder, audclntErrBufferTooLarge:
			return fmt.Errorf("%w: %v", driver.ErrXrun, err)
		}
	}
	return err
}

func capturePacketByteCount(frames uint32, blockAlign int) (int, error) {
	maxInt := uint64(^uint(0) >> 1)
	if blockAlign <= 0 || uint64(frames) > maxInt/uint64(blockAlign) {
		return 0, fmt.Errorf("%w: capture packet size overflows", driver.ErrFormat)
	}
	return int(frames) * blockAlign, nil
}

func (s *captureStager) appendPacket(packet []byte, frames int, flags uint32, devicePosition, qpcPosition uint64) error {
	if frames <= 0 || s.blockAlign <= 0 || frames > int(^uint(0)>>1)/s.blockAlign {
		return fmt.Errorf("%w: invalid capture packet geometry", driver.ErrFormat)
	}
	bytes := frames * s.blockAlign
	silent := flags&audclntBufferFlagSilent != 0
	if !silent && len(packet) < bytes {
		return fmt.Errorf("%w: short capture packet", driver.ErrFormat)
	}
	positionValid := flags&audclntCaptureBufferFlagTimestampError == 0
	discontinuity := flags&audclntCaptureBufferFlagDataDiscontinuity != 0
	if positionValid && s.expectedDevicePositionOK && devicePosition != s.expectedDevicePosition {
		discontinuity = true
	}
	if discontinuity {
		s.dropouts++
		s.resetStage()
		s.expectedDevicePositionOK = false
	}
	if frames > s.capacity {
		s.dropouts++
		s.resetStage()
		s.expectedDevicePositionOK = false
		return fmt.Errorf("%w: capture packet of %d frames exceeds staging capacity %d", driver.ErrFormat, frames, s.capacity)
	}
	if s.frames+frames > s.capacity {
		// The oldest partial data cannot form a complete callback period without
		// risking a stale sample after this overflow. Resume at this packet.
		s.dropouts++
		s.resetStage()
		s.expectedDevicePositionOK = false
	}
	start := s.frames * s.blockAlign
	if silent {
		clear(s.data[start : start+bytes])
	} else {
		copy(s.data[start:start+bytes], packet[:bytes])
	}
	if s.frames == 0 {
		s.stageDevicePosition = devicePosition
		s.stageDevicePositionValid = positionValid
		s.stageQPCPosition = qpcPosition
		s.stageQPCPositionValid = positionValid
	}
	s.frames += frames
	if positionValid {
		s.expectedDevicePosition = devicePosition + uint64(frames)
		s.expectedDevicePositionOK = true
	} else {
		s.expectedDevicePositionOK = false
	}
	return nil
}

func (s *captureStager) preparePeriod() bool {
	if s.ready || s.frames < s.period {
		return s.ready
	}
	periodBytes := s.period * s.blockAlign
	copy(s.input, s.data[:periodBytes])
	s.activeQPCPosition = s.stageQPCPosition
	s.activeQPCPositionValid = s.stageQPCPositionValid
	s.ready = true
	return true
}

func (s *captureStager) commitPeriod() {
	if !s.ready {
		return
	}
	periodBytes := s.period * s.blockAlign
	remainingBytes := s.frames*s.blockAlign - periodBytes
	copy(s.data[:remainingBytes], s.data[periodBytes:periodBytes+remainingBytes])
	s.frames -= s.period
	if s.stageDevicePositionValid {
		s.stageDevicePosition += uint64(s.period)
	}
	if s.stageQPCPositionValid {
		s.stageQPCPosition += uint64(s.period) * 10_000_000 / uint64(s.rate)
	}
	if s.frames == 0 {
		s.stageDevicePositionValid = false
		s.stageQPCPositionValid = false
	}
	s.activeQPCPositionValid = false
	s.ready = false
}

func (s *captureStager) resetStage() {
	s.frames = 0
	s.ready = false
	s.stageDevicePositionValid = false
	s.stageQPCPositionValid = false
	s.activeQPCPositionValid = false
}

func (s *captureStager) reset(clearPosition bool) {
	s.resetStage()
	if clearPosition {
		s.expectedDevicePositionOK = false
	}
}

func captureQPCOffset() (int64, bool) {
	var counter, frequency int64
	result, _, callErr := procQueryPerformanceCounter.Call(uintptr(unsafe.Pointer(&counter)))
	runtime.KeepAlive(&counter)
	if result == 0 {
		_ = callErr
		return 0, false
	}
	result, _, callErr = procQueryPerformanceFrequency.Call(uintptr(unsafe.Pointer(&frequency)))
	runtime.KeepAlive(&frequency)
	if result == 0 || frequency <= 0 || counter < 0 {
		_ = callErr
		return 0, false
	}
	count := uint64(counter)
	freq := uint64(frequency)
	hundredNanos := count/freq*10_000_000 + count%freq*10_000_000/freq
	if hundredNanos > uint64(math.MaxInt64/100) {
		return 0, false
	}
	return rt.Now() - int64(hundredNanos*100), true
}
