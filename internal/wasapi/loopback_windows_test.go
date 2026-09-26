//go:build windows

package wasapi

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"m31labs.dev/tymbal/internal/driver"
	"m31labs.dev/tymbal/internal/format"
)

func TestNativeSharedRenderLoopback(t *testing.T) {
	if os.Getenv("TYMBAL_REQUIRE_LOOPBACK") != "1" {
		t.Skip("set TYMBAL_REQUIRE_LOOPBACK=1 to run native WASAPI render loopback")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	devices, err := New().Devices()
	if err != nil {
		t.Fatal(err)
	}
	var output *driver.Info
	for i := range devices {
		if devices[i].Default&1 != 0 && devices[i].Outputs > 0 && len(devices[i].SampleRates) != 0 {
			output = &devices[i]
			break
		}
	}
	if output == nil {
		t.Fatal("Windows exposes no default WASAPI render endpoint for loopback")
	}
	period, err := defaultSharedPeriod(output.ID)
	if err != nil {
		t.Fatalf("query default shared period: %v", err)
	}
	req := driver.Request{
		Output: output, OutChannels: output.Outputs,
		SampleRate: output.SampleRates[0], Period: period, Periods: 2,
	}
	renderDriver, err := openRenderStream(req)
	if err != nil {
		t.Fatalf("open silent render stream: %v", err)
	}
	loopbackDriver, err := openLoopbackCaptureStream(req)
	if err != nil {
		_ = renderDriver.Close()
		t.Fatalf("open WASAPI render loopback capture: %v", err)
	}
	render := renderDriver.(*wasapiStream)
	loopback := loopbackDriver.(*captureStream)
	defer func() {
		if err := loopback.Close(); err != nil {
			t.Errorf("close loopback capture: %v", err)
		}
		if err := render.Close(); err != nil {
			t.Errorf("close render: %v", err)
		}
	}()
	if loopback.params.SampleRate != render.params.SampleRate || loopback.params.Period != render.params.Period {
		t.Fatalf("render and loopback parameters differ: render=%+v loopback=%+v", render.params, loopback.params)
	}
	if err := render.Start(); err != nil {
		t.Fatalf("start silent render: %v", err)
	}
	if err := loopback.Start(); err != nil {
		_ = render.Stop()
		t.Fatalf("start render loopback capture: %v", err)
	}

	bps := format.BytesPerSample(loopback.params.InFormat)
	frameBytes := loopback.params.InChannels * bps
	captured := make([]byte, loopback.params.SampleRate*2*frameBytes)
	capturedBytes, periods, nonzero := 0, 0, 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := loopback.Wait(); err != nil {
			_ = loopback.Stop()
			_ = render.Stop()
			t.Fatalf("wait for loopback capture: %v", err)
		}
		if err := render.Wait(); err != nil {
			_ = loopback.Stop()
			_ = render.Stop()
			t.Fatalf("wait for silent render: %v", err)
		}
		in, _ := loopback.Buffers()
		_, out := render.Buffers()
		if len(in) != loopback.params.Period*frameBytes || len(out) != render.params.Period*render.params.OutChannels*format.BytesPerSample(render.params.OutFormat) {
			_ = loopback.Stop()
			_ = render.Stop()
			t.Fatalf("incomplete period buffers: input=%d output=%d", len(in), len(out))
		}
		clear(out)
		for _, sample := range out {
			if sample != 0 {
				_ = loopback.Stop()
				_ = render.Stop()
				t.Fatal("loopback render test attempted to send nonzero output")
			}
		}
		for _, sample := range in {
			if sample != 0 {
				nonzero++
			}
		}
		if capturedBytes+len(in) <= len(captured) {
			capturedBytes += copy(captured[capturedBytes:], in)
		}
		periods++
		if err := loopback.Commit(); err != nil {
			_ = loopback.Stop()
			_ = render.Stop()
			t.Fatalf("commit loopback period: %v", err)
		}
		if err := render.Commit(); err != nil {
			_ = loopback.Stop()
			_ = render.Stop()
			t.Fatalf("commit silent render period: %v", err)
		}
	}
	if err := loopback.Stop(); err != nil {
		t.Errorf("stop loopback capture: %v", err)
	}
	if err := render.Stop(); err != nil {
		t.Errorf("stop silent render: %v", err)
	}
	if periods < 2 || capturedBytes == 0 {
		t.Fatalf("loopback produced insufficient periods: periods=%d bytes=%d", periods, capturedBytes)
	}
	evidence, err := writeLoopbackWave(loopback.params, captured[:capturedBytes])
	if err != nil {
		t.Fatalf("write loopback capture evidence: %v", err)
	}
	t.Logf("WASAPI shared render loopback: rate=%d period=%d channels=%d format=%s periods=%d captured_bytes=%d nonzero_bytes=%d evidence=%s; rendered output was zero-filled",
		loopback.params.SampleRate, loopback.params.Period, loopback.params.InChannels, loopback.params.InFormat,
		periods, capturedBytes, nonzero, evidence)
}

func writeLoopbackWave(params driver.Params, samples []byte) (string, error) {
	bps := format.BytesPerSample(params.InFormat)
	if params.SampleRate <= 0 || params.InChannels <= 0 || bps <= 0 || len(samples) > int(^uint32(0))-36 {
		return "", fmt.Errorf("invalid loopback WAV geometry")
	}
	wave := make([]byte, 44+len(samples))
	copy(wave[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wave[4:8], uint32(len(wave)-8))
	copy(wave[8:12], "WAVE")
	copy(wave[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wave[16:20], 16)
	waveFormatTag := uint16(1)
	if params.InFormat == format.F32LE {
		waveFormatTag = 3
	}
	binary.LittleEndian.PutUint16(wave[20:22], waveFormatTag)
	binary.LittleEndian.PutUint16(wave[22:24], uint16(params.InChannels))
	binary.LittleEndian.PutUint32(wave[24:28], uint32(params.SampleRate))
	binary.LittleEndian.PutUint32(wave[28:32], uint32(params.SampleRate*params.InChannels*bps))
	binary.LittleEndian.PutUint16(wave[32:34], uint16(params.InChannels*bps))
	binary.LittleEndian.PutUint16(wave[34:36], uint16(bps*8))
	copy(wave[36:40], "data")
	binary.LittleEndian.PutUint32(wave[40:44], uint32(len(samples)))
	copy(wave[44:], samples)
	file, err := os.CreateTemp(os.TempDir(), "tymbal-wasapi-loopback-*.wav")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if _, err := file.Write(wave); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return path, nil
}
