//go:build darwin && (amd64 || arm64)

package coreaudio

import (
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
	"m31labs.dev/tymbal/internal/format"
)

// duplex joins two independent HAL devices. Output paces rendering; capture's
// bounded ring and band-limited rate bridge track the input clock separately.
type duplex struct {
	output     *stream
	input      *captureStream
	reader     *inputReader
	params     driver.Params
	in         []byte
	clock      driver.ClockSample
	outReady   bool
	lastDrops  uint64
	queueDrops uint64
	queueReset bool
	maxQueue   float64
	ready      bool
}

func (b *backend) openInput(req driver.Request) (driver.Stream, error) {
	if req.Exclusive {
		return nil, fmt.Errorf("%w: CoreAudio exclusive input is unavailable", driver.ErrUnsupported)
	}
	if req.Input == nil || req.InChannels < 1 || req.InChannels > maxChannels || req.SampleRate < 8000 || req.SampleRate > 384000 || req.Period < 1 || req.Period > 8192 || req.Periods < 1 || req.Periods > 32 {
		return nil, driver.ErrFormat
	}
	if req.Output != nil && (req.OutChannels < 1 || req.OutChannels > maxChannels) {
		return nil, driver.ErrFormat
	}
	if req.Output == nil && req.OutChannels != 0 {
		return nil, driver.ErrFormat
	}
	initHAL()
	id, err := deviceByUID(req.Input.ID)
	if err != nil {
		return nil, err
	}
	if count := channelCount(id, scopeInput); req.InChannels > count {
		return nil, fmt.Errorf("%w: %q has %d input channels, requested %d", driver.ErrFormat, req.Input.Name, count, req.InChannels)
	}
	nominal, err := f64Prop(id, selNominalSampleRate, scopeGlobal)
	if err != nil {
		return nil, classify(err)
	}
	rate := int(math.Round(nominal))
	ratio := float64(rate) / float64(req.SampleRate)
	if rate < 8000 || rate > 384000 || ratio < 0.125 || ratio > 8 {
		return nil, fmt.Errorf("%w: input/project rate ratio outside 1:8..8:1", driver.ErrFormat)
	}
	if req.Output != nil {
		outputID, e := deviceByUID(req.Output.ID)
		if e != nil {
			return nil, e
		}
		outputRate, e := f64Prop(outputID, selNominalSampleRate, scopeGlobal)
		if e != nil {
			return nil, classify(e)
		}
		if int(math.Round(outputRate)) != req.SampleRate || req.OutChannels > channelCount(outputID, scopeOutput) {
			return nil, fmt.Errorf("%w: output device does not match requested rate/channels", driver.ErrFormat)
		}
	}
	// All request validation above is side-effect free. This is the only place
	// opening the library may request microphone consent, never enumeration.
	if err := ensureInputPermission(); err != nil {
		return nil, err
	}
	d := &duplex{}
	if req.Output != nil {
		outReq := req
		outReq.Input = nil
		outReq.InChannels = 0
		out, err := b.openOutput(outReq)
		if err != nil {
			return nil, err
		}
		d.output = out.(*stream)
		d.params = d.output.Params()
	} else {
		d.params = driver.Params{SampleRate: req.SampleRate, Period: req.Period, Periods: req.Periods}
	}
	period := max(1, int(math.Ceil(float64(d.params.Period)*ratio)))
	if lo, hi := bufferRange(id); hi > 0 {
		period = max(lo, min(period, hi))
	}
	value := uint32(period)
	_ = audioObjectSetPropertyData(id, selBufferFrameSize, scopeGlobal, 4, unsafe.Pointer(&value))
	deviceFrames := period
	if actual, e := u32Prop(id, selBufferFrameSize, scopeGlobal); e == nil && actual > 0 {
		deviceFrames = int(actual)
	}
	pipe := deviceFrames
	for _, selector := range []uint32{selDeviceLatency, selSafetyOffset} {
		if value, e := u32Prop(id, selector, scopeInput); e == nil {
			pipe += int(value)
		}
	}
	if streams, e := u32sProp(id, selDeviceStreams, scopeInput); e == nil && len(streams) > 0 {
		if value, e := u32Prop(streams[0], selDeviceLatency, scopeGlobal); e == nil {
			pipe += int(value)
		}
	}
	d.input, err = newCaptureStream(rate, req.InChannels, deviceFrames, pipe)
	if err != nil {
		if d.output != nil {
			_ = d.output.Close()
		}
		return nil, err
	}
	if err = d.input.attach(id); err != nil {
		_ = d.Close()
		return nil, classify(err)
	}
	target := max(2*deviceFrames, 3*int(math.Ceil(float64(d.params.Period)*ratio))) + inputTaps/2
	d.reader = newInputReader(d.input.data, req.InChannels, d.params.Period, rate, d.params.SampleRate, target)
	d.maxQueue = float64(target + max(4*deviceFrames, 4*int(math.Ceil(float64(d.params.Period)*ratio))))
	d.in = make([]byte, d.params.Period*req.InChannels*4)
	d.params.InChannels = req.InChannels
	d.params.InFormat = format.F32LE
	d.params.LatencyIn = time.Duration(float64(pipe+target) * float64(time.Second) / float64(rate))
	return d, nil
}

