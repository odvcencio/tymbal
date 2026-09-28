//go:build windows

package wasapi

import (
	"errors"
	"fmt"

	"m31labs.dev/tymbal/internal/driver"
)

// duplexStream combines the shared-mode capture and render clients on one
// locked stream thread. Both clients use the same callback period.
type duplexStream struct {
	capture *captureStream
	render  *wasapiStream
	params  driver.Params
	started bool
	stopped bool
}

var _ driver.Stream = (*duplexStream)(nil)

func openDuplexStream(req driver.Request) (driver.Stream, error) {
	if req.Input == nil || req.Output == nil {
		return nil, driver.ErrUnsupported
	}
	if req.Exclusive {
		return nil, fmt.Errorf("%w: WASAPI duplex currently requires shared mode", driver.ErrUnsupported)
	}
	captureReq := req
	captureReq.Output = nil
	captureReq.OutChannels = 0
	renderReq := req
	renderReq.Input = nil
	renderReq.InChannels = 0
	captureDriver, err := openCaptureStream(captureReq)
	if err != nil {
		return nil, err
	}
	captureStream, ok := captureDriver.(*captureStream)
	if !ok {
		_ = captureDriver.Close()
		return nil, fmt.Errorf("tymbal wasapi: capture opener returned an unexpected stream type")
	}
	renderDriver, err := openRenderStream(renderReq)
	if err != nil {
		_ = captureStream.Close()
		return nil, err
	}
	renderStream, ok := renderDriver.(*wasapiStream)
	if !ok {
		_ = captureStream.Close()
		_ = renderDriver.Close()
		return nil, fmt.Errorf("tymbal wasapi: render opener returned an unexpected stream type")
	}
	captureParams := captureStream.Params()
	renderParams := renderStream.Params()
	if err := validateDuplexGeometry(captureParams, renderParams); err != nil {
		_ = captureStream.Close()
		_ = renderStream.Close()
		return nil, err
	}
	params := captureParams
	params.OutChannels = renderParams.OutChannels
	params.OutFormat = renderParams.OutFormat
	params.LatencyOut = renderParams.LatencyOut
	params.Priority = renderParams.Priority
	params.HasDeadline = renderParams.HasDeadline
	return &duplexStream{capture: captureStream, render: renderStream, params: params}, nil
}

func validateDuplexGeometry(capture, render driver.Params) error {
	if capture.SampleRate != render.SampleRate || capture.Period != render.Period || capture.Periods != render.Periods {
		return fmt.Errorf("%w: WASAPI duplex endpoints negotiated different rate or buffer geometry", driver.ErrFormat)
	}
	return nil
}

func (s *duplexStream) Params() driver.Params { return s.params }

func (s *duplexStream) Start() error {
	if s.started || s.stopped {
		return fmt.Errorf("tymbal wasapi: duplex stream already started or stopped")
	}
	if err := s.capture.Start(); err != nil {
		s.stopped = true
		return err
	}
	if err := s.render.Start(); err != nil {
		cleanupErr := s.capture.Stop()
		s.stopped = true
		return errors.Join(err, cleanupErr)
	}
	s.started = true
	return nil
}

func (s *duplexStream) Wait() error {
	if !s.started || s.stopped {
		return fmt.Errorf("tymbal wasapi: wait outside a running duplex stream")
	}
	if err := s.capture.Wait(); err != nil {
		return err
	}
	return s.render.Wait()
}

func (s *duplexStream) Interrupt() {
	s.capture.Interrupt()
	s.render.Interrupt()
}

func (s *duplexStream) Buffers() (in, out []byte) {
	in, _ = s.capture.Buffers()
	_, out = s.render.Buffers()
	return in, out
}

func (s *duplexStream) Commit() error {
	if err := s.capture.Commit(); err != nil {
		return err
	}
	return s.render.Commit()
}

func (s *duplexStream) Clock() (outNano, inNano int64) {
	outNano, _ = s.render.Clock()
	_, inNano = s.capture.Clock()
	return outNano, inNano
}

func (s *duplexStream) ClockSample() driver.ClockSample {
	output := s.render.ClockSample()
	input := s.capture.ClockSample()
	output.InputPosition = input.InputPosition
	output.InputFrequency = input.InputFrequency
	output.InputQPCNano = input.InputQPCNano
	return output
}

func (s *duplexStream) Deadlines() (wakeNano, commitNano int64) {
	return s.render.Deadlines()
}

func (s *duplexStream) Dropouts() uint64 {
	return s.capture.Dropouts() + s.render.Dropouts()
}

func (s *duplexStream) Recover() error {
	return s.capture.Recover()
}

func (s *duplexStream) Stop() error {
	if s.stopped {
		return nil
	}
	s.stopped = true
	return errors.Join(s.capture.Stop(), s.render.Stop())
}

func (s *duplexStream) Close() error {
	return errors.Join(s.capture.Close(), s.render.Close())
}
