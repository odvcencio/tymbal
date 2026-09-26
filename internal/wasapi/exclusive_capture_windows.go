//go:build windows

package wasapi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
	audioformat "m31labs.dev/tymbal/internal/format"
)

const (
	audclntShareModeExclusive = 1

	audclntErrDeviceInUse             = 0x8889000a
	audclntErrExclusiveModeNotAllowed = 0x8889000e
	audclntErrBufferSizeNotAligned    = 0x88890019
	audclntErrBufferError             = 0x88890018

	audioClientInitialize          = 3
	audioClientIsFormatSupported   = 7
	audioClientGetDevicePeriod     = 9
	exclusiveMaxBufferDurationHNS  = int64(5 * 10_000_000)
	referenceTimeUnitsPerSecond    = uint64(10_000_000)
	waveFormatExtensibleByteLength = 40
)

var errExclusiveCapturePacketUnavailable = errors.New("tymbal wasapi: exclusive capture packet is not available yet")

type exclusiveCaptureProbe struct {
	formatBytes  []byte
	format       audioformat.Format
	rate         int
	channels     int
	period       int
	periods      int
	bufferFrames uint32
	bufferHNS    int64
	latency      time.Duration
	blockAlign   int
}

type exclusiveWaveFormat struct {
	bytes      []byte
	format     audioformat.Format
	blockAlign int
}

// exclusiveCaptureStream reuses the fixed-period packet stager and stream
// lifecycle methods shared by capture, while Start owns exclusive negotiation.
type exclusiveCaptureStream struct {
	captureStream
	bufferHNS int64
}

var _ driver.Stream = (*exclusiveCaptureStream)(nil)

func openExclusiveCaptureStream(req driver.Request) (driver.Stream, error) {
	if req.Input == nil || req.Output != nil {
		return nil, driver.ErrUnsupported
	}
	if req.Input.ID == "" || req.InChannels <= 0 || req.SampleRate <= 0 || req.Period <= 0 || req.Periods <= 0 {
		return nil, driver.ErrFormat
	}

	probe, err := withSTA(func() (exclusiveCaptureProbe, error) { return probeExclusiveCapture(req) })
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
	return &exclusiveCaptureStream{
		captureStream: captureStream{
			deviceID:     req.Input.ID,
			params:       params,
			wave:         probe.formatBytes,
			stager:       *stager,
			bufferFrames: probe.bufferFrames,
		},
		bufferHNS: probe.bufferHNS,
	}, nil
}

func probeExclusiveCapture(req driver.Request) (exclusiveCaptureProbe, error) {
	enumerator, err := createEnumerator()
	if err != nil {
		return exclusiveCaptureProbe{}, err
	}
	defer release(enumerator)

	device, err := getDevice(enumerator, req.Input.ID)
	if err != nil {
		return exclusiveCaptureProbe{}, err
	}
	defer release(device)

	client, err := activateAudioClient(device)
	if err != nil {
		return exclusiveCaptureProbe{}, err
	}
	defer func() { release(client) }()

	formats, err := exclusiveFormatCandidates(client, req.SampleRate, req.InChannels)
	if err != nil {
		return exclusiveCaptureProbe{}, err
	}
	minimumPeriod, err := getExclusiveMinimumPeriod(client)
	if err != nil {
		return exclusiveCaptureProbe{}, err
	}
	duration, err := requestedExclusiveBufferDuration(req.Period, req.Periods, req.SampleRate, minimumPeriod)
	if err != nil {
		return exclusiveCaptureProbe{}, err
	}

	selected, err := selectExclusiveFormat(formats, func(candidate []byte) (bool, error) {
		supported, probeErr := isExclusiveFormatSupported(client, uintptr(unsafe.Pointer(&candidate[0])))
		runtime.KeepAlive(candidate)
		return supported, probeErr
	})
	if err != nil {
		return exclusiveCaptureProbe{}, err
	}

	bufferFrames, actualHNS, err := initializeExclusiveAlignedForDevice(device, &client, selected.bytes, duration, req.SampleRate)
	if err != nil {
		return exclusiveCaptureProbe{}, classifyExclusiveError("IAudioClient.Initialize", err)
	}
	event, err := createEvent()
	if err != nil {
		return exclusiveCaptureProbe{}, err
	}
	defer closeEvent(event)
	if err := setAudioEvent(client, event); err != nil {
		return exclusiveCaptureProbe{}, classifyExclusiveError("IAudioClient.SetEventHandle", err)
	}

	periods, _, err := captureBufferGeometry(bufferFrames, req.Period, req.SampleRate)
	if err != nil {
		return exclusiveCaptureProbe{}, err
	}
	latency, err := getStreamLatency(client)
	if err != nil {
		return exclusiveCaptureProbe{}, err
	}
	return exclusiveCaptureProbe{
		formatBytes:  selected.bytes,
		format:       selected.format,
		rate:         req.SampleRate,
		channels:     req.InChannels,
		period:       req.Period,
		periods:      periods,
		bufferFrames: bufferFrames,
		bufferHNS:    actualHNS,
		latency:      latency,
		blockAlign:   selected.blockAlign,
	}, nil
}

