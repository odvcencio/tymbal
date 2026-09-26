//go:build windows

package wasapi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
	audioformat "m31labs.dev/tymbal/internal/format"
)

func TestMakeExclusiveWaveFormat(t *testing.T) {
	wave, blockAlign, err := makeExclusiveWaveFormat(48_000, 2, audioformat.F32LE)
	if err != nil {
		t.Fatal(err)
	}
	if len(wave) != waveFormatExtensibleByteLength || blockAlign != 8 {
		t.Fatalf("exclusive WAVEFORMATEXTENSIBLE = (%d bytes, blockAlign %d), want (40, 8)", len(wave), blockAlign)
	}
	if binary.LittleEndian.Uint16(wave[0:2]) != waveFormatExtensibleTag ||
		binary.LittleEndian.Uint16(wave[2:4]) != 2 ||
		binary.LittleEndian.Uint32(wave[4:8]) != 48_000 ||
		binary.LittleEndian.Uint32(wave[8:12]) != 384_000 ||
		binary.LittleEndian.Uint16(wave[12:14]) != 8 ||
		binary.LittleEndian.Uint16(wave[14:16]) != 32 ||
		binary.LittleEndian.Uint16(wave[16:18]) != 22 ||
		binary.LittleEndian.Uint16(wave[18:20]) != 32 ||
		binary.LittleEndian.Uint32(wave[20:24]) != 3 ||
		binary.LittleEndian.Uint32(wave[24:28]) != waveSubFormatIEEEFloat.data1 {
		t.Fatalf("unexpected stereo F32 exclusive format fields: %v", wave)
	}
	_, described, channels, rate, err := describeWaveFormat(uintptr(unsafe.Pointer(&wave[0])))
	if err != nil || described != audioformat.F32LE || channels != 2 || rate != 48_000 {
		t.Fatalf("describe generated format = (%s, %d, %d, %v)", described, channels, rate, err)
	}
}

func TestExclusiveFormatSelectionRequiresAnExactGrant(t *testing.T) {
	first := exclusiveWaveFormat{bytes: []byte{1, 2}, format: audioformat.F32LE, blockAlign: 8}
	second := exclusiveWaveFormat{bytes: []byte{3, 4}, format: audioformat.S16LE, blockAlign: 4}
	var probed [][]byte
	selected, err := selectExclusiveFormat([]exclusiveWaveFormat{first, second}, func(candidate []byte) (bool, error) {
		probed = append(probed, candidate)
		return candidate[0] == second.bytes[0], nil
	})
	if err != nil || selected.format != second.format || len(probed) != 2 {
		t.Fatalf("negotiated exact second format = (%+v, %v), probed %d candidates", selected, err, len(probed))
	}
	_, err = selectExclusiveFormat([]exclusiveWaveFormat{first, second}, func([]byte) (bool, error) {
		return false, nil
	})
	if !errors.Is(err, driver.ErrFormat) {
		t.Fatalf("no exact format result error = %v, want driver.ErrFormat", err)
	}

	probed = nil
	selected, err = selectExclusiveFormat([]exclusiveWaveFormat{first, second}, func(candidate []byte) (bool, error) {
		probed = append(probed, candidate)
		return exactExclusiveFormatResult(uintptr(0)), nil
	})
	if err != nil || selected.format != first.format || len(probed) != 1 {
		t.Fatalf("exact S_OK selection = (%+v, %v), probed %d candidates", selected, err, len(probed))
	}
	if exactExclusiveFormatResult(uintptr(1)) || exactExclusiveFormatResult(uintptr(audclntErrUnsupportedFormat)) {
		t.Fatal("S_FALSE and AUDCLNT_E_UNSUPPORTED_FORMAT must not count as exact support")
	}
}

func TestExclusiveBufferDurationUsesRequestAndDeviceMinimum(t *testing.T) {
	duration, err := requestedExclusiveBufferDuration(240, 2, 48_000, 100_000)
	if err != nil || duration != 100_000 {
		t.Fatalf("minimum-period duration = (%d, %v), want 100000 hns", duration, err)
	}
	duration, err = requestedExclusiveBufferDuration(480, 2, 48_000, 100_000)
	if err != nil || duration != 200_000 {
		t.Fatalf("requested duration = (%d, %v), want 200000 hns", duration, err)
	}
	if frames, ok := hnsToFramesCeil(100_001, 48_000); !ok || frames != 481 {
		t.Fatalf("ceil minimum frames = (%d, %v), want (481, true)", frames, ok)
	}
	if frames, ok := hnsToFramesCeil(100_000, 48_000); !ok || frames != 480 {
		t.Fatalf("exact minimum frames = (%d, %v), want (480, true)", frames, ok)
	}
	if hns, ok := framesToHNS(480, 48_000); !ok || hns != 100_000 {
		t.Fatalf("aligned duration = (%d, %v), want (100000, true)", hns, ok)
	}
}

