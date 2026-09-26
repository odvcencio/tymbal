//go:build windows

package wasapi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
	"m31labs.dev/tymbal/internal/format"
)

const (
	waveFormatPCM           = 0x0001
	waveFormatIEEEFloat     = 0x0003
	waveFormatExtensibleTag = 0xfffe

	audclntErrOutOfOrder          = 0x88890001
	audclntErrBufferTooLarge      = 0x88890006
	audclntErrUnsupportedFormat   = 0x88890008
	audclntErrInvalidDevicePeriod = 0x88890037

	waitObject0 = 0
	waitFailed  = 0xffffffff
	infinite    = 0xffffffff

	maxWaveFormatBytes = 4096
)

var (
	iidAudioClient3Render = iidAudioClient3

	waveSubFormatPCM       = guid{data1: 0x00000001, data3: 0x0010, data4: [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}}
	waveSubFormatIEEEFloat = guid{data1: 0x00000003, data3: 0x0010, data4: [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}}

	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	procCreateEventW           = kernel32.NewProc("CreateEventW")
	procSetEvent               = kernel32.NewProc("SetEvent")
	procCloseHandle            = kernel32.NewProc("CloseHandle")
	procWaitForMultipleObjects = kernel32.NewProc("WaitForMultipleObjects")
)

type sharedPeriodRange struct {
	defaultFrames     uint32
	fundamentalFrames uint32
	minFrames         uint32
	maxFrames         uint32
}

type renderProbe struct {
	formatBytes  []byte
	format       format.Format
	rate         int
	channels     int
	period       int
	periods      int
	bufferFrames uint32
	latency      time.Duration
}

// wasapiStream contains plain negotiated data until Start. Start creates all
// COM interfaces on Tymbal's locked stream thread; Stop releases them there.
type wasapiStream struct {
	deviceID string
	params   driver.Params
	wave     []byte
	out      []byte

	enumerator     uintptr
	device         uintptr
	client         uintptr
	render         uintptr
	audioEvent     uintptr
	interruptEvent uintptr
	waitProc       uintptr
	waitHandles    [2]uintptr
	bufferFrames   uint32
	renderBuffer   uintptr // reusable COM output slot; its address stays off the stack

	comInitialized bool
	clientStarted  bool
	started        bool
	stopped        bool
	ready          bool
	dropouts       uint64

	interruptRequested atomic.Bool
	interruptMu        sync.Mutex
}

var _ driver.Stream = (*wasapiStream)(nil)

func openRenderStream(req driver.Request) (driver.Stream, error) {
	if req.Output == nil {
		if req.Input != nil {
			return nil, driver.ErrUnsupported
		}
		return nil, fmt.Errorf("tymbal wasapi: %w: a render endpoint is required", driver.ErrUnsupported)
	}
	if req.Input != nil || req.Exclusive {
		return nil, driver.ErrUnsupported
	}
	if req.Output.ID == "" || req.OutChannels <= 0 || req.SampleRate <= 0 || req.Period <= 0 || req.Periods <= 0 {
		return nil, driver.ErrFormat
	}

	probe, err := withSTA(func() (renderProbe, error) { return probeRender(req) })
	if err != nil {
		return nil, err
	}
	params := driver.Params{
		SampleRate: probe.rate, Period: probe.period, Periods: probe.periods,
		OutChannels: probe.channels, OutFormat: probe.format,
		LatencyOut: probe.latency,
	}
	bytesPerSample := format.BytesPerSample(probe.format)
	maxInt := int(^uint(0) >> 1)
	if probe.period > maxInt/probe.channels || probe.period*probe.channels > maxInt/bytesPerSample {
		return nil, driver.ErrFormat
	}
	outBytes := probe.period * probe.channels * bytesPerSample
	return &wasapiStream{
		deviceID:     req.Output.ID,
		params:       params,
		wave:         probe.formatBytes,
		out:          make([]byte, outBytes),
		bufferFrames: probe.bufferFrames,
	}, nil
}