func (s *exclusiveCaptureStream) Start() error {
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
	s.qpcOffsetNano, s.qpcOffsetValid = captureQPCOffset()
	if err := checkHRESULT("IAudioClient.Start", comCall0(s.client, audioClientStart)); err != nil {
		cleanupErr := s.cleanupOnStreamThread()
		s.stopped = true
		return errWithCleanup(classifyExclusiveError("IAudioClient.Start", err), cleanupErr)
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

func (s *exclusiveCaptureStream) openExclusiveOnStreamThread() error {
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

	supported, err := isExclusiveFormatSupported(client, uintptr(unsafe.Pointer(&s.wave[0])))
	if err != nil {
		return classifyExclusiveError("IAudioClient.IsFormatSupported", err)
	}
	if !supported {
		return fmt.Errorf("%w: exclusive endpoint no longer supports the negotiated format", driver.ErrFormat)
	}
	bufferFrames, actualHNS, err := initializeExclusiveAlignedForDevice(s.device, &s.client, s.wave, s.bufferHNS, s.params.SampleRate)
	if err != nil {
		return classifyExclusiveError("IAudioClient.Initialize", err)
	}
	if bufferFrames != s.bufferFrames || actualHNS != s.bufferHNS {
		return fmt.Errorf("%w: exclusive capture buffer changed from %d frames at %d hns to %d frames at %d hns after Open", driver.ErrFormat, s.bufferFrames, s.bufferHNS, bufferFrames, actualHNS)
	}
	periods, _, err := captureBufferGeometry(bufferFrames, s.params.Period, s.params.SampleRate)
	if err != nil {
		return err
	}
	latency, err := getStreamLatency(s.client)
	if err != nil {
		return err
	}
	if periods != s.params.Periods || latency != s.params.LatencyIn {
		return fmt.Errorf("%w: exclusive capture buffer geometry or stream latency changed after Open", driver.ErrFormat)
	}
	if err := createExclusiveCaptureEvents(&s.captureStream); err != nil {
		return err
	}
	if err := setAudioEvent(s.client, s.audioEvent); err != nil {
		return classifyExclusiveError("IAudioClient.SetEventHandle", err)
	}
	capture, err := getCaptureClient(s.client)
	if err != nil {
		return err
	}
	s.capture = capture
	return nil
}

func (s *exclusiveCaptureStream) Wait() error {
	if !s.started || s.stopped {
		return fmt.Errorf("tymbal wasapi: wait outside a running exclusive capture stream")
	}
	if s.stager.ready {
		return fmt.Errorf("tymbal wasapi: wait before committing the previous exclusive capture period")
	}
	if s.interruptRequested.Load() {
		return driver.ErrInterrupted
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
			return fmt.Errorf("tymbal wasapi: wait for exclusive capture event: %w", callErr)
		}
		if result != waitObject0 {
			return fmt.Errorf("tymbal wasapi: unexpected exclusive capture wait result 0x%08X", uint32(result))
		}
		if err := s.drainExclusiveCapturePackets(); err != nil && !errors.Is(err, errExclusiveCapturePacketUnavailable) {
			return err
		}
		if s.stager.preparePeriod() {
			return nil
		}
	}
}

func (s *exclusiveCaptureStream) drainExclusiveCapturePackets() error {
	for {
		s.packetNative = 0
		s.packetFrames = 0
		s.packetFlags = 0
		s.packetDevicePos = 0
		s.packetQPCPosition = 0
		hr := comCall5(s.capture, audioCaptureClientGetBuffer,
			uintptr(unsafe.Pointer(&s.packetNative)),
			uintptr(unsafe.Pointer(&s.packetFrames)),
			uintptr(unsafe.Pointer(&s.packetFlags)),
			uintptr(unsafe.Pointer(&s.packetDevicePos)),
			uintptr(unsafe.Pointer(&s.packetQPCPosition)))
		runtime.KeepAlive(s)
		if uint32(hr) == audclntErrBufferError {
			// Exclusive event capture can signal before GetBuffer can expose the
			// next packet. Wait for the next event instead of spinning.
			return errExclusiveCapturePacketUnavailable
		}
		if err := captureHRESULT("IAudioCaptureClient.GetBuffer", hr); err != nil {
			return err
		}
		if s.packetFrames == 0 {
			// AUDCLNT_S_BUFFER_EMPTY does not require ReleaseBuffer.
			return errExclusiveCapturePacketUnavailable
		}

		var packet []byte
		packetErr := error(nil)
		if s.packetFrames != s.bufferFrames {
			packetErr = fmt.Errorf("%w: exclusive capture returned %d frames, expected full buffer %d", driver.ErrFormat, s.packetFrames, s.bufferFrames)
		} else if s.packetFlags&audclntBufferFlagSilent == 0 {
			byteCount, sizeErr := capturePacketByteCount(s.packetFrames, s.stager.blockAlign)
			if sizeErr != nil {
				packetErr = sizeErr
			} else if s.packetNative == 0 {
				packetErr = errors.New("tymbal wasapi: non-silent exclusive capture packet returned nil data")
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

func createExclusiveCaptureEvents(s *captureStream) error {
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

func activateAudioClient(device uintptr) (uintptr, error) {
	var client uintptr
	hr := comCall4(device, immDeviceActivate,
		uintptr(unsafe.Pointer(&iidAudioClient)),
		clsctxAll,
		0,
		uintptr(unsafe.Pointer(&client)))
	runtime.KeepAlive(&iidAudioClient)
	runtime.KeepAlive(&client)
	if err := checkHRESULT("IMMDevice.Activate(IAudioClient)", hr); err != nil {
		release(client)
		return 0, classifyExclusiveError("IMMDevice.Activate(IAudioClient)", err)
	}
	if client == 0 {
		return 0, errors.New("tymbal wasapi: activation returned a nil IAudioClient")
	}
	return client, nil
}

// isExclusiveFormatSupported checks one exact format. Exclusive mode requires
// a nil closest-match pointer; S_OK is the only exact-support result.
func isExclusiveFormatSupported(client, wave uintptr) (bool, error) {
	hr := comCall3(client, audioClientIsFormatSupported,
		audclntShareModeExclusive, wave, 0)
	if int32(hr) >= 0 {
		return exactExclusiveFormatResult(hr), nil
	}
	if uint32(hr) == audclntErrUnsupportedFormat {
		return false, nil
	}
	return false, classifyExclusiveError("IAudioClient.IsFormatSupported", checkHRESULT("IAudioClient.IsFormatSupported", hr))
}

func exactExclusiveFormatResult(hr uintptr) bool { return uint32(hr) == 0 }

func exclusiveFormatCandidates(client uintptr, rate, channels int) ([]exclusiveWaveFormat, error) {
	mixFormat, err := getMixFormat(client)
	if err != nil {
		return nil, err
	}
	defer procCoTaskMemFree.Call(mixFormat)
	return exclusiveFormatCandidatesFromMix(mixFormat, rate, channels)
}

func exclusiveFormatCandidatesFromMix(mixFormat uintptr, rate, channels int) ([]exclusiveWaveFormat, error) {
	candidates := make([]exclusiveWaveFormat, 0, 5)
	if mixFormat == 0 {
		return nil, fmt.Errorf("%w: endpoint returned an empty mix format", driver.ErrFormat)
	}
	if bytes, sampleFormat, mixChannels, mixRate, describeErr := describeWaveFormat(mixFormat); describeErr == nil && mixRate == rate && mixChannels == channels {
		blockAlign := int((*waveFormatEx)(unsafe.Pointer(mixFormat)).blockAlign)
		candidates = append(candidates, exclusiveWaveFormat{bytes: bytes, format: sampleFormat, blockAlign: blockAlign})
	}

	for _, sampleFormat := range []audioformat.Format{audioformat.F32LE, audioformat.S16LE, audioformat.S24_3LE, audioformat.S32LE} {
		wave, blockAlign, buildErr := makeExclusiveWaveFormat(rate, channels, sampleFormat)
		if buildErr != nil {
			return nil, buildErr
		}
		duplicate := false
		for _, previous := range candidates {
			if equalBytes(previous.bytes, wave) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			candidates = append(candidates, exclusiveWaveFormat{bytes: wave, format: sampleFormat, blockAlign: blockAlign})
		}
	}
	return candidates, nil
}

// supportsAnyExclusiveFormat reports whether any direct format candidate at
// the endpoint's nominal rate and channel count is accepted exactly.
func supportsAnyExclusiveFormat(client, mixFormat uintptr, rate, channels int) (bool, error) {
	candidates, err := exclusiveFormatCandidatesFromMix(mixFormat, rate, channels)
	if err != nil {
		return false, err
	}
	for _, candidate := range candidates {
		supported, probeErr := isExclusiveFormatSupported(client, uintptr(unsafe.Pointer(&candidate.bytes[0])))
		runtime.KeepAlive(candidate.bytes)
		if probeErr != nil {
			return false, probeErr
		}
		if supported {
			return true, nil
		}
	}
	return false, nil
}

func makeExclusiveWaveFormat(rate, channels int, sampleFormat audioformat.Format) ([]byte, int, error) {
	if rate <= 0 || uint64(rate) > math.MaxUint32 || channels <= 0 || channels > math.MaxUint16 {
		return nil, 0, fmt.Errorf("%w: invalid exclusive format geometry", driver.ErrFormat)
	}
	bytesPerSample := audioformat.BytesPerSample(sampleFormat)
	if bytesPerSample == 0 || channels > math.MaxUint16/bytesPerSample {
		return nil, 0, fmt.Errorf("%w: unsupported exclusive sample format geometry", driver.ErrFormat)
	}
	blockAlign := channels * bytesPerSample
	if uint64(rate)*uint64(blockAlign) > math.MaxUint32 {
		return nil, 0, fmt.Errorf("%w: exclusive average byte rate overflows WAVEFORMATEX", driver.ErrFormat)
	}
	bits := uint16(bytesPerSample * 8)
	bytes := make([]byte, waveFormatExtensibleByteLength)
	binary.LittleEndian.PutUint16(bytes[0:2], waveFormatExtensibleTag)
	binary.LittleEndian.PutUint16(bytes[2:4], uint16(channels))
	binary.LittleEndian.PutUint32(bytes[4:8], uint32(rate))
	binary.LittleEndian.PutUint32(bytes[8:12], uint32(rate*blockAlign))
	binary.LittleEndian.PutUint16(bytes[12:14], uint16(blockAlign))
	binary.LittleEndian.PutUint16(bytes[14:16], bits)
	binary.LittleEndian.PutUint16(bytes[16:18], 22)
	binary.LittleEndian.PutUint16(bytes[18:20], bits)
	binary.LittleEndian.PutUint32(bytes[20:24], exclusiveChannelMask(channels))
	subFormat := waveSubFormatIEEEFloat
	if sampleFormat != audioformat.F32LE {
		subFormat = waveSubFormatPCM
	}
	binary.LittleEndian.PutUint32(bytes[24:28], subFormat.data1)
	binary.LittleEndian.PutUint16(bytes[28:30], subFormat.data2)
	binary.LittleEndian.PutUint16(bytes[30:32], subFormat.data3)
	copy(bytes[32:40], subFormat.data4[:])
	return bytes, blockAlign, nil
}

func exclusiveChannelMask(channels int) uint32 {
	switch channels {
	case 1:
		return 0x00000004 // SPEAKER_FRONT_CENTER
	case 2:
		return 0x00000003 // SPEAKER_FRONT_LEFT | SPEAKER_FRONT_RIGHT
	case 3:
		return 0x00000007
	case 4:
		return 0x00000033
	case 5:
		return 0x00000037
	case 6:
		return 0x0000003f
	case 7:
		return 0x00000637
	case 8:
		return 0x0000063f
	default:
		return 0 // WAVEFORMATEXTENSIBLE permits an unspecified channel mask.
	}
}

func selectExclusiveFormat(candidates []exclusiveWaveFormat, supports func([]byte) (bool, error)) (exclusiveWaveFormat, error) {
	for _, candidate := range candidates {
		if len(candidate.bytes) == 0 {
			continue
		}
		supported, err := supports(candidate.bytes)
		if err != nil {
			return exclusiveWaveFormat{}, err
		}
		if supported {
			return candidate, nil
		}
	}
	return exclusiveWaveFormat{}, fmt.Errorf("%w: endpoint has no exact exclusive format for the requested capture", driver.ErrFormat)
}

func getExclusiveMinimumPeriod(client uintptr) (int64, error) {
	var defaultPeriod, minimumPeriod int64
	hr := comCall2(client, audioClientGetDevicePeriod,
		uintptr(unsafe.Pointer(&defaultPeriod)), uintptr(unsafe.Pointer(&minimumPeriod)))
	runtime.KeepAlive(&defaultPeriod)
	runtime.KeepAlive(&minimumPeriod)
	if err := checkHRESULT("IAudioClient.GetDevicePeriod", hr); err != nil {
		return 0, classifyExclusiveError("IAudioClient.GetDevicePeriod", err)
	}
	if minimumPeriod <= 0 {
		return 0, fmt.Errorf("%w: WASAPI returned an invalid exclusive minimum device period", driver.ErrFormat)
	}
	return minimumPeriod, nil
}

func requestedExclusiveBufferDuration(period, periods, rate int, minimumPeriodHNS int64) (int64, error) {
	if period <= 0 || periods <= 0 || rate <= 0 || minimumPeriodHNS <= 0 {
		return 0, fmt.Errorf("%w: invalid exclusive buffer request", driver.ErrFormat)
	}
	if uint64(period) > math.MaxUint64/uint64(periods) {
		return 0, fmt.Errorf("%w: exclusive buffer frame request overflows", driver.ErrFormat)
	}
	frames := uint64(period) * uint64(periods)
	minimumFrames, ok := hnsToFramesCeil(minimumPeriodHNS, uint32(rate))
	if !ok {
		return 0, fmt.Errorf("%w: exclusive minimum device period overflows", driver.ErrFormat)
	}
	if frames < minimumFrames {
		frames = minimumFrames
	}
	duration, ok := framesToHNS(frames, uint32(rate))
	if !ok || duration <= 0 || duration > exclusiveMaxBufferDurationHNS {
		return 0, fmt.Errorf("%w: exclusive event buffer duration is outside the supported range", driver.ErrFormat)
	}
	return duration, nil
}

func framesToHNS(frames uint64, rate uint32) (int64, bool) {
	if frames == 0 || rate == 0 {
		return 0, false
	}
	whole := frames / uint64(rate)
	remainder := frames % uint64(rate)
	if whole > uint64(math.MaxInt64)/referenceTimeUnitsPerSecond {
		return 0, false
	}
	value := whole * referenceTimeUnitsPerSecond
	fraction := (remainder*referenceTimeUnitsPerSecond + uint64(rate)/2) / uint64(rate)
	if value > uint64(math.MaxInt64)-fraction {
		return 0, false
	}
	return int64(value + fraction), true
}

func hnsToFramesCeil(hns int64, rate uint32) (uint64, bool) {
	if hns <= 0 || rate == 0 {
		return 0, false
	}
	units := uint64(hns)
	whole := units / referenceTimeUnitsPerSecond
	remainder := units % referenceTimeUnitsPerSecond
	if whole > math.MaxUint64/uint64(rate) {
		return 0, false
	}
	frames := whole * uint64(rate)
	fractionNumerator := remainder * uint64(rate)
	fraction := (fractionNumerator + referenceTimeUnitsPerSecond - 1) / referenceTimeUnitsPerSecond
	if frames > math.MaxUint64-fraction {
		return 0, false
	}
	return frames + fraction, true
}

type exclusiveInitializeOps struct {
	initialize func(client uintptr, duration int64) uintptr
	bufferSize func(client uintptr) (uint32, error)
	release    func(client uintptr)
	activate   func() (uintptr, error)
}

func initializeExclusiveAligned(client *uintptr, duration int64, rate int, ops exclusiveInitializeOps) (uint32, int64, error) {
	if client == nil || *client == 0 || duration <= 0 || rate <= 0 || ops.initialize == nil || ops.bufferSize == nil || ops.release == nil || ops.activate == nil {
		return 0, 0, fmt.Errorf("%w: invalid exclusive initialization state", driver.ErrFormat)
	}
	hr := ops.initialize(*client, duration)
	if uint32(hr) == audclntErrBufferSizeNotAligned {
		alignedFrames, err := ops.bufferSize(*client)
		if err != nil {
			return 0, 0, err
		}
		alignedDuration, ok := framesToHNS(uint64(alignedFrames), uint32(rate))
		if !ok {
			return 0, 0, fmt.Errorf("%w: aligned exclusive buffer duration overflows", driver.ErrFormat)
		}
		ops.release(*client)
		*client = 0
		reactivated, err := ops.activate()
		if err != nil {
			return 0, 0, err
		}
		if reactivated == 0 {
			return 0, 0, errors.New("tymbal wasapi: exclusive client reactivation returned nil")
		}
		*client = reactivated
		duration = alignedDuration
		hr = ops.initialize(*client, duration)
	}
	if err := checkHRESULT("IAudioClient.Initialize", hr); err != nil {
		return 0, duration, err
	}
	bufferFrames, err := ops.bufferSize(*client)
	if err != nil {
		return 0, duration, err
	}
	return bufferFrames, duration, nil
}

func initializeExclusiveAlignedForDevice(device uintptr, client *uintptr, wave []byte, duration int64, rate int) (uint32, int64, error) {
	if len(wave) == 0 {
		return 0, 0, fmt.Errorf("%w: empty exclusive wave format", driver.ErrFormat)
	}
	ops := exclusiveInitializeOps{
		initialize: func(current uintptr, hns int64) uintptr {
			hr := comCall6(current, audioClientInitialize,
				audclntShareModeExclusive,
				audclntStreamEventCallback,
				uintptr(hns), uintptr(hns),
				uintptr(unsafe.Pointer(&wave[0])), 0)
			runtime.KeepAlive(wave)
			return hr
		},
		bufferSize: getCaptureBufferSize,
		release:    release,
		activate: func() (uintptr, error) {
			return activateAudioClient(device)
		},
	}
	return initializeExclusiveAligned(client, duration, rate, ops)
}

func classifyExclusiveError(op string, err error) error {
	if err == nil || errors.Is(err, driver.ErrLost) {
		return err
	}
	var hrErr *hresultError
	if errors.As(err, &hrErr) {
		switch hrErr.code {
		case audclntErrDeviceInUse:
			return fmt.Errorf("%w: exclusive capture endpoint is busy (%s)", driver.ErrBusy, op)
		case audclntErrExclusiveModeNotAllowed:
			return fmt.Errorf("%w: endpoint settings disable exclusive capture (%s)", driver.ErrUnsupported, op)
		case audclntErrUnsupportedFormat:
			return fmt.Errorf("%w: endpoint does not support the requested exclusive format (%s)", driver.ErrFormat, op)
		case audclntErrBufferSizeNotAligned:
			return fmt.Errorf("%w: endpoint rejected the aligned exclusive buffer (%s)", driver.ErrFormat, op)
		case audclntErrInvalidDevicePeriod:
			return fmt.Errorf("%w: endpoint rejected the exclusive device period (%s)", driver.ErrFormat, op)
		}
	}
	return err
}
