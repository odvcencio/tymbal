//go:build windows

package wasapi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
	"m31labs.dev/tymbal/internal/format"
)

func TestDescribeWaveFormatDirectLayouts(t *testing.T) {
	tests := []struct {
		name string
		wave waveFormatEx
		want format.Format
	}{
		{
			name: "float32",
			wave: waveFormatEx{formatTag: waveFormatIEEEFloat, channels: 2, samplesPerSec: 48000, avgBytesPerSec: 384000, blockAlign: 8, bitsPerSample: 32},
			want: format.F32LE,
		},
		{
			name: "signed 16 bit",
			wave: waveFormatEx{formatTag: waveFormatPCM, channels: 2, samplesPerSec: 44100, avgBytesPerSec: 176400, blockAlign: 4, bitsPerSample: 16},
			want: format.S16LE,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got, channels, rate, err := describeWaveFormat(uintptr(unsafe.Pointer(&tt.wave)))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want || channels != 2 || rate <= 0 {
				t.Fatalf("describeWaveFormat = (%q, %d, %d), want (%q, 2, positive)", got, channels, rate, tt.want)
			}
		})
	}

	var extensible [40]byte
	binary.LittleEndian.PutUint16(extensible[0:2], waveFormatExtensibleTag)
	binary.LittleEndian.PutUint16(extensible[2:4], 2)
	binary.LittleEndian.PutUint32(extensible[4:8], 48000)
	binary.LittleEndian.PutUint32(extensible[8:12], 192000)
	binary.LittleEndian.PutUint16(extensible[12:14], 4)
	binary.LittleEndian.PutUint16(extensible[14:16], 16)
	binary.LittleEndian.PutUint16(extensible[16:18], 22)
	binary.LittleEndian.PutUint16(extensible[18:20], 16)
	binary.LittleEndian.PutUint32(extensible[20:24], 3)
	binary.LittleEndian.PutUint32(extensible[24:28], waveSubFormatPCM.data1)
	binary.LittleEndian.PutUint16(extensible[28:30], waveSubFormatPCM.data2)
	binary.LittleEndian.PutUint16(extensible[30:32], waveSubFormatPCM.data3)
	copy(extensible[32:40], waveSubFormatPCM.data4[:])
	_, got, channels, rate, err := describeWaveFormat(uintptr(unsafe.Pointer(&extensible[0])))
	if err != nil {
		t.Fatal(err)
	}
	if got != format.S16LE || channels != 2 || rate != 48000 {
		t.Fatalf("extensible format = (%q, %d, %d), want (%q, 2, 48000)", got, channels, rate, format.S16LE)
	}
}

func TestDescribeWaveFormatRejectsPaddedPCM24(t *testing.T) {
	var extensible [40]byte
	binary.LittleEndian.PutUint16(extensible[0:2], waveFormatExtensibleTag)
	binary.LittleEndian.PutUint16(extensible[2:4], 2)
	binary.LittleEndian.PutUint32(extensible[4:8], 48000)
	binary.LittleEndian.PutUint32(extensible[8:12], 384000)
	binary.LittleEndian.PutUint16(extensible[12:14], 8)
	binary.LittleEndian.PutUint16(extensible[14:16], 32)
	binary.LittleEndian.PutUint16(extensible[16:18], 22)
	binary.LittleEndian.PutUint16(extensible[18:20], 24)
	binary.LittleEndian.PutUint32(extensible[20:24], 3)
	binary.LittleEndian.PutUint32(extensible[24:28], waveSubFormatPCM.data1)
	binary.LittleEndian.PutUint16(extensible[28:30], waveSubFormatPCM.data2)
	binary.LittleEndian.PutUint16(extensible[30:32], waveSubFormatPCM.data3)
	copy(extensible[32:40], waveSubFormatPCM.data4[:])
	if _, _, _, _, err := describeWaveFormat(uintptr(unsafe.Pointer(&extensible[0]))); !errors.Is(err, driver.ErrFormat) {
		t.Fatalf("describeWaveFormat error = %v, want driver.ErrFormat", err)
	}
}