func probeRender(req driver.Request) (renderProbe, error) {
	enumerator, err := createEnumerator()
	if err != nil {
		return renderProbe{}, err
	}
	defer release(enumerator)

	device, err := getDevice(enumerator, req.Output.ID)
	if err != nil {
		return renderProbe{}, err
	}
	defer release(device)

	client, err := activateAudioClient3(device)
	if err != nil {
		return renderProbe{}, err
	}
	defer release(client)

	mixFormat, err := getMixFormat(client)
	if err != nil {
		return renderProbe{}, err
	}
	defer procCoTaskMemFree.Call(mixFormat)

	waveBytes, sampleFormat, channels, rate, err := describeWaveFormat(mixFormat)
	if err != nil {
		return renderProbe{}, err
	}
	if rate != req.SampleRate || channels != req.OutChannels {
		return renderProbe{}, fmt.Errorf("%w: WASAPI shared mode uses the endpoint mix format (%d Hz, %d channels)", driver.ErrFormat, rate, channels)
	}
	periods, err := getSharedPeriodRange(client, mixFormat)
	if err != nil {
		return renderProbe{}, err
	}
	period := nearestAllowedPeriod(uint64(req.Period), periods)
	if period == 0 {
		return renderProbe{}, fmt.Errorf("%w: WASAPI returned no valid shared period in %d..%d", driver.ErrFormat, periods.minFrames, periods.maxFrames)
	}

	event, err := createEvent()
	if err != nil {
		return renderProbe{}, err
	}
	defer closeEvent(event)

	if err := initializeShared(client, mixFormat, period); err != nil {
		return renderProbe{}, classifyInitializeError(err)
	}
	if err := setAudioEvent(client, event); err != nil {
		return renderProbe{}, err
	}
	bufferFrames, err := getBufferSize(client)
	if err != nil {
		return renderProbe{}, err
	}
	currentFormat, currentPeriod, err := getCurrentSharedEnginePeriod(client)
	if err != nil {
		return renderProbe{}, err
	}
	defer procCoTaskMemFree.Call(currentFormat)
	if currentPeriod != period {
		return renderProbe{}, fmt.Errorf("%w: WASAPI granted period %d, selected %d", driver.ErrFormat, currentPeriod, period)
	}
	currentBytes, _, currentChannels, currentRate, err := describeWaveFormat(currentFormat)
	if err != nil {
		return renderProbe{}, err
	}
	if currentRate != rate || currentChannels != channels || !equalBytes(currentBytes, waveBytes) {
		return renderProbe{}, fmt.Errorf("%w: WASAPI shared engine format changed during negotiation", driver.ErrFormat)
	}
	periodCount, latency, err := bufferGeometry(bufferFrames, int(period), rate)
	if err != nil {
		return renderProbe{}, err
	}

	return renderProbe{
		formatBytes:  waveBytes,
		format:       sampleFormat,
		rate:         rate,
		channels:     channels,
		period:       int(period),
		periods:      periodCount,
		bufferFrames: bufferFrames,
		latency:      latency,
	}, nil
}

func (s *wasapiStream) Params() driver.Params { return s.params }

