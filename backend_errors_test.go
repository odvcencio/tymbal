package tymbal

import (
	"errors"
	"testing"
	"time"

	"m31labs.dev/tymbal/internal/driver"
)

type failingDriver struct {
	driver.Driver
	startErr, commitErr, closeErr error
}

func (d failingDriver) Open(req driver.Request) (driver.Stream, error) {
	s, err := d.Driver.Open(req)
	if err != nil {
		return nil, err
	}
	return &failingStream{Stream: s, startErr: d.startErr, commitErr: d.commitErr, closeErr: d.closeErr}, nil
}

type failingStream struct {
	driver.Stream
	startErr, commitErr, closeErr error
}

func (s *failingStream) Start() error {
	if s.startErr != nil {
		return s.startErr
	}
	return s.Stream.Start()
}

func (s *failingStream) Commit() error {
	if s.commitErr != nil {
		return s.commitErr
	}
	return s.Stream.Commit()
}

func (s *failingStream) Close() error {
	if err := s.Stream.Close(); err != nil {
		return err
	}
	return s.closeErr
}

func TestBackendFailuresUsePublicErrors(t *testing.T) {
	for _, test := range []struct {
		name                          string
		startErr, commitErr, closeErr error
		want                          error
	}{
		{name: "changed format at start", startErr: driver.ErrFormat, want: ErrFormat},
		{name: "device lost during commit", commitErr: driver.ErrLost, want: ErrDeviceLost},
		{name: "device lost during close", closeErr: driver.ErrLost, want: ErrDeviceLost},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, _ := NewFakeHost(FakeConfig{})
			h.d = failingDriver{Driver: h.d, startErr: test.startErr, commitErr: test.commitErr, closeErr: test.closeErr}
			device, err := h.Default(Output)
			if err != nil {
				t.Fatal(err)
			}
			s, err := Open(h, Config{Output: &device, OutChannels: 1, SampleRate: 48000, Period: 4}, func(Time, [][]float32, [][]float32) {})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			err = s.Start()
			if test.startErr != nil {
				if !errors.Is(err, test.want) || !errors.Is(s.Err(), test.want) {
					t.Fatalf("Start/Err = %v/%v, want %v", err, s.Err(), test.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.commitErr != nil {
				select {
				case <-s.done:
				case <-time.After(time.Second):
					t.Fatal("commit failure did not stop stream")
				}
				if !errors.Is(s.Err(), test.want) {
					t.Fatalf("Err = %v, want %v", s.Err(), test.want)
				}
			} else if err := s.Close(); !errors.Is(err, test.want) {
				t.Fatalf("Close = %v, want %v", err, test.want)
			}
		})
	}
}
