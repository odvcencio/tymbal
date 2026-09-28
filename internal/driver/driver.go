// Package driver defines the boundary between the portable stream core and
// operating-system audio backends.
package driver

import (
	"errors"
	"time"

	"m31labs.dev/tymbal/internal/format"
)

// ErrXrun reports a recoverable underrun or overrun.
var ErrXrun = errors.New("tymbal driver: xrun")

// ErrLost reports that the device has disappeared.
var ErrLost = errors.New("tymbal driver: device lost")

// ErrInterrupted reports a wait ended because Interrupt was requested.
var ErrInterrupted = errors.New("tymbal driver: interrupted")

// ErrBusy reports that the requested endpoint is already in use.
var ErrBusy = errors.New("tymbal driver: device busy")

// ErrFormat reports a request the device cannot grant without conversion.
var ErrFormat = errors.New("tymbal driver: unsupported format or period")

// ErrUnsupported reports a backend mode that has not been implemented.
var ErrUnsupported = errors.New("tymbal driver: unsupported mode")

// Info describes a device without referring to the public package.
type Info struct {
	ID, Name             string
	Inputs, Outputs      int
	MixFormat            string
	MixBits              int
	SampleRates          []int
	MinPeriod, MaxPeriod int
	DefaultPeriod        int
	FundamentalPeriod    int
	Default              uint8
	Exclusive            bool
}

// ClockSample is the latest device clock observation captured by a stream.
// QPC timestamps use the same monotonic nanosecond domain as rt.Now. A zero
// frequency or timestamp means the backend could not provide that value.
type ClockSample struct {
	OutputPosition, OutputFrequency uint64
	OutputQPCNano                   int64
	InputPosition, InputFrequency   uint64
	InputQPCNano                    int64
}

// ClockSampler is an optional stream extension for backends that can expose
// their device position and host-clock observation without another OS call.
type ClockSampler interface {
	ClockSample() ClockSample
}

// Event is a device-change notification.
type Event struct {
	Kind   uint8
	Device Info
}

// Request is the validated stream request sent to a backend.
type Request struct {
	Output, Input *Info
	OutChannels   int
	InChannels    int
	SampleRate    int
	Period        int
	Periods       int
	Exclusive     bool
}

// Params describes the values negotiated by a backend.
type Params struct {
	SampleRate, Period, Periods int
	OutChannels, InChannels     int
	OutFormat, InFormat         format.Format
	LatencyOut, LatencyIn       time.Duration
	Priority                    string
	HasDeadline                 bool
}

// Driver enumerates devices and opens backend streams.
type Driver interface {
	Name() string
	Devices() ([]Info, error)
	Default(dir uint8) (Info, error)
	Watch(fn func(Event)) (stop func())
	Open(req Request) (Stream, error)
}

// Stream runs one negotiated device stream. Wait, Buffers, Commit, Clock,
// Dropouts, and Recover are called serially on the locked stream thread.
// Interrupt may be called from another goroutine while Wait is blocked.
type Stream interface {
	Params() Params
	Start() error
	Wait() error
	Interrupt()
	Buffers() (in, out []byte)
	Commit() error
	Clock() (outNano, inNano int64)
	Deadlines() (wakeNano, commitNano int64) // same monotonic domain as rt.Now; zero if unavailable
	Dropouts() uint64
	Recover() error
	Stop() error
	Close() error
}