func (s *wasapiStream) Start() error {
	if s.started || s.stopped {
		return fmt.Errorf("tymbal wasapi: stream already started or stopped")
	}
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
	if err := primeRenderBuffer(s.render, s.bufferFrames); err != nil {
		cleanupErr := s.cleanupOnStreamThread()
		s.stopped = true
		return errors.Join(err, cleanupErr)
	}
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

func (s *wasapiStream) openOnStreamThread() error {
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
	if rate != s.params.SampleRate || channels != s.params.OutChannels || sampleFormat != s.params.OutFormat || !equalBytes(waveBytes, s.wave) {
		return fmt.Errorf("%w: endpoint mix format changed after Open", driver.ErrFormat)
	}
	periods, err := getSharedPeriodRange(client, mixFormat)
	if err != nil {
		return err
	}
	if !periodAllowed(uint32(s.params.Period), periods) {
		return fmt.Errorf("%w: requested period is no longer supported", driver.ErrFormat)
	}
	if err := createStreamEvents(s); err != nil {
		return err
	}
	if err := initializeShared(client, mixFormat, uint32(s.params.Period)); err != nil {
		return classifyInitializeError(err)
	}
	if err := setAudioEvent(client, s.audioEvent); err != nil {
		return err
	}
	bufferFrames, err := getBufferSize(client)
	if err != nil {
		return err
	}
	if bufferFrames != s.bufferFrames {
		return fmt.Errorf("%w: WASAPI buffer changed from %d to %d frames after Open", driver.ErrFormat, s.bufferFrames, bufferFrames)
	}
	periodCount, latency, err := bufferGeometry(bufferFrames, s.params.Period, s.params.SampleRate)
	if err != nil {
		return err
	}
	if periodCount != s.params.Periods || latency != s.params.LatencyOut {
		return fmt.Errorf("%w: WASAPI buffer geometry changed after Open", driver.ErrFormat)
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
	if currentRate != s.params.SampleRate || currentChannels != s.params.OutChannels || !equalBytes(currentBytes, s.wave) {
		return fmt.Errorf("%w: WASAPI shared engine format changed after Open", driver.ErrFormat)
	}
	render, err := getRenderClient(client)
	if err != nil {
		return err
	}
	s.render = render
	return nil
}

func createStreamEvents(s *wasapiStream) error {
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

func (s *wasapiStream) Wait() error {
	if !s.started || s.stopped {
		return fmt.Errorf("tymbal wasapi: wait outside a running stream")
	}
	if s.interruptRequested.Load() {
		s.ready = false
		return driver.ErrInterrupted
	}
	s.ready = false
	result, _, callErr := syscall.SyscallN(
		s.waitProc,
		2,
		uintptr(unsafe.Pointer(&s.waitHandles[0])),
		0,
		infinite,
	)
	runtime.KeepAlive(&s.waitHandles)
	if result == waitObject0 {
		if s.interruptRequested.Load() {
			return driver.ErrInterrupted
		}
		s.ready = true
		return nil
	}
	if result == waitObject0+1 {
		return driver.ErrInterrupted
	}
	if result == waitFailed {
		if callErr == 0 {
			callErr = syscall.EINVAL
		}
		return fmt.Errorf("tymbal wasapi: wait for render event: %w", callErr)
	}
	return fmt.Errorf("tymbal wasapi: unexpected wait result 0x%08X", uint32(result))
}

func (s *wasapiStream) Interrupt() {
	s.interruptRequested.Store(true)
	s.interruptMu.Lock()
	if s.interruptEvent != 0 {
		_ = signalEvent(s.interruptEvent)
	}
	s.interruptMu.Unlock()
}

func (s *wasapiStream) Buffers() (in, out []byte) { return nil, s.out }

func (s *wasapiStream) Commit() error {
	if !s.ready || s.render == 0 {
		return fmt.Errorf("tymbal wasapi: commit without a render event")
	}
	s.renderBuffer = 0
	hr := comCall2(s.render, audioRenderClientGetBuffer, uintptr(s.params.Period), uintptr(unsafe.Pointer(&s.renderBuffer)))
	runtime.KeepAlive(s)
	if err := renderHRESULT("IAudioRenderClient.GetBuffer", hr); err != nil {
		if errors.Is(err, driver.ErrXrun) {
			s.dropouts++
		}
		s.ready = false
		return err
	}
	if s.renderBuffer == 0 {
		s.ready = false
		return fmt.Errorf("tymbal wasapi: render buffer returned nil")
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(s.renderBuffer)), len(s.out)), s.out)
	runtime.KeepAlive(s.out)
	hr = comCall2(s.render, audioRenderClientReleaseBuffer, uintptr(s.params.Period), 0)
	if err := renderHRESULT("IAudioRenderClient.ReleaseBuffer", hr); err != nil {
		if errors.Is(err, driver.ErrXrun) {
			s.dropouts++
		}
		s.ready = false
		return err
	}
	s.ready = false
	return nil
}

func (s *wasapiStream) Clock() (outNano, inNano int64) { return 0, 0 }

func (s *wasapiStream) Deadlines() (wakeNano, commitNano int64) { return 0, 0 }

func (s *wasapiStream) Dropouts() uint64 { return s.dropouts }

func (s *wasapiStream) Recover() error { return nil }

func (s *wasapiStream) Stop() error {
	if s.stopped {
		return nil
	}
	stopErr := s.cleanupOnStreamThread()
	s.stopped = true
	return stopErr
}

func (s *wasapiStream) cleanupOnStreamThread() error {
	var firstErr error
	if s.clientStarted && s.client != 0 {
		if err := checkHRESULT("IAudioClient.Stop", comCall0(s.client, audioClientStop)); err != nil {
			firstErr = err
		}
		s.clientStarted = false
	}
	release(s.render)
	s.render = 0
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
	s.ready = false
	return firstErr
}

func (s *wasapiStream) Close() error { return nil }

func getDevice(enumerator uintptr, id string) (uintptr, error) {
	name, err := syscall.UTF16PtrFromString(id)
	if err != nil {
		return 0, fmt.Errorf("tymbal wasapi: invalid endpoint ID: %w", err)
	}
	var device uintptr
	hr := comCall2(enumerator, immDeviceEnumeratorGetDevice, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&device)))
	runtime.KeepAlive(name)
	runtime.KeepAlive(&device)
	if err := checkHRESULT("IMMDeviceEnumerator.GetDevice", hr); err != nil {
		release(device)
		if isNotFound(err) {
			return 0, fmt.Errorf("%w: %v", driver.ErrLost, err)
		}
		return 0, err
	}
	if device == 0 {
		return 0, errors.New("tymbal wasapi: GetDevice returned nil")
	}
	return device, nil
}

