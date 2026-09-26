// Package tymbal provides cgo-free native audio streams.
//
// The M0 implementation includes the portable stream core and a deterministic
// fake host. Platform audio hosts are added by later milestones.
package tymbal

import (
	"errors"
	"sync"
	"time"

	"m31labs.dev/tymbal/internal/driver"
	"m31labs.dev/tymbal/internal/hist"
)

var (
	ErrDeviceLost  = errors.New("tymbal: device lost")
	ErrBusy        = errors.New("tymbal: device busy")
	ErrFormat      = errors.New("tymbal: no supported format")
	ErrUnsupported = errors.New("tymbal: not supported by this host")
	ErrCallback    = errors.New("tymbal: callback panicked")
	ErrState       = errors.New("tymbal: invalid stream state")
)

// Direction identifies stream directions and device defaults.
type Direction uint8

const (
	Output Direction = 1 << iota
	Input
)

// Host is a value backed by an internal audio driver. Its zero value is
// invalid. Use Hosts or NewFakeHost to obtain a Host.
type Host struct{ d driver.Driver }

// Name returns the backend name, or an empty string for an invalid Host.
func (h Host) Name() string {
	if h.d == nil {
		return ""
	}
	return h.d.Name()
}

// Devices returns a snapshot of the host's currently available devices.
func (h Host) Devices() ([]Device, error) {
	if h.d == nil {
		return nil, ErrState
	}
	devices, err := h.d.Devices()
	if err != nil {
		return nil, mapBackendError(err)
	}
	out := make([]Device, len(devices))
	for i := range devices {
		out[i] = publicDevice(h.d.Name(), devices[i])
	}
	return out, nil
}

// Default returns the host's current default device for dir.
func (h Host) Default(dir Direction) (Device, error) {
	if h.d == nil || (dir != Input && dir != Output) {
		return Device{}, ErrState
	}
	d, err := h.d.Default(uint8(dir))
	if err != nil {
		return Device{}, mapBackendError(err)
	}
	return publicDevice(h.d.Name(), d), nil
}

// Watch subscribes to host device changes. The returned function stops the
// subscription and may be called more than once.
func (h Host) Watch(fn func(DeviceEvent)) (stop func()) {
	if h.d == nil || fn == nil {
		return func() {}
	}
	stop = h.d.Watch(func(e driver.Event) {
		fn(DeviceEvent{Kind: EventKind(e.Kind), Device: publicDevice(h.d.Name(), e.Device)})
	})
	if stop == nil {
		return func() {}
	}
	var once sync.Once
	return func() { once.Do(stop) }
}

// Device describes the channels and nominal formats available on a host.
type Device struct {
	ID, Name, Host       string
	Inputs, Outputs      int
	SampleRates          []int
	MinPeriod, MaxPeriod int
	Default              Direction
	Exclusive            bool
}

// EventKind identifies a device change.
type EventKind uint8

const (
	DeviceAdded EventKind = iota + 1
	DeviceRemoved
	DefaultChanged
	FormatChanged
)

// DeviceEvent is delivered by Host.Watch.
type DeviceEvent struct {
	Kind   EventKind
	Device Device
}

// Config requests a stream. A nil device disables that direction.
type Config struct {
	Output, Input           *Device
	OutChannels, InChannels int
	SampleRate              int
	Period                  int
	Periods                 int
	Exclusive               bool
	NormalPriority          bool
}

// Callback renders one period on the stream's audio thread. Input and output
// are non-interleaved, with one Period-sized slice per channel. The slices are
// valid only for the duration of the call.
type Callback func(t Time, in, out [][]float32)

// Time describes the first frame of the callback period.
type Time struct {
	Frame         uint64
	OutputNano    int64
	InputNano     int64
	Dropouts      uint32
	Discontinuity bool
}

// Actual reports the stream parameters granted by the host.
type Actual struct {
	SampleRate, Period, Periods int
	OutChannels, InChannels     int
	OutFormat, InFormat         string
	LatencyOut, LatencyIn       time.Duration
	Priority                    string
	HasDeadline                 bool
}

// Histogram is the public timing histogram type used by Stats.
type Histogram = hist.Histogram

// Stats is a lock-free snapshot of stream counters and timing distributions.
// A live snapshot can contain values from adjacent callbacks; after Stop it is
// stable.
// WakeInterval observes elapsed time between serviced wakes, including callback,
// backend, recovery, and scheduling time, not device deadline lateness.
// WakeIntervalMax is the exact maximum observed interval, without histogram rounding.
type Stats struct {
	Callbacks       uint64
	Dropouts        uint64
	Late            uint64
	WakeLate        Histogram
	WakeInterval    Histogram
	CallbackTime    Histogram
	WakeLateMax     time.Duration
	WakeIntervalMax time.Duration
	CallbackMax     time.Duration
	AllocsSinceRun  uint64
}

func publicDevice(host string, d driver.Info) Device {
	rates := append([]int(nil), d.SampleRates...)
	return Device{
		ID: d.ID, Name: d.Name, Host: host,
		Inputs: d.Inputs, Outputs: d.Outputs,
		SampleRates: rates, MinPeriod: d.MinPeriod, MaxPeriod: d.MaxPeriod,
		Default: Direction(d.Default), Exclusive: d.Exclusive,
	}
}

func driverInfo(d Device) driver.Info {
	rates := append([]int(nil), d.SampleRates...)
	return driver.Info{
		ID: d.ID, Name: d.Name, Inputs: d.Inputs, Outputs: d.Outputs,
		SampleRates: rates, MinPeriod: d.MinPeriod, MaxPeriod: d.MaxPeriod,
		Default: uint8(d.Default), Exclusive: d.Exclusive,
	}
}
