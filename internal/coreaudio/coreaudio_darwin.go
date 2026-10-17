//go:build darwin && (amd64 || arm64)

// Package coreaudio implements macOS Core Audio endpoint discovery and AUHAL
// output streams without cgo and without a run-time FFI library. It does not
// use cgo, so there is no C preamble and no C compiler in the build. It does
// not use a dlopen-style loader library such as ebitengine's either. It builds
// with CGO_ENABLED=0, and cross-compiles from any host.
//
// # Calling Core Audio
//
// The technique is the one the Go standard library uses for its own macOS
// frameworks. crypto/x509/internal/macos calls CoreFoundation and
// Security.framework like this, and package syscall reaches libSystem the same
// way (golang.org/x/sys/unix, Code-Hex/battery and others copy it):
//
//   - A `//go:cgo_import_dynamic` directive names a symbol and the framework
//     that exports it. The directive is permitted in ordinary Go files
//     (cmd/compile/internal/noder). The internal linker adds the framework to
//     the binary's load commands and binds the symbol when dyld starts the
//     process, so cross-compiling from Linux needs no macOS SDK.
//   - A one-line assembly stub per symbol (`JMP symbol(SB)`) gives Go an
//     address to call, published to Go through a DATA symbol.
//   - `//go:linkname` to syscall.syscall and syscall.syscall6, which the
//     runtime keeps stable for x/sys (go.dev/issue/67401), calls that address
//     on the system stack with the C ABI and tells the scheduler the thread is
//     in a system call. This needs neither runtime.cgocall nor iscgo.
//
// bind_darwin.go and bind_darwin.s hold all of it. Background and the options
// that were rejected are in the research report
// hypha://m31labs/tymbal/research/2026-10-16-no-cgo-coreaudio-options.
//
// # Core Audio calling back
//
// The standard library's frameworks never call back into Go, so this is the
// part that goes beyond it. An AUHAL render callback runs on a real-time
// thread that Core Audio owns and Go has never seen. A binary built without
// cgo cannot enter the Go runtime from such a thread (there is no g, and no
// extra M to adopt it). So the callback is not Go. It is a C-ABI function
// written in assembly (render_darwin_arm64.s and render_darwin_amd64.s): it
// copies frames out of a ring buffer, zero-fills any shortfall, publishes how
// much it consumed, and signals a Mach semaphore. It touches only the ctrl
// block and the ring (ctrl_darwin.go), which live in an mmap'd arena outside
// the Go heap, and it reaches memcpy, memset and semaphore_signal through
// pointers stored in the ctrl block.
//
// The Go side is an ordinary stream thread. It waits on the semaphore, renders
// one period into the ring ahead of Core Audio, and publishes it with an
// atomic store (stream_darwin.go). The Linux and Windows backends wait on the
// device and write straight into its buffer. Here Core Audio owns the thread
// that runs on the device's clock, so Go works a few periods ahead of it, the
// depth of the ring, and the output latency includes that depth.
//
// # Status
//
// Output only, on the default or any other output device, at the device's
// current nominal sample rate, as interleaved Float32. Not yet implemented:
// input and duplex streams, device-change events (Watch), real-time thread
// policy and the IO workgroup, and the ten-minute soak that gates them.
//
// # Linking
//
// The Go linker links internally and records the three frameworks itself. That
// holds with CGO_ENABLED=0, and with CGO_ENABLED=1 as long as the only cgo in
// the program is the standard library's (net, os/user). A program that also
// contains cgo code of its own is linked externally, and the toolchain does not
// let a package outside the standard library pass -framework flags
// (//go:cgo_ldflag is refused), so that build fails with undefined
// _AudioObject* symbols unless it sets
//
//	CGO_LDFLAGS="-framework CoreAudio -framework AudioToolbox -framework CoreFoundation"
package coreaudio

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
)

// errNoDevice reports that the system has no audio device to use, such as a
// headless CI machine. Tests skip on it.
var errNoDevice = errors.New("coreaudio: no audio device")