func activateAudioClient3(device uintptr) (uintptr, error) {
	var client uintptr
	hr := comCall4(device, immDeviceActivate,
		uintptr(unsafe.Pointer(&iidAudioClient3Render)),
		clsctxAll,
		0,
		uintptr(unsafe.Pointer(&client)))
	runtime.KeepAlive(&iidAudioClient3Render)
	runtime.KeepAlive(&client)
	if err := checkHRESULT("IMMDevice.Activate(IAudioClient3)", hr); err != nil {
		release(client)
		var hrErr *hresultError
		if errors.As(err, &hrErr) && hrErr.code == 0x80004002 {
			return 0, fmt.Errorf("%w: IAudioClient3 requires Windows 10 or later", driver.ErrUnsupported)
		}
		return 0, err
	}
	if client == 0 {
		return 0, errors.New("tymbal wasapi: activation returned a nil IAudioClient3")
	}
	return client, nil
}

func getMixFormat(client uintptr) (uintptr, error) {
	var mixFormat uintptr
	hr := comCall1(client, audioClientGetMixFormat, uintptr(unsafe.Pointer(&mixFormat)))
	runtime.KeepAlive(&mixFormat)
	if err := checkHRESULT("IAudioClient.GetMixFormat", hr); err != nil {
		if mixFormat != 0 {
			procCoTaskMemFree.Call(mixFormat)
		}
		return 0, err
	}
	if mixFormat == 0 {
		return 0, errors.New("tymbal wasapi: GetMixFormat returned nil")
	}
	return mixFormat, nil
}

func describeWaveFormat(ptr uintptr) ([]byte, format.Format, int, int, error) {
	if ptr == 0 {
		return nil, "", 0, 0, errors.New("tymbal wasapi: nil wave format")
	}
	wave := (*waveFormatEx)(unsafe.Pointer(ptr))
	extra := int(wave.cbSize)
	if extra > maxWaveFormatBytes-18 {
		return nil, "", 0, 0, fmt.Errorf("%w: oversized WAVEFORMATEX extension", driver.ErrFormat)
	}
	total := 18 + extra
	bytes := make([]byte, total)
	copy(bytes, unsafe.Slice((*byte)(unsafe.Pointer(ptr)), total))
	channels := int(wave.channels)
	rate := int(wave.samplesPerSec)
	if channels <= 0 || rate <= 0 || wave.blockAlign == 0 {
		return nil, "", 0, 0, fmt.Errorf("%w: invalid WAVEFORMATEX geometry", driver.ErrFormat)
	}
	encoding := wave.formatTag
	validBits := wave.bitsPerSample
	if wave.formatTag == waveFormatExtensibleTag {
		if extra < 22 || total < 40 {
			return nil, "", 0, 0, fmt.Errorf("%w: incomplete WAVEFORMATEXTENSIBLE", driver.ErrFormat)
		}
		validBits = binary.LittleEndian.Uint16(bytes[18:20])
		subFormat := guid{
			data1: binary.LittleEndian.Uint32(bytes[24:28]),
			data2: binary.LittleEndian.Uint16(bytes[28:30]),
			data3: binary.LittleEndian.Uint16(bytes[30:32]),
		}
		copy(subFormat.data4[:], bytes[32:40])
		switch subFormat {
		case waveSubFormatPCM:
			encoding = waveFormatPCM
		case waveSubFormatIEEEFloat:
			encoding = waveFormatIEEEFloat
		default:
			return nil, "", 0, 0, fmt.Errorf("%w: unsupported WAVEFORMATEXTENSIBLE subformat", driver.ErrFormat)
		}
	}

	var sampleFormat format.Format
	switch {
	case encoding == waveFormatIEEEFloat && wave.bitsPerSample == 32 && validBits == 32:
		sampleFormat = format.F32LE
	case encoding == waveFormatPCM && wave.bitsPerSample == 16 && validBits == 16:
		sampleFormat = format.S16LE
	case encoding == waveFormatPCM && wave.bitsPerSample == 24 && validBits == 24:
		sampleFormat = format.S24_3LE
	case encoding == waveFormatPCM && wave.bitsPerSample == 32 && validBits == 32:
		sampleFormat = format.S32LE
	default:
		return nil, "", 0, 0, fmt.Errorf("%w: endpoint mix sample format is not directly supported", driver.ErrFormat)
	}
	if int(wave.blockAlign) != channels*format.BytesPerSample(sampleFormat) {
		return nil, "", 0, 0, fmt.Errorf("%w: endpoint mix format uses a padded sample container", driver.ErrFormat)
	}
	return bytes, sampleFormat, channels, rate, nil
}

