//go:build windows

package wasapi

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
	audioformat "m31labs.dev/tymbal/internal/format"
)

const exclusiveRenderPingPongBuffers = 2

type exclusiveRenderProbe struct {
	formatBytes  []byte
	format       audioformat.Format
	rate         int
	channels     int
	bufferFrames uint32
	bufferHNS    int64
	latency      time.Duration
}

// exclusiveRenderStream shares the preallocated render buffer and lifecycle
// cleanup with shared mode. Its Wait uses event-driven exclusive semantics,
// where each event grants exactly one complete native buffer.
type exclusiveRenderStream struct {
	wasapiStream
	bufferHNS int64
}

var _ driver.Stream = (*exclusiveRenderStream)(nil)

func openExclusiveRenderStream(req driver.Request) (driver.Stream, error) {
	if err := validateExclusiveRenderRequest(req); err != nil {
		return nil, err
	}

	probe, err := withSTA(func() (exclusiveRenderProbe, error) { return probeExclusiveRender(req) })
	if err != nil {
		return nil, err
	}
	period, periods, _, err := exclusiveRenderBufferGeometry(probe.bufferFrames)
	if err != nil {
		return nil, err
	}
	bytesPerSample := audioformat.BytesPerSample(probe.format)
	maxInt := uint64(int(^uint(0) >> 1))
	frameBytes := uint64(probe.channels) * uint64(bytesPerSample)
	if bytesPerSample == 0 || frameBytes == 0 || uint64(probe.bufferFrames) > maxInt/frameBytes {
		return nil, fmt.Errorf("%w: exclusive render buffer allocation overflows", driver.ErrFormat)
	}
	outBytes := int(uint64(probe.bufferFrames) * frameBytes)
	params := driver.Params{
		SampleRate: probe.rate, Period: period, Periods: periods,
		OutChannels: probe.channels, OutFormat: probe.format,
		LatencyOut: probe.latency,
	}
	return &exclusiveRenderStream{
		wasapiStream: wasapiStream{
			deviceID:     req.Output.ID,
			params:       params,
			wave:         probe.formatBytes,
			out:          make([]byte, outBytes),
			bufferFrames: probe.bufferFrames,
		},
		bufferHNS: probe.bufferHNS,
	}, nil
}

func validateExclusiveRenderRequest(req driver.Request) error {
	if req.Input != nil || req.Output == nil {
		return driver.ErrUnsupported
	}
	if req.Periods != exclusiveRenderPingPongBuffers {
		return fmt.Errorf("%w: exclusive event render requires exactly two native buffers", driver.ErrFormat)
	}
	if req.Output.ID == "" || req.OutChannels <= 0 || req.SampleRate <= 0 || req.Period <= 0 {
		return driver.ErrFormat
	}
	return nil
}