// Property selectors, scopes and object IDs, from CoreAudio/AudioHardware.h
// and AudioHardwareBase.h. Selectors are four-character codes.
const (
	objectSystem = 1 // kAudioObjectSystemObject

	scopeGlobal = 'g'<<24 | 'l'<<16 | 'o'<<8 | 'b'
	scopeInput  = 'i'<<24 | 'n'<<16 | 'p'<<8 | 't'
	scopeOutput = 'o'<<24 | 'u'<<16 | 't'<<8 | 'p'

	selDevices             = 'd'<<24 | 'e'<<16 | 'v'<<8 | '#' // kAudioHardwarePropertyDevices
	selDefaultOutputDevice = 'd'<<24 | 'O'<<16 | 'u'<<8 | 't' // kAudioHardwarePropertyDefaultOutputDevice
	selDefaultInputDevice  = 'd'<<24 | 'I'<<16 | 'n'<<8 | ' ' // kAudioHardwarePropertyDefaultInputDevice
	selRunLoop             = 'r'<<24 | 'n'<<16 | 'l'<<8 | 'p' // kAudioHardwarePropertyRunLoop
	selObjectName          = 'l'<<24 | 'n'<<16 | 'a'<<8 | 'm' // kAudioObjectPropertyName
	selDeviceUID           = 'u'<<24 | 'i'<<16 | 'd'<<8 | ' ' // kAudioDevicePropertyDeviceUID
	selDeviceIsAlive       = 'l'<<24 | 'i'<<16 | 'v'<<8 | 'n' // kAudioDevicePropertyDeviceIsAlive
	selStreamConfiguration = 's'<<24 | 'l'<<16 | 'a'<<8 | 'y' // kAudioDevicePropertyStreamConfiguration
	selNominalSampleRate   = 'n'<<24 | 's'<<16 | 'r'<<8 | 't' // kAudioDevicePropertyNominalSampleRate
	selBufferFrameSize     = 'f'<<24 | 's'<<16 | 'i'<<8 | 'z' // kAudioDevicePropertyBufferFrameSize
	selBufferFrameSizeRng  = 'f'<<24 | 's'<<16 | 'z'<<8 | '#' // kAudioDevicePropertyBufferFrameSizeRange
	selDeviceLatency       = 'l'<<24 | 't'<<16 | 'n'<<8 | 'c' // kAudioDevicePropertyLatency
	selSafetyOffset        = 's'<<24 | 'a'<<16 | 'f'<<8 | 't' // kAudioDevicePropertySafetyOffset
	selDeviceStreams       = 's'<<24 | 't'<<16 | 'm'<<8 | '#' // kAudioDevicePropertyStreams

	dirOutput = 1 // driver.Info.Default and Default's argument: tymbal.Output
	dirInput  = 2 // tymbal.Input
)

// backend is the Core Audio driver. It holds no state: every call asks Core
// Audio, and each stream owns what it creates.
type backend struct{}

// New returns the Core Audio driver.
func New() driver.Driver { return &backend{} }

func (*backend) Name() string { return "coreaudio" }

// hal runs once, before the first call that reaches Core Audio.
//
// By default the HAL delivers its system notifications on the process's main
// run loop. A Go program never runs one, so a long-lived process would see a
// device list frozen at its first query. Setting kAudioHardwarePropertyRunLoop
// to NULL makes the HAL manage its own notification thread, which is what
// every Core Audio client without a run loop does (Apple TN2223; miniaudio and
// PortAudio do the same).
var hal sync.Once

func initHAL() {
	hal.Do(func() {
		var runLoop uintptr // NULL
		_ = audioObjectSetPropertyData(objectSystem, selRunLoop, scopeGlobal,
			uint32(unsafe.Sizeof(runLoop)), unsafe.Pointer(&runLoop))
	})
}