func getSharedPeriodRange(client, wave uintptr) (sharedPeriodRange, error) {
	var periods sharedPeriodRange
	hr := comCall5(client, audioClient3GetSharedModeEnginePeriod,
		wave,
		uintptr(unsafe.Pointer(&periods.defaultFrames)),
		uintptr(unsafe.Pointer(&periods.fundamentalFrames)),
		uintptr(unsafe.Pointer(&periods.minFrames)),
		uintptr(unsafe.Pointer(&periods.maxFrames)))
	runtime.KeepAlive(&periods)
	if err := checkHRESULT("IAudioClient3.GetSharedModeEnginePeriod", hr); err != nil {
		return sharedPeriodRange{}, err
	}
	if periods.fundamentalFrames == 0 || periods.minFrames == 0 || periods.maxFrames < periods.minFrames {
		return sharedPeriodRange{}, fmt.Errorf("%w: invalid WASAPI engine period range", driver.ErrFormat)
	}
	return periods, nil
}

func periodAllowed(requested uint32, periods sharedPeriodRange) bool {
	return requested > 0 && periods.fundamentalFrames > 0 &&
		requested >= periods.minFrames && requested <= periods.maxFrames &&
		requested%periods.fundamentalFrames == 0
}

// nearestAllowedPeriod picks the closest fundamental-period multiple inside
// the engine's range. Ties round upward; zero means the range has no multiple.
func nearestAllowedPeriod(requested uint64, periods sharedPeriodRange) uint32 {
	fundamental := uint64(periods.fundamentalFrames)
	if fundamental == 0 || periods.minFrames == 0 || periods.maxFrames < periods.minFrames {
		return 0
	}
	minimum := uint64(periods.minFrames)
	minimumAllowed := ((minimum + fundamental - 1) / fundamental) * fundamental
	maximumAllowed := uint64(periods.maxFrames) / fundamental * fundamental
	if minimumAllowed > maximumAllowed || maximumAllowed > uint64(^uint32(0)) {
		return 0
	}
	multiple := requested / fundamental
	if requested%fundamental >= (fundamental+1)/2 {
		multiple++
	}
	selected := multiple * fundamental
	if selected < minimumAllowed {
		selected = minimumAllowed
	} else if selected > maximumAllowed {
		selected = maximumAllowed
	}
	return uint32(selected)
}

func initializeShared(client, wave uintptr, period uint32) error {
	hr := comCall4(client, audioClient3InitializeSharedAudioStream,
		audclntStreamEventCallback,
		uintptr(period),
		wave,
		0)
	return checkHRESULT("IAudioClient3.InitializeSharedAudioStream", hr)
}

func classifyInitializeError(err error) error {
	if err == nil || errors.Is(err, driver.ErrLost) {
		return err
	}
	var hrErr *hresultError
	if errors.As(err, &hrErr) && hrErr.code == 0x80004002 {
		return fmt.Errorf("%w: IAudioClient3 shared streams are unavailable", driver.ErrUnsupported)
	}
	return fmt.Errorf("%w: %v", driver.ErrFormat, err)
}

func setAudioEvent(client, event uintptr) error {
	return checkHRESULT("IAudioClient.SetEventHandle", comCall1(client, audioClientSetEventHandle, event))
}

func getBufferSize(client uintptr) (uint32, error) {
	var frames uint32
	hr := comCall1(client, audioClientGetBufferSize, uintptr(unsafe.Pointer(&frames)))
	runtime.KeepAlive(&frames)
	if err := checkHRESULT("IAudioClient.GetBufferSize", hr); err != nil {
		return 0, err
	}
	if frames == 0 {
		return 0, fmt.Errorf("%w: WASAPI reported an empty render buffer", driver.ErrFormat)
	}
	return frames, nil
}