func probeExclusiveRender(req driver.Request) (exclusiveRenderProbe, error) {
	enumerator, err := createEnumerator()
	if err != nil {
		return exclusiveRenderProbe{}, err
	}
	defer release(enumerator)

	device, err := getDevice(enumerator, req.Output.ID)
	if err != nil {
		return exclusiveRenderProbe{}, err
	}
	defer release(device)

	client, err := activateAudioClient(device)
	if err != nil {
		return exclusiveRenderProbe{}, err
	}
	defer func() { release(client) }()

	formats, err := exclusiveFormatCandidates(client, req.SampleRate, req.OutChannels)
	if err != nil {
		return exclusiveRenderProbe{}, err
	}
	selected, err := selectExclusiveFormat(formats, func(candidate []byte) (bool, error) {
		supported, probeErr := isExclusiveFormatSupported(client, uintptr(unsafe.Pointer(&candidate[0])))
		runtime.KeepAlive(candidate)
		return supported, probeErr
	})
	if err != nil {
		if errors.Is(err, driver.ErrFormat) {
			return exclusiveRenderProbe{}, fmt.Errorf("%w: endpoint has no exact exclusive format for requested render", driver.ErrFormat)
		}
		return exclusiveRenderProbe{}, err
	}
	minimumPeriod, err := getExclusiveMinimumPeriod(client)
	if err != nil {
		return exclusiveRenderProbe{}, classifyExclusiveRenderError("IAudioClient.GetDevicePeriod", err)
	}
	// The caller's Period describes one event buffer. WASAPI's exclusive event
	// mode exposes a ping-pong pair, which is reported separately as Periods=2.
	duration, err := requestedExclusiveBufferDuration(req.Period, 1, req.SampleRate, minimumPeriod)
	if err != nil {
		return exclusiveRenderProbe{}, err
	}
	bufferFrames, actualHNS, err := initializeExclusiveAlignedForDevice(device, &client, selected.bytes, duration, req.SampleRate)
	if err != nil {
		return exclusiveRenderProbe{}, classifyExclusiveRenderError("IAudioClient.Initialize", err)
	}
	period, _, _, err := exclusiveRenderBufferGeometry(bufferFrames)
	if err != nil {
		return exclusiveRenderProbe{}, err
	}
	if period <= 0 || actualHNS <= 0 {
		return exclusiveRenderProbe{}, fmt.Errorf("%w: WASAPI returned an invalid exclusive render buffer", driver.ErrFormat)
	}
	event, err := createEvent()
	if err != nil {
		return exclusiveRenderProbe{}, err
	}
	defer closeEvent(event)
	if err := setAudioEvent(client, event); err != nil {
		return exclusiveRenderProbe{}, classifyExclusiveRenderError("IAudioClient.SetEventHandle", err)
	}
	latency, err := getStreamLatency(client)
	if err != nil {
		return exclusiveRenderProbe{}, classifyExclusiveRenderError("IAudioClient.GetStreamLatency", err)
	}

	return exclusiveRenderProbe{
		formatBytes:  selected.bytes,
		format:       selected.format,
		rate:         req.SampleRate,
		channels:     req.OutChannels,
		bufferFrames: bufferFrames,
		bufferHNS:    actualHNS,
		latency:      latency,
	}, nil
}

// exclusiveRenderBufferGeometry reports the complete single-buffer callback
// period and total ping-pong frame capacity. It does not estimate OS latency.
func exclusiveRenderBufferGeometry(bufferFrames uint32) (period, periods int, capacityFrames uint64, err error) {
	if bufferFrames == 0 {
		return 0, 0, 0, fmt.Errorf("%w: invalid exclusive render buffer geometry", driver.ErrFormat)
	}
	maxInt := uint64(int(^uint(0) >> 1))
	frames := uint64(bufferFrames)
	if frames > maxInt {
		return 0, 0, 0, fmt.Errorf("%w: exclusive render period overflows", driver.ErrFormat)
	}
	capacityFrames = frames * exclusiveRenderPingPongBuffers
	return int(bufferFrames), exclusiveRenderPingPongBuffers, capacityFrames, nil
}