// Devices lists every audio device that has at least one input or output
// channel. A device's ID is its Core Audio UID, which survives reboots and
// re-plugging (an AudioObjectID does not).
func (*backend) Devices() ([]driver.Info, error) {
	initHAL()
	ids, err := deviceIDs()
	if err != nil {
		return nil, err
	}
	defOut := defaultDeviceID(selDefaultOutputDevice)
	defIn := defaultDeviceID(selDefaultInputDevice)
	infos := make([]driver.Info, 0, len(ids))
	for _, id := range ids {
		info, ok := describeDevice(id)
		if !ok {
			continue // gone between the two calls, or has no audio streams
		}
		if id == defOut {
			info.Default |= dirOutput
		}
		if id == defIn {
			info.Default |= dirInput
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// Default returns the system default input (2) or output (1) device.
func (*backend) Default(dir uint8) (driver.Info, error) {
	var selector uint32
	switch dir {
	case dirOutput:
		selector = selDefaultOutputDevice
	case dirInput:
		selector = selDefaultInputDevice
	default:
		return driver.Info{}, fmt.Errorf("coreaudio: invalid direction %d", dir)
	}
	initHAL()
	id := defaultDeviceID(uint32(selector))
	if id == 0 {
		return driver.Info{}, fmt.Errorf("%w: no default device for direction %d", errNoDevice, dir)
	}
	info, ok := describeDevice(id)
	if !ok {
		return driver.Info{}, fmt.Errorf("%w: default device %d cannot be described", errNoDevice, id)
	}
	info.Default |= dir
	return info, nil
}

// Watch reports no device changes yet.
//
// TODO(M8): AudioObjectAddPropertyListener calls back on a HAL thread, which
// needs the same kind of assembly trampoline as the render callback (store a
// flag, signal a semaphore) with a Go goroutine on the far side. Until then
// callers poll Devices.
func (*backend) Watch(fn func(driver.Event)) (stop func()) {
	return func() {}
}

// Open creates an AUHAL output stream on req.Output at the device's current
// sample rate.
//
// It asks the HAL for an IO buffer of the requested period (clamped to the
// device's range), because a small period is only worth having if the device's
// cycle is small too. That request belongs to this process: another process
// still reads the old size, and it ends with the process. Open never changes
// the device's sample rate; a request for another rate fails with
// driver.ErrFormat.
func (*backend) Open(req driver.Request) (driver.Stream, error) {
	switch {
	case req.Input != nil:
		return nil, fmt.Errorf("%w: coreaudio input and duplex streams are not implemented", driver.ErrUnsupported)
	case req.Output == nil:
		return nil, fmt.Errorf("%w: coreaudio needs an output device", driver.ErrUnsupported)
	case req.Exclusive:
		return nil, fmt.Errorf("%w: coreaudio exclusive (hog) mode is not implemented", driver.ErrUnsupported)
	case req.OutChannels <= 0 || req.SampleRate <= 0 || req.Period <= 0 || req.Periods <= 0 || req.InChannels != 0:
		return nil, driver.ErrFormat
	case req.Period > maxRingFrames || req.Periods > maxRingFrames:
		return nil, errRingGeometry // also keeps Period*Periods from overflowing
	}
	initHAL()
	id, err := deviceByUID(req.Output.ID)
	if err != nil {
		return nil, err
	}

	nominal, err := f64Prop(id, selNominalSampleRate, scopeGlobal)
	if err != nil {
		return nil, classify(err)
	}
	rate := int(math.Round(nominal))
	if rate != req.SampleRate {
		return nil, fmt.Errorf("%w: %q runs at %d Hz and tymbal does not change a device's sample rate; requested %d Hz",
			driver.ErrFormat, req.Output.Name, rate, req.SampleRate)
	}
	if outs := channelCount(id, scopeOutput); req.OutChannels > outs {
		return nil, fmt.Errorf("%w: %q has %d output channels, requested %d", driver.ErrFormat, req.Output.Name, outs, req.OutChannels)
	}

	// Period. The Go render period is free to differ from the device's IO
	// buffer, since the ring decouples them, but the point of asking for a
	// small one is a small device buffer too.
	period := req.Period
	if lo, hi := bufferRange(id); hi > 0 {
		period = max(lo, min(period, hi))
	}
	deviceFrames := period
	want := uint32(period)
	if cur, err := u32Prop(id, selBufferFrameSize, scopeGlobal); err != nil || cur != want {
		_ = audioObjectSetPropertyData(id, selBufferFrameSize, scopeGlobal, 4, unsafe.Pointer(&want))
		if got, err := u32Prop(id, selBufferFrameSize, scopeGlobal); err == nil && got > 0 {
			deviceFrames = int(got)
		}
	} else {
		deviceFrames = int(cur)
	}

	// Frames between the ring and the speaker: the IO buffer being played, the
	// device and stream latency, and the safety offset the HAL keeps ahead.
	pipe := deviceFrames
	for _, p := range []uint32{selDeviceLatency, selSafetyOffset} {
		if v, err := u32Prop(id, p, scopeOutput); err == nil {
			pipe += int(v)
		}
	}
	if streams, err := u32sProp(id, selDeviceStreams, scopeOutput); err == nil && len(streams) > 0 {
		if v, err := u32Prop(streams[0], selDeviceLatency, scopeGlobal); err == nil {
			pipe += int(v)
		}
	}

	// Frames kept queued ahead of Core Audio. At least the requested number of
	// periods, and always room for one device buffer plus a period of slack.
	fill := max(req.Periods, 2) * period
	if floor := deviceFrames + period; fill < floor {
		fill = (floor + period - 1) / period * period
	}

	s, err := newStream(streamConfig{rate: rate, channels: req.OutChannels, period: period, fill: fill, pipeFrames: pipe})
	if err != nil {
		return nil, err
	}
	if err := s.attachUnit(id); err != nil {
		_ = s.Close()
		return nil, classify(err)
	}
	return s, nil
}

var _ driver.Driver = (*backend)(nil)

// deviceIDs returns the AudioObjectID of every device. A machine whose audio
// server is not running, as in a headless CI sandbox, reports errNoDevice.
func deviceIDs() ([]uint32, error) {
	ids, err := u32sProp(objectSystem, selDevices, scopeGlobal)
	if err != nil {
		switch status, _ := statusOf(err); status {
		case statusNotRunning, statusUnknownProperty, statusBadObject:
			return nil, fmt.Errorf("%w: %v", errNoDevice, err)
		}
		return nil, classify(err)
	}
	return ids, nil
}

// defaultDeviceID returns the default device for selector, or 0 for none.
func defaultDeviceID(selector uint32) uint32 {
	id, err := u32Prop(objectSystem, selector, scopeGlobal)
	if err != nil {
		return 0
	}
	return id
}

// deviceByUID finds the device whose UID is uid.
func deviceByUID(uid string) (uint32, error) {
	ids, err := deviceIDs()
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if got, ok := stringProp(id, selDeviceUID, scopeGlobal); ok && got == uid {
			return id, nil
		}
	}
	return 0, fmt.Errorf("%w: no audio device with ID %q", driver.ErrLost, uid)
}

// describeDevice reads one device's Info. It reports false for a device that
// disappeared or has no streams.
func describeDevice(id uint32) (driver.Info, bool) {
	uid, ok := stringProp(id, selDeviceUID, scopeGlobal)
	if !ok {
		return driver.Info{}, false
	}
	name, ok := stringProp(id, selObjectName, scopeGlobal)
	if !ok || name == "" {
		name = uid
	}
	info := driver.Info{
		ID: uid, Name: name,
		Inputs:  channelCount(id, scopeInput),
		Outputs: channelCount(id, scopeOutput),
	}
	if info.Inputs == 0 && info.Outputs == 0 {
		return driver.Info{}, false
	}
	// Open does not change the sample rate, so the nominal rate is the only
	// rate on offer.
	if rate, err := f64Prop(id, selNominalSampleRate, scopeGlobal); err == nil && rate > 0 {
		info.SampleRates = []int{int(math.Round(rate))}
	}
	info.MinPeriod, info.MaxPeriod = bufferRange(id)
	return info, true
}

// bufferRange returns the device's IO buffer size range in frames, or 0, 0.
func bufferRange(id uint32) (lo, hi int) {
	var r struct{ min, max float64 } // AudioValueRange
	size := uint32(unsafe.Sizeof(r))
	if audioObjectGetPropertyData(id, selBufferFrameSizeRng, scopeGlobal, &size, unsafe.Pointer(&r)) != 0 {
		return 0, 0
	}
	if r.min < 1 || r.max < r.min || r.max > math.MaxInt32 {
		return 0, 0
	}
	return int(r.min), int(r.max)
}

// channelCount sums mNumberChannels over the device's stream configuration in
// scope. The property is a variable-length AudioBufferList.
func channelCount(id, scope uint32) int {
	var size uint32
	if audioObjectGetPropertyDataSize(id, selStreamConfiguration, scope, &size) != 0 || size < 8 || size > 1<<16 {
		return 0
	}
	buf := make([]uint64, (size+7)/8) // 8-byte aligned, like AudioBufferList
	got := size
	if audioObjectGetPropertyData(id, selStreamConfiguration, scope, &got, unsafe.Pointer(&buf[0])) != 0 {
		return 0
	}
	base := unsafe.Pointer(&buf[0])
	buffers := int(*(*uint32)(base)) // mNumberBuffers
	channels := 0
	for i := 0; i < buffers; i++ {
		off := 8 + 16*i // sizeof(AudioBuffer) is 16 and the array starts at offset 8
		if off+16 > int(got) {
			break
		}
		channels += int(*(*uint32)(unsafe.Add(base, off))) // mNumberChannels
	}
	return channels
}

// u32Prop reads a UInt32 property.
func u32Prop(obj, selector, scope uint32) (uint32, error) {
	var v uint32
	size := uint32(4)
	if err := check("AudioObjectGetPropertyData", audioObjectGetPropertyData(obj, selector, scope, &size, unsafe.Pointer(&v))); err != nil {
		return 0, err
	}
	return v, nil
}

// f64Prop reads a Float64 property.
func f64Prop(obj, selector, scope uint32) (float64, error) {
	var v float64
	size := uint32(8)
	if err := check("AudioObjectGetPropertyData", audioObjectGetPropertyData(obj, selector, scope, &size, unsafe.Pointer(&v))); err != nil {
		return 0, err
	}
	return v, nil
}

// u32sProp reads a property that is an array of UInt32, such as a list of
// AudioObjectIDs.
func u32sProp(obj, selector, scope uint32) ([]uint32, error) {
	var size uint32
	if err := check("AudioObjectGetPropertyDataSize", audioObjectGetPropertyDataSize(obj, selector, scope, &size)); err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	if size > 1<<20 {
		return nil, fmt.Errorf("coreaudio: property %#x is %d bytes", selector, size)
	}
	vals := make([]uint32, size/4)
	got := size
	if err := check("AudioObjectGetPropertyData", audioObjectGetPropertyData(obj, selector, scope, &got, unsafe.Pointer(&vals[0]))); err != nil {
		return nil, err
	}
	return vals[:got/4], nil
}

// stringProp reads a CFString property, which Core Audio hands over retained.
func stringProp(obj, selector, scope uint32) (string, bool) {
	var ref uintptr
	size := uint32(unsafe.Sizeof(ref))
	if audioObjectGetPropertyData(obj, selector, scope, &size, unsafe.Pointer(&ref)) != 0 || ref == 0 {
		return "", false
	}
	defer cfRelease(ref)
	return cfStringToString(ref)
}
