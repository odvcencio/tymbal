//go:build windows

package wasapi

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"m31labs.dev/tymbal/internal/driver"
	audioformat "m31labs.dev/tymbal/internal/format"
)

func TestExclusiveRenderRequestRequiresOutputOnlyAndTwoBuffers(t *testing.T) {
	output := &driver.Info{ID: "render"}
	valid := driver.Request{
		Output: output, OutChannels: 2, SampleRate: 48_000, Period: 128,
		Periods: exclusiveRenderPingPongBuffers, Exclusive: true,
	}
	if err := validateExclusiveRenderRequest(valid); err != nil {
		t.Fatalf("valid exclusive render request rejected: %v", err)
	}

	for _, periods := range []int{0, 1, 3} {
		request := valid
		request.Periods = periods
		if err := validateExclusiveRenderRequest(request); !errors.Is(err, driver.ErrFormat) {
			t.Errorf("exclusive render Periods=%d error = %v, want driver.ErrFormat", periods, err)
		}
	}

	duplex := valid
	duplex.Input = &driver.Info{ID: "capture"}
	if err := validateExclusiveRenderRequest(duplex); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("exclusive duplex request error = %v, want driver.ErrUnsupported", err)
	}
	inputOnly := valid
	inputOnly.Output = nil
	if err := validateExclusiveRenderRequest(inputOnly); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("exclusive input-only request error = %v, want driver.ErrUnsupported", err)
	}
}

func TestExclusiveRenderGeometryUsesFullBufferAndReportsPingPongCapacity(t *testing.T) {
	period, periods, capacity, err := exclusiveRenderBufferGeometry(480)
	if err != nil || period != 480 || periods != 2 || capacity != 960 {
		t.Fatalf("exclusiveRenderBufferGeometry = (%d, %d, %d, %v), want (480, 2, 960, nil)", period, periods, capacity, err)
	}

	period, periods, capacity, err = exclusiveRenderBufferGeometry(128)
	if err != nil || period != 128 || periods != 2 || capacity != 256 {
		t.Fatalf("128-frame geometry = (%d, %d, %d, %v), want complete 128-frame callbacks and 256 frames total", period, periods, capacity, err)
	}
	if _, _, _, err := exclusiveRenderBufferGeometry(0); !errors.Is(err, driver.ErrFormat) {
		t.Errorf("zero-frame geometry error = %v, want driver.ErrFormat", err)
	}
}

func TestExclusiveRenderDurationUsesOneRequestedBuffer(t *testing.T) {
	duration, err := requestedExclusiveBufferDuration(128, 1, 48_000, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	want, ok := framesToHNS(128, 48_000)
	if !ok || duration != want {
		t.Fatalf("requested exclusive render duration = %d hns, want one 128-frame buffer (%d hns)", duration, want)
	}
}

func TestExclusiveRenderClassifiesHRESULT(t *testing.T) {
	cases := []struct {
		code uint32
		want error
	}{
		{audclntErrDeviceInUse, driver.ErrBusy},
		{audclntErrExclusiveModeNotAllowed, driver.ErrUnsupported},
		{audclntErrUnsupportedFormat, driver.ErrFormat},
		{audclntErrBufferSizeNotAligned, driver.ErrFormat},
		{audclntErrInvalidDevicePeriod, driver.ErrFormat},
	}
	for _, tc := range cases {
		err := classifyExclusiveRenderError("test", &hresultError{code: tc.code})
		if !errors.Is(err, tc.want) {
			t.Errorf("HRESULT 0x%08x classified as %v, want %v", tc.code, err, tc.want)
		}
		if strings.Contains(err.Error(), "capture") {
			t.Errorf("render error uses capture wording: %v", err)
		}
	}
}

func TestExclusiveRenderRejectsDuplexBeforeCOM(t *testing.T) {
	_, err := openRenderStream(driver.Request{
		Output: &driver.Info{ID: "render"}, Input: &driver.Info{ID: "capture"},
		OutChannels: 2, InChannels: 2, SampleRate: 48_000, Period: 480,
		Periods: 2, Exclusive: true,
	})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("exclusive duplex open error = %v, want driver.ErrUnsupported", err)
	}
}

func TestNativeExclusiveRenderLifecycle(t *testing.T) {
	if os.Getenv("TYMBAL_REQUIRE_EXCLUSIVE_RENDER") != "1" {
		t.Skip("set TYMBAL_REQUIRE_EXCLUSIVE_RENDER=1 to require native exclusive render")
	}

	devices, err := New().Devices()
	if err != nil {
		t.Fatalf("enumerate WASAPI endpoints for required exclusive render test: %v", err)
	}
	var output *driver.Info
	for i := range devices {
		if devices[i].Outputs == 0 || len(devices[i].SampleRates) == 0 {
			continue
		}
		if output == nil || devices[i].Default == 1 {
			output = &devices[i]
			if devices[i].Default == 1 {
				break
			}
		}
	}
	if output == nil {
		t.Fatal("TYMBAL_REQUIRE_EXCLUSIVE_RENDER=1 but Windows exposes no active render endpoint")
	}

	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		stream, openErr := New().Open(driver.Request{
			Output: output, OutChannels: output.Outputs,
			SampleRate: output.SampleRates[0], Period: 480, Periods: exclusiveRenderPingPongBuffers,
			Exclusive: true,
		})
		if openErr != nil {
			done <- fmt.Errorf("open required exclusive render: %w", openErr)
			return
		}
		if startErr := stream.Start(); startErr != nil {
			_ = stream.Stop()
			_ = stream.Close()
			done <- fmt.Errorf("start required exclusive render: %w", startErr)
			return
		}
		params := stream.Params()
		_, out := stream.Buffers()
		wantBytes := params.Period * params.OutChannels * audioformat.BytesPerSample(params.OutFormat)
		if params.Period <= 0 || params.Periods != exclusiveRenderPingPongBuffers || len(out) != wantBytes {
			_ = stream.Stop()
			_ = stream.Close()
			done <- fmt.Errorf("exclusive render params/buffer = period %d, periods %d, bytes %d; want a complete two-buffer grant and %d bytes", params.Period, params.Periods, len(out), wantBytes)
			return
		}
		servicePeriod := func() error {
			if waitErr := stream.Wait(); waitErr != nil {
				return fmt.Errorf("wait for exclusive render event: %w", waitErr)
			}
			_, buffer := stream.Buffers()
			clear(buffer) // Output silence only; no microphone or system audio is read.
			if commitErr := stream.Commit(); commitErr != nil {
				return fmt.Errorf("commit full exclusive render buffer: %w", commitErr)
			}
			return nil
		}
		var periodErr error
		for i := 0; i < 3; i++ {
			if periodErr = servicePeriod(); periodErr != nil {
				break
			}
		}
		var allocErr error
		allocs := testing.AllocsPerRun(5, func() {
			if allocErr == nil {
				allocErr = servicePeriod()
			}
		})
		stopErr := stream.Stop()
		closeErr := stream.Close()
		if periodErr != nil {
			done <- periodErr
			return
		}
		if allocErr != nil {
			done <- allocErr
			return
		}
		if allocs != 0 {
			done <- fmt.Errorf("exclusive render Wait/Buffers/Commit allocated %.2f times per period", allocs)
			return
		}
		done <- errors.Join(stopErr, closeErr)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("required exclusive render lifecycle did not finish within 15 seconds")
	}
}