func getCurrentSharedEnginePeriod(client uintptr) (uintptr, uint32, error) {
	var wave uintptr
	var period uint32
	hr := comCall2(client, audioClient3GetCurrentSharedModeEnginePeriod,
		uintptr(unsafe.Pointer(&wave)), uintptr(unsafe.Pointer(&period)))
	runtime.KeepAlive(&wave)
	runtime.KeepAlive(&period)
	if err := checkHRESULT("IAudioClient3.GetCurrentSharedModeEnginePeriod", hr); err != nil {
		if wave != 0 {
			procCoTaskMemFree.Call(wave)
		}
		return 0, 0, err
	}
	if wave == 0 || period == 0 {
		if wave != 0 {
			procCoTaskMemFree.Call(wave)
		}
		return 0, 0, fmt.Errorf("%w: WASAPI reported an empty current engine period", driver.ErrFormat)
	}
	return wave, period, nil
}

func bufferGeometry(bufferFrames uint32, period, rate int) (int, time.Duration, error) {
	if bufferFrames == 0 || period <= 0 || rate <= 0 || uint64(bufferFrames) < uint64(period) || bufferFrames%uint32(period) != 0 {
		return 0, 0, fmt.Errorf("%w: WASAPI buffer of %d frames cannot provide complete %d-frame callbacks", driver.ErrFormat, bufferFrames, period)
	}
	periods := uint64(bufferFrames) / uint64(period)
	if periods == 0 || periods > uint64(int(^uint(0)>>1)) {
		return 0, 0, fmt.Errorf("%w: WASAPI buffer geometry overflows", driver.ErrFormat)
	}
	latency := time.Duration(uint64(bufferFrames) * uint64(time.Second) / uint64(rate))
	return int(periods), latency, nil
}

func getRenderClient(client uintptr) (uintptr, error) {
	var render uintptr
	hr := comCall2(client, audioClientGetService,
		uintptr(unsafe.Pointer(&iidAudioRenderClient)), uintptr(unsafe.Pointer(&render)))
	runtime.KeepAlive(&iidAudioRenderClient)
	runtime.KeepAlive(&render)
	if err := checkHRESULT("IAudioClient.GetService(IAudioRenderClient)", hr); err != nil {
		release(render)
		return 0, err
	}
	if render == 0 {
		return 0, errors.New("tymbal wasapi: GetService returned nil render client")
	}
	return render, nil
}

func primeRenderBuffer(render uintptr, frames uint32) error {
	var native uintptr
	hr := comCall2(render, audioRenderClientGetBuffer, uintptr(frames), uintptr(unsafe.Pointer(&native)))
	runtime.KeepAlive(&native)
	if err := renderHRESULT("IAudioRenderClient.GetBuffer(prime)", hr); err != nil {
		return err
	}
	if native == 0 {
		return errors.New("tymbal wasapi: render priming buffer returned nil")
	}
	if err := renderHRESULT("IAudioRenderClient.ReleaseBuffer(prime)", comCall2(render, audioRenderClientReleaseBuffer, uintptr(frames), audclntBufferFlagSilent)); err != nil {
		return err
	}
	return nil
}

func renderHRESULT(op string, hr uintptr) error {
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

func createEvent() (uintptr, error) {
	handle, _, callErr := procCreateEventW.Call(0, 0, 0, 0)
	if handle == 0 {
		if callErr == nil {
			callErr = syscall.EINVAL
		}
		return 0, fmt.Errorf("tymbal wasapi: CreateEventW: %w", callErr)
	}
	return handle, nil
}

func signalEvent(handle uintptr) error {
	result, _, callErr := procSetEvent.Call(handle)
	if result != 0 {
		return nil
	}
	if callErr == nil {
		callErr = syscall.EINVAL
	}
	return fmt.Errorf("tymbal wasapi: SetEvent: %w", callErr)
}

func closeEvent(handle uintptr) error {
	if handle == 0 {
		return nil
	}
	result, _, callErr := procCloseHandle.Call(handle)
	if result != 0 {
		return nil
	}
	if callErr == nil {
		callErr = syscall.EINVAL
	}
	return fmt.Errorf("tymbal wasapi: CloseHandle: %w", callErr)
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func errWithCleanup(err, cleanupErr error) error {
	if cleanupErr == nil {
		return err
	}
	return errors.Join(err, cleanupErr)
}