func (d *duplex) Params() driver.Params { return d.params }
func (d *duplex) Start() error {
	if err := d.input.start(); err != nil {
		return err
	}
	// Prime capture before output starts, avoiding synthetic zero input at the
	// first callback and absorbing independent HAL callback packet sizes.
	if err := d.input.wait(uint64(d.reader.target)); err != nil {
		_ = d.input.stop()
		return err
	}
	head := atomic.LoadUint64(&d.input.c.head)
	if head > uint64(d.reader.target) {
		d.reader.phase = float64(head) - d.reader.target
	}
	if d.output != nil {
		if err := d.output.Start(); err != nil {
			_ = d.input.stop()
			return err
		}
	}
	return nil
}
func (d *duplex) Wait() error {
	if d.ready {
		return errWaitCommit
	}
	if d.output != nil && !d.outReady {
		if err := d.output.Wait(); err != nil {
			return err
		}
		d.outReady = true
	}
	if drops := atomic.LoadUint64(&d.input.c.under); drops != d.lastDrops {
		return driver.ErrXrun
	}
	if err := d.input.wait(d.reader.required()); err != nil {
		return err
	}
	phase := d.reader.phase
	head := atomic.LoadUint64(&d.input.c.head)
	if d.reader.started && float64(head)-d.reader.phase > d.maxQueue {
		d.queueDrops++
		d.queueReset = true
		return driver.ErrXrun
	}
	if !d.reader.started && head > uint64(d.reader.target) {
		phase = float64(head) - d.reader.target
		d.reader.phase = phase
	}
	d.clock = d.input.clock(phase)
	tail := d.reader.read(d.in, head)
	atomic.StoreUint64(&d.input.c.tail, tail)
	d.ready = true
	return nil
}
func (d *duplex) Buffers() ([]byte, []byte) {
	if d.output != nil {
		return d.in, d.output.out
	}
	return d.in, nil
}
func (d *duplex) Commit() error {
	if !d.ready {
		return errState
	}
	if d.output != nil {
		if err := d.output.Commit(); err != nil {
			return err
		}
		d.outReady = false
	}
	d.ready = false
	return nil
}
func (d *duplex) Clock() (int64, int64) {
	var out int64
	if d.output != nil {
		out, _ = d.output.Clock()
	}
	return out, d.clock.InputQPCNano
}
func (d *duplex) ClockSample() driver.ClockSample { return d.clock }
func (d *duplex) Deadlines() (int64, int64)       { return 0, 0 }
func (d *duplex) Dropouts() uint64 {
	var n uint64
	if d.output != nil {
		n = d.output.Dropouts()
	}
	if d.input.closed.Load() {
		return n + d.input.finalDrops + d.queueDrops
	}
	return n + atomic.LoadUint64(&d.input.c.under) + d.queueDrops
}
func (d *duplex) Recover() error {
	d.lastDrops = atomic.LoadUint64(&d.input.c.under)
	head := atomic.LoadUint64(&d.input.c.head)
	if d.queueReset && head > uint64(d.reader.target) {
		head -= uint64(d.reader.target)
	}
	d.queueReset = false
	d.reader.reset(head)
	atomic.StoreUint64(&d.input.c.tail, head)
	return nil
}
func (d *duplex) Interrupt() {
	if d.output != nil {
		d.output.Interrupt()
	}
	d.input.interrupt()
}
func (d *duplex) Stop() error {
	var first error
	if d.output != nil {
		first = d.output.Stop()
	}
	return errors.Join(first, d.input.stop())
}
func (d *duplex) Close() error {
	var first error
	if d.output != nil {
		first = d.output.Close()
	}
	return errors.Join(first, d.input.close())
}
