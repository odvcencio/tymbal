//go:build windows

package tymbal_test

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"m31labs.dev/tymbal"
)

func TestNativePublicDuplex(t *testing.T) {
	if os.Getenv("TYMBAL_REQUIRE_DUPLEX") != "1" {
		t.Skip("set TYMBAL_REQUIRE_DUPLEX=1 to run native Windows duplex")
	}
	var host tymbal.Host
	for _, candidate := range tymbal.Hosts() {
		if candidate.Name() == "wasapi" {
			host = candidate
			break
		}
	}
	if host.Name() == "" {
		t.Fatal("Windows exposes no WASAPI host")
	}
	devices, err := host.Devices()
	if err != nil {
		t.Fatalf("enumerate WASAPI devices: %v", err)
	}
	var input, output *tymbal.Device
	for i := range devices {
		if output == nil && devices[i].Default&tymbal.Output != 0 && devices[i].Outputs > 0 {
			output = &devices[i]
		}
		if input == nil && devices[i].Default&tymbal.Input != 0 && devices[i].Inputs > 0 {
			input = &devices[i]
		}
	}
	if input == nil || output == nil {
		t.Fatalf("native duplex requires default capture and render endpoints (input=%v output=%v)", input != nil, output != nil)
	}
	rate := 0
	for _, candidate := range output.SampleRates {
		for _, supported := range input.SampleRates {
			if candidate == supported {
				rate = candidate
				break
			}
		}
		if rate != 0 {
			break
		}
	}
	if rate == 0 {
		t.Fatal("default WASAPI duplex endpoints have no common advertised sample rate")
	}

	const secondsToCapture = 3
	maxFrames := rate * secondsToCapture
	maxSamples := maxFrames * input.Inputs
	captured := make([]float32, maxSamples)
	var capturedFrames int
	var callbacks atomic.Uint64
	var invalid atomic.Bool
	var previousFrame uint64
	var haveFrame bool
	actualPeriod := 0
	stream, err := tymbal.Open(host, tymbal.Config{
		Input: input, Output: output,
		InChannels: input.Inputs, OutChannels: output.Outputs,
		SampleRate: rate, Period: 480, Periods: 2,
	}, func(tm tymbal.Time, in, out [][]float32) {
		if len(in) != input.Inputs || len(out) != output.Outputs {
			invalid.Store(true)
		} else {
			for _, channel := range in {
				if len(channel) != actualPeriod {
					invalid.Store(true)
				}
			}
			for _, channel := range out {
				if len(channel) != actualPeriod {
					invalid.Store(true)
				}
				clear(channel)
			}
			frames := len(in[0])
			if capturedFrames+frames <= maxFrames {
				for frame := 0; frame < frames; frame++ {
					for channel := range in {
						captured[(capturedFrames+frame)*input.Inputs+channel] = in[channel][frame]
					}
				}
				capturedFrames += frames
			}
		}
		if !haveFrame {
			if tm.Frame != 0 {
				invalid.Store(true)
			}
			haveFrame = true
		} else if tm.Frame != previousFrame+uint64(actualPeriod) {
			invalid.Store(true)
		}
		previousFrame = tm.Frame
		callbacks.Add(1)
	})
	if err != nil {
		t.Fatalf("open shared WASAPI duplex: %v", err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			t.Errorf("close duplex stream: %v", err)
		}
	}()
	actualPeriod = stream.Actual().Period
	if actualPeriod <= 0 || stream.Actual().InChannels != input.Inputs || stream.Actual().OutChannels != output.Outputs {
		t.Fatalf("invalid duplex parameters: %+v", stream.Actual())
	}
	if err := stream.Start(); err != nil {
		t.Fatalf("start shared WASAPI duplex: %v", err)
	}
	time.Sleep(secondsToCapture * time.Second)
	if err := stream.Stop(); err != nil {
		t.Fatalf("stop shared WASAPI duplex: %v", err)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("shared WASAPI duplex failed: %v", err)
	}
	var stats tymbal.Stats
	stream.Stats(&stats)
	if callbacks.Load() < 2 || stats.Callbacks != callbacks.Load() || invalid.Load() || capturedFrames == 0 {
		t.Fatalf("duplex callback incomplete: observed=%d reported=%d frames=%d invalid=%t", callbacks.Load(), stats.Callbacks, capturedFrames, invalid.Load())
	}
	if stats.Dropouts != 0 {
		t.Fatalf("duplex audio gate: dropouts=%d, want zero", stats.Dropouts)
	}
	evidence, err := writeFloatWave("tymbal-wasapi-duplex-", rate, input.Inputs, captured[:capturedFrames*input.Inputs])
	if err != nil {
		t.Fatalf("write duplex capture evidence: %v", err)
	}
	t.Logf("WASAPI shared duplex: rate=%d period=%d input_channels=%d output_channels=%d callbacks=%d dropouts=%d captured_frames=%d evidence=%s; render samples were all zero",
		rate, actualPeriod, input.Inputs, output.Outputs, callbacks.Load(), stats.Dropouts, capturedFrames, evidence)
}

func writeFloatWave(prefix string, rate, channels int, samples []float32) (string, error) {
	if rate <= 0 || channels <= 0 || len(samples) > (int(^uint32(0))-36)/4 {
		return "", fmt.Errorf("invalid WAV geometry")
	}
	dataBytes := len(samples) * 4
	wave := make([]byte, 44+dataBytes)
	copy(wave[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wave[4:8], uint32(len(wave)-8))
	copy(wave[8:12], "WAVE")
	copy(wave[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wave[16:20], 16)
	binary.LittleEndian.PutUint16(wave[20:22], 3)
	binary.LittleEndian.PutUint16(wave[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(wave[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(wave[28:32], uint32(rate*channels*4))
	binary.LittleEndian.PutUint16(wave[32:34], uint16(channels*4))
	binary.LittleEndian.PutUint16(wave[34:36], 32)
	copy(wave[36:40], "data")
	binary.LittleEndian.PutUint32(wave[40:44], uint32(dataBytes))
	for i, sample := range samples {
		binary.LittleEndian.PutUint32(wave[44+i*4:], math.Float32bits(sample))
	}
	file, err := os.CreateTemp(os.TempDir(), prefix+"*.wav")
	if err != nil {
		return "", err
	}
	path := filepath.Clean(file.Name())
	if _, err := file.Write(wave); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return path, nil
}