func TestExclusiveInitializeAlignmentReleasesAndReactivatesClient(t *testing.T) {
	client := uintptr(1)
	var calls []string
	ops := exclusiveInitializeOps{
		initialize: func(current uintptr, duration int64) uintptr {
			calls = append(calls, fmt.Sprintf("initialize:%d:%d", current, duration))
			if current == 1 {
				return uintptr(audclntErrBufferSizeNotAligned)
			}
			return 0
		},
		bufferSize: func(current uintptr) (uint32, error) {
			calls = append(calls, fmt.Sprintf("buffer-size:%d", current))
			return 480, nil
		},
		release: func(current uintptr) {
			calls = append(calls, fmt.Sprintf("release:%d", current))
		},
		activate: func() (uintptr, error) {
			calls = append(calls, "activate")
			return 2, nil
		},
	}
	frames, duration, err := initializeExclusiveAligned(&client, 200_000, 48_000, ops)
	if err != nil || frames != 480 || duration != 100_000 || client != 2 {
		t.Fatalf("alignment retry = (%d frames, %d hns, client %d, %v)", frames, duration, client, err)
	}
	want := []string{"initialize:1:200000", "buffer-size:1", "release:1", "activate", "initialize:2:100000", "buffer-size:2"}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("alignment operation order = %v, want %v", calls, want)
	}
}

func TestClassifyExclusiveCaptureErrors(t *testing.T) {
	cases := []struct {
		code uint32
		want error
	}{
		{audclntErrDeviceInUse, driver.ErrBusy},
		{audclntErrExclusiveModeNotAllowed, driver.ErrUnsupported},
		{audclntErrUnsupportedFormat, driver.ErrFormat},
		{audclntErrBufferSizeNotAligned, driver.ErrFormat},
	}
	for _, tc := range cases {
		err := classifyExclusiveError("test", &hresultError{code: tc.code})
		if !errors.Is(err, tc.want) {
			t.Errorf("HRESULT 0x%08x classified as %v, want %v", tc.code, err, tc.want)
		}
	}
}

func TestExclusiveCaptureRejectsDuplexBeforeCOM(t *testing.T) {
	input := &driver.Info{ID: "capture"}
	output := &driver.Info{ID: "render"}
	_, err := openCaptureStream(driver.Request{
		Input: input, Output: output, Exclusive: true,
		InChannels: 2, OutChannels: 2, SampleRate: 48_000, Period: 480, Periods: 2,
	})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("exclusive duplex open error = %v, want driver.ErrUnsupported", err)
	}
}

func TestNativeExclusiveCaptureLifecycle(t *testing.T) {
	if os.Getenv("TYMBAL_REQUIRE_EXCLUSIVE_CAPTURE") != "1" {
		t.Skip("set TYMBAL_REQUIRE_EXCLUSIVE_CAPTURE=1 to require native exclusive capture")
	}

	devices, err := New().Devices()
	if err != nil {
		t.Fatalf("enumerate WASAPI endpoints for required exclusive test: %v", err)
	}
	var input *driver.Info
	for i := range devices {
		if devices[i].Inputs > 0 && len(devices[i].SampleRates) != 0 {
			input = &devices[i]
			if devices[i].Default == 2 {
				break
			}
		}
	}
	if input == nil {
		t.Fatal("TYMBAL_REQUIRE_EXCLUSIVE_CAPTURE=1 but Windows exposes no active capture endpoint")
	}

	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		stream, openErr := New().Open(driver.Request{
			Input: input, InChannels: input.Inputs,
			SampleRate: input.SampleRates[0], Period: 480, Periods: 2,
			Exclusive: true,
		})
		if openErr != nil {
			done <- fmt.Errorf("open required exclusive capture: %w", openErr)
			return
		}
		if startErr := stream.Start(); startErr != nil {
			_ = stream.Stop()
			_ = stream.Close()
			done <- fmt.Errorf("start required exclusive capture: %w", startErr)
			return
		}
		params := stream.Params()
		wantBytes := params.Period * params.InChannels * audioformat.BytesPerSample(params.InFormat)
		servicePeriod := func() error {
			if waitErr := stream.Wait(); waitErr != nil {
				return fmt.Errorf("wait for an exclusive capture packet: %w", waitErr)
			}
			in, out := stream.Buffers()
			if len(in) != wantBytes || len(out) != 0 {
				return fmt.Errorf("exclusive capture buffers = (%d, %d), want (%d, 0)", len(in), len(out), wantBytes)
			}
			return stream.Commit()
		}
		if err := servicePeriod(); err != nil {
			_ = stream.Stop()
			_ = stream.Close()
			done <- err
			return
		}
		allocs := testing.AllocsPerRun(5, func() {
			if err := servicePeriod(); err != nil {
				t.Errorf("exclusive Wait/Buffers/Commit: %v", err)
			}
		})
		stopErr := stream.Stop()
		closeErr := stream.Close()
		if allocs != 0 {
			done <- fmt.Errorf("exclusive Wait/Buffers/Commit allocated %.2f times per period", allocs)
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
		t.Fatal("required exclusive capture lifecycle did not finish within 15 seconds")
	}
}
