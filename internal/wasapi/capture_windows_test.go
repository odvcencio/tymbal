//go:build windows

package wasapi

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"m31labs.dev/tymbal/internal/driver"
	"m31labs.dev/tymbal/internal/format"
)

func TestCaptureStagerAssemblesVariablePacketsIntoWholePeriods(t *testing.T) {
	stager, err := newCaptureStager(8, 4, 2, 10_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := stager.appendPacket([]byte{0, 1, 2, 3, 4, 5}, 3, 0, 100, 1000); err != nil {
		t.Fatal(err)
	}
	if stager.preparePeriod() {
		t.Fatal("three staged frames produced a four-frame period")
	}
	if err := stager.appendPacket([]byte{6, 7, 8, 9}, 2, 0, 103, 1003); err != nil {
		t.Fatal(err)
	}
	if !stager.preparePeriod() {
		t.Fatal("five staged frames did not produce a four-frame period")
	}
	if !bytes.Equal(stager.input, []byte{0, 1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("first period = %v, want [0 1 2 3 4 5 6 7]", stager.input)
	}
	if stager.activeQPCPosition != 1000 || !stager.activeQPCPositionValid {
		t.Fatalf("first period QPC = (%d, %v), want (1000, true)", stager.activeQPCPosition, stager.activeQPCPositionValid)
	}
	stager.commitPeriod()
	if stager.frames != 1 || stager.stageDevicePosition != 104 || stager.stageQPCPosition != 1004 {
		t.Fatalf("after first commit: frames=%d device=%d qpc=%d, want 1/104/1004", stager.frames, stager.stageDevicePosition, stager.stageQPCPosition)
	}
	if err := stager.appendPacket([]byte{10, 11, 12, 13, 14, 15}, 3, 0, 105, 1005); err != nil {
		t.Fatal(err)
	}
	if !stager.preparePeriod() {
		t.Fatal("staged remainder and next packet did not produce a complete period")
	}
	if !bytes.Equal(stager.input, []byte{8, 9, 10, 11, 12, 13, 14, 15}) {
		t.Fatalf("second period = %v, want [8 9 10 11 12 13 14 15]", stager.input)
	}
	if stager.activeQPCPosition != 1004 || !stager.activeQPCPositionValid {
		t.Fatalf("second period QPC = (%d, %v), want (1004, true)", stager.activeQPCPosition, stager.activeQPCPositionValid)
	}
}

func TestCaptureStagerSilenceDiscontinuityAndTimestampError(t *testing.T) {
	stager, err := newCaptureStager(8, 3, 2, 48_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := stager.appendPacket([]byte{1, 2, 3, 4}, 2, 0, 10, 100); err != nil {
		t.Fatal(err)
	}
	if err := stager.appendPacket(nil, 2, audclntBufferFlagSilent, 12, 102); err != nil {
		t.Fatal(err)
	}
	if !stager.preparePeriod() {
		t.Fatal("data and silent packets did not produce a complete period")
	}
	if !bytes.Equal(stager.input, []byte{1, 2, 3, 4, 0, 0}) {
		t.Fatalf("silence period = %v, want [1 2 3 4 0 0]", stager.input)
	}
	stager.commitPeriod()

	packet := []byte{7, 8, 9, 10, 11, 12}
	if err := stager.appendPacket(packet, 3, audclntCaptureBufferFlagDataDiscontinuity, 30, 200); err != nil {
		t.Fatal(err)
	}
	if stager.dropouts != 1 || stager.frames != 3 {
		t.Fatalf("after discontinuity: dropouts=%d staged=%d, want 1/3", stager.dropouts, stager.frames)
	}
	if !stager.preparePeriod() || !bytes.Equal(stager.input, packet) {
		t.Fatalf("discontinuity period = %v, want %v", stager.input, packet)
	}
	if stager.activeQPCPosition != 200 || !stager.activeQPCPositionValid {
		t.Fatalf("post-discontinuity QPC = (%d, %v), want (200, true)", stager.activeQPCPosition, stager.activeQPCPositionValid)
	}
	stager.commitPeriod()

	if err := stager.appendPacket(make([]byte, 6), 3, audclntCaptureBufferFlagTimestampError, 40, 300); err != nil {
		t.Fatal(err)
	}
	if !stager.preparePeriod() {
		t.Fatal("timestamp-error packet did not produce a complete period")
	}
	if stager.activeQPCPositionValid || stager.stageDevicePositionValid {
		t.Fatal("timestamp-error packet exposed an invalid capture position")
	}
}

func TestCaptureStagerDetectsDevicePositionGap(t *testing.T) {
	stager, err := newCaptureStager(8, 4, 1, 48_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := stager.appendPacket([]byte{1, 2}, 2, 0, 10, 100); err != nil {
		t.Fatal(err)
	}
	if err := stager.appendPacket([]byte{3, 4}, 2, 0, 13, 103); err != nil {
		t.Fatal(err)
	}
	if stager.dropouts != 1 || stager.frames != 2 || stager.preparePeriod() {
		t.Fatalf("gap handling: dropouts=%d staged=%d ready=%v, want 1/2/false", stager.dropouts, stager.frames, stager.ready)
	}
	if err := stager.appendPacket([]byte{5, 6}, 2, 0, 15, 105); err != nil {
		t.Fatal(err)
	}
	if !stager.preparePeriod() || !bytes.Equal(stager.input, []byte{3, 4, 5, 6}) {
		t.Fatalf("gap recovery period = %v, want [3 4 5 6]", stager.input)
	}
}

func TestCaptureBufferGeometryAllowsVariablePacketQuantum(t *testing.T) {
	periods, latency, err := captureBufferGeometry(1001, 480, 48_000)
	if err != nil || periods != 3 || latency != time.Duration(uint64(1001)*uint64(time.Second)/48_000) {
		t.Fatalf("captureBufferGeometry = (%d, %v, %v), want (3, exact 1001-frame latency, nil)", periods, latency, err)
	}
}

func TestOpenRejectsDuplexCaptureUntilSharedClockExists(t *testing.T) {
	input := &driver.Info{ID: "capture"}
	output := &driver.Info{ID: "render"}
	_, err := New().Open(driver.Request{
		Input: input, Output: output,
		InChannels: 2, OutChannels: 2,
		SampleRate: 48_000, Period: 480, Periods: 2,
	})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("duplex Open error = %v, want driver.ErrUnsupported", err)
	}
}

func TestNativeSharedCaptureEventAndInterrupt(t *testing.T) {
	devices, err := New().Devices()
	if err != nil {
		t.Fatal(err)
	}
	var input *driver.Info
	for i := range devices {
		if devices[i].Inputs > 0 && len(devices[i].SampleRates) > 0 {
			input = &devices[i]
			break
		}
	}
	if input == nil {
		if os.Getenv("TYMBAL_REQUIRE_CAPTURE") == "1" {
			t.Fatal("Windows exposes no active capture endpoint")
		}
		t.Skip("Windows exposes no active capture endpoint; set TYMBAL_REQUIRE_CAPTURE=1 to require capture hardware")
	}
	period, err := defaultSharedPeriod(input.ID)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	waiting := make(chan driver.Stream, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		stream, openErr := New().Open(driver.Request{
			Input: input, InChannels: input.Inputs,
			SampleRate: input.SampleRates[0], Period: period, Periods: 2,
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
		params := stream.Params()
		if params.InChannels != input.Inputs || params.OutChannels != 0 || params.Period <= 0 {
			_ = stream.Stop()
			_ = stream.Close()
			done <- fmt.Errorf("capture Params = %+v, want input-only negotiated stream", params)
			return
		}
		if waitErr := stream.Wait(); waitErr != nil {
			_ = stream.Stop()
			_ = stream.Close()
			done <- waitErr
			return
		}
		in, out := stream.Buffers()
		wantBytes := params.Period * params.InChannels * format.BytesPerSample(params.InFormat)
		if len(in) != wantBytes || len(out) != 0 {
			_ = stream.Stop()
			_ = stream.Close()
			done <- fmt.Errorf("capture buffers = (%d, %d), want (%d, 0)", len(in), len(out), wantBytes)
			return
		}
		if commitErr := stream.Commit(); commitErr != nil {
			_ = stream.Stop()
			_ = stream.Close()
			done <- commitErr
			return
		}
		allocs := testing.AllocsPerRun(5, func() {
			if waitErr := stream.Wait(); waitErr != nil {
				t.Fatalf("capture Wait during allocation check: %v", waitErr)
			}
			if in, out := stream.Buffers(); len(in) != wantBytes || len(out) != 0 {
				t.Fatalf("capture Buffers during allocation check = (%d, %d), want (%d, 0)", len(in), len(out), wantBytes)
			}
			if commitErr := stream.Commit(); commitErr != nil {
				t.Fatalf("capture Commit during allocation check: %v", commitErr)
			}
		})
		if allocs != 0 {
			_ = stream.Stop()
			_ = stream.Close()
			done <- fmt.Errorf("capture Wait/Buffers/Commit allocated %.2f times per period", allocs)
			return
		}
		waiting <- stream
		for {
			waitErr := stream.Wait()
			if errors.Is(waitErr, driver.ErrInterrupted) {
				break
			}
			if waitErr != nil {
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
	case <-time.After(5 * time.Second):
		t.Fatal("native WASAPI capture event timed out")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("native WASAPI capture event or interrupt timed out")
	}
}