func TestSharedPeriodAndBufferGeometry(t *testing.T) {
	periods := sharedPeriodRange{defaultFrames: 480, fundamentalFrames: 4, minFrames: 48, maxFrames: 480}
	for _, period := range []uint32{48, 64, 128, 480} {
		if !periodAllowed(period, periods) {
			t.Errorf("periodAllowed(%d) = false", period)
		}
	}
	for _, period := range []uint32{0, 44, 49, 484} {
		if periodAllowed(period, periods) {
			t.Errorf("periodAllowed(%d) = true", period)
		}
	}
	if got := nearestAllowedPeriod(256, sharedPeriodRange{fundamentalFrames: 6, minFrames: 48, maxFrames: 480}); got != 258 {
		t.Fatalf("nearestAllowedPeriod(256) = %d, want 258", got)
	}
	if got := nearestAllowedPeriod(1, periods); got != 48 {
		t.Fatalf("nearestAllowedPeriod below minimum = %d, want 48", got)
	}
	count, latency, err := bufferGeometry(960, 480, 48000)
	if err != nil || count != 2 || latency != 20*time.Millisecond {
		t.Fatalf("bufferGeometry = (%d, %v, %v), want (2, 20ms, nil)", count, latency, err)
	}
	if _, _, err := bufferGeometry(1000, 480, 48000); !errors.Is(err, driver.ErrFormat) {
		t.Fatalf("misaligned buffer geometry error = %v, want driver.ErrFormat", err)
	}
}

func TestNativeSharedRenderEventAndInterrupt(t *testing.T) {
	devices, err := New().Devices()
	if err != nil {
		t.Fatal(err)
	}
	var output *driver.Info
	for i := range devices {
		if devices[i].Outputs > 0 && len(devices[i].SampleRates) > 0 {
			output = &devices[i]
			break
		}
	}
	if output == nil {
		t.Skip("Windows exposes no active render endpoint")
	}

	period, err := defaultSharedPeriod(output.ID)
	if err != nil {
		if errors.Is(err, driver.ErrUnsupported) {
			t.Skipf("shared WASAPI path unavailable: %v", err)
		}
		t.Fatal(err)
	}

	// Run all stream COM calls on one locked thread, matching Stream.run.
	done := make(chan error, 1)
	waiting := make(chan driver.Stream, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		stream, openErr := openRenderStream(driver.Request{
			Output: output, OutChannels: output.Outputs,
			SampleRate: output.SampleRates[0], Period: period, Periods: 2,
		})
		if openErr != nil {
			done <- openErr
			return
		}
		if startErr := stream.Start(); startErr != nil {
			_ = stream.Stop()
			_ = stream.Close()
			done <- startErr
			return
		}
		_, out := stream.Buffers()
		params := stream.Params()
		if want := params.Period * params.OutChannels * format.BytesPerSample(params.OutFormat); len(out) != want {
			_ = stream.Stop()
			_ = stream.Close()
			done <- errors.New("render buffer does not contain one complete period")
			return
		}
		if waitErr := stream.Wait(); waitErr != nil {
			_ = stream.Stop()
			_ = stream.Close()
			done <- waitErr
			return
		}
		if commitErr := stream.Commit(); commitErr != nil {
			_ = stream.Stop()
			_ = stream.Close()
			done <- commitErr
			return
		}
		waiting <- stream
		if waitErr := stream.Wait(); !errors.Is(waitErr, driver.ErrInterrupted) {
			_ = stream.Stop()
			_ = stream.Close()
			done <- fmt.Errorf("interrupted Wait() = %v, want driver.ErrInterrupted", waitErr)
			return
		}
		stopErr := stream.Stop()
		closeErr := stream.Close()
		done <- errors.Join(stopErr, closeErr)
	}()

	var interruptTarget driver.Stream
	select {
	case interruptTarget = <-waiting:
		time.Sleep(time.Millisecond)
		interruptTarget.Interrupt()
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		return
	case <-time.After(3 * time.Second):
		t.Fatal("native WASAPI render event timed out")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("native WASAPI render event or interrupt timed out")
	}
}

func defaultSharedPeriod(endpointID string) (int, error) {
	period, err := withSTA(func() (uint32, error) {
		enumerator, err := createEnumerator()
		if err != nil {
			return 0, err
		}
		defer release(enumerator)
		device, err := getDevice(enumerator, endpointID)
		if err != nil {
			return 0, err
		}
		defer release(device)
		client, err := activateAudioClient3(device)
		if err != nil {
			return 0, err
		}
		defer release(client)
		wave, err := getMixFormat(client)
		if err != nil {
			return 0, err
		}
		defer procCoTaskMemFree.Call(wave)
		periods, err := getSharedPeriodRange(client, wave)
		return periods.defaultFrames, err
	})
	if err != nil {
		return 0, err
	}
	return int(period), nil
}