func (s *exclusiveRenderStream) Start() error {
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
	if err := s.openExclusiveOnStreamThread(); err != nil {
		cleanupErr := s.cleanupOnStreamThread()
		s.stopped = true
		return errors.Join(err, cleanupErr)
	}
	// Exclusive event render requires the first complete ping-pong buffer to be
	// queued before Start. ReleaseBuffer(SILENT) primes it without audio data.
	if err := primeRenderBuffer(s.render, s.bufferFrames); err != nil {
		cleanupErr := s.cleanupOnStreamThread()
		s.stopped = true
		return errWithCleanup(classifyExclusiveRenderError("IAudioRenderClient priming", err), cleanupErr)
	}
	if err := checkHRESULT("IAudioClient.Start", comCall0(s.client, audioClientStart)); err != nil {
		cleanupErr := s.cleanupOnStreamThread()
		s.stopped = true
		return errWithCleanup(classifyExclusiveRenderError("IAudioClient.Start", err), cleanupErr)
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

func (s *exclusiveRenderStream) openExclusiveOnStreamThread() error {
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
	client, err := activateAudioClient(device)
	if err != nil {
		return err
	}
	s.client = client

	supported, err := isExclusiveFormatSupported(s.client, uintptr(unsafe.Pointer(&s.wave[0])))
	if err != nil {
		return classifyExclusiveRenderError("IAudioClient.IsFormatSupported", err)
	}
	if !supported {
		return fmt.Errorf("%w: endpoint no longer supports the negotiated exclusive render format", driver.ErrFormat)
	}
	bufferFrames, actualHNS, err := initializeExclusiveAlignedForDevice(s.device, &s.client, s.wave, s.bufferHNS, s.params.SampleRate)
	if err != nil {
		return classifyExclusiveRenderError("IAudioClient.Initialize", err)
	}
	if bufferFrames != s.bufferFrames || actualHNS != s.bufferHNS {
		return fmt.Errorf("%w: exclusive render buffer changed from %d frames at %d hns to %d frames at %d hns after Open", driver.ErrFormat, s.bufferFrames, s.bufferHNS, bufferFrames, actualHNS)
	}
	period, periods, _, err := exclusiveRenderBufferGeometry(bufferFrames)
	if err != nil {
		return err
	}
	if period != s.params.Period || periods != s.params.Periods {
		return fmt.Errorf("%w: exclusive render buffer geometry changed after Open", driver.ErrFormat)
	}
	latency, err := getStreamLatency(s.client)
	if err != nil {
		return classifyExclusiveRenderError("IAudioClient.GetStreamLatency", err)
	}
	if latency != s.params.LatencyOut {
		return fmt.Errorf("%w: exclusive render OS latency changed from %s to %s after Open", driver.ErrFormat, s.params.LatencyOut, latency)
	}
	if err := createExclusiveRenderEvents(&s.wasapiStream); err != nil {
		return err
	}
	if err := setAudioEvent(s.client, s.audioEvent); err != nil {
		return classifyExclusiveRenderError("IAudioClient.SetEventHandle", err)
	}
	render, err := getRenderClient(s.client)
	if err != nil {
		return classifyExclusiveRenderError("IAudioClient.GetService(IAudioRenderClient)", err)
	}
	s.render = render
	return nil
}

func createExclusiveRenderEvents(s *wasapiStream) error {
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
	s.waitProc = procWaitForMultipleObjects.Addr()
	s.waitHandles = [2]uintptr{audio, interrupt}
	if s.interruptRequested.Load() {
		_ = signalEvent(interrupt)
	}
	return nil
}

func (s *exclusiveRenderStream) Wait() error {
	if !s.started || s.stopped {
		return fmt.Errorf("tymbal wasapi: wait outside a running exclusive render stream")
	}
	if s.ready {
		return fmt.Errorf("tymbal wasapi: wait before committing the previous exclusive render buffer")
	}
	if s.interruptRequested.Load() {
		return driver.ErrInterrupted
	}
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
		return fmt.Errorf("tymbal wasapi: wait for exclusive render event: %w", callErr)
	}
	if result != waitObject0 {
		return fmt.Errorf("tymbal wasapi: unexpected exclusive render wait result 0x%08X", uint32(result))
	}
	s.ready = true
	return nil
}

func classifyExclusiveRenderError(op string, err error) error {
	if err == nil || errors.Is(err, driver.ErrLost) {
		return err
	}
	var hrErr *hresultError
	if errors.As(err, &hrErr) {
		switch hrErr.code {
		case audclntErrDeviceInUse:
			return fmt.Errorf("%w: exclusive render endpoint is busy (%s)", driver.ErrBusy, op)
		case audclntErrExclusiveModeNotAllowed:
			return fmt.Errorf("%w: endpoint settings disable exclusive render (%s)", driver.ErrUnsupported, op)
		case audclntErrUnsupportedFormat:
			return fmt.Errorf("%w: endpoint does not support the requested exclusive render format (%s)", driver.ErrFormat, op)
		case audclntErrBufferSizeNotAligned:
			return fmt.Errorf("%w: endpoint rejected the aligned exclusive render buffer (%s)", driver.ErrFormat, op)
		case audclntErrInvalidDevicePeriod:
			return fmt.Errorf("%w: endpoint rejected the exclusive render device period (%s)", driver.ErrFormat, op)
		}
	}
	return err
}
