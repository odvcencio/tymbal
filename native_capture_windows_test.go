//go:build windows

package tymbal_test

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"m31labs.dev/tymbal"
)

func TestNativePublicCapture(t *testing.T) {
	if os.Getenv("TYMBAL_REQUIRE_CAPTURE") != "1" {
		t.Skip("set TYMBAL_REQUIRE_CAPTURE=1 to run the native Windows capture test")
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
	var input, firstInput *tymbal.Device
	for i := range devices {
		if devices[i].Inputs > 0 && len(devices[i].SampleRates) > 0 {
			if firstInput == nil {
				firstInput = &devices[i]
			}
			if devices[i].Default&tymbal.Input != 0 {
				input = &devices[i]
				break
			}
		}
	}
	if input == nil {
		input = firstInput
	}
	if input == nil {
		t.Fatal("Windows exposes no active WASAPI capture endpoint")
	}

	var callbacks atomic.Uint64
	var invalid atomic.Bool
	var inputNanoSamples atomic.Uint64
	var previousFrame uint64
	var previousInputNano int64
	var haveFrame bool
	const requestedPeriod = 480
	actualPeriod := requestedPeriod
	stream, err := tymbal.Open(host, tymbal.Config{
		Input: input, InChannels: input.Inputs,
		SampleRate: input.SampleRates[0], Period: requestedPeriod, Periods: 2,
	}, func(tm tymbal.Time, in, out [][]float32) {
		if !haveFrame && tm.Frame != 0 {
			invalid.Store(true)
		}
		if len(in) != input.Inputs || len(out) != 0 {
			invalid.Store(true)
		} else {
			for _, channel := range in {
				if len(channel) != actualPeriod {
					invalid.Store(true)
					break
				}
			}
		}
		if haveFrame && tm.Frame != previousFrame+uint64(actualPeriod) {
			invalid.Store(true)
		}
		previousFrame, haveFrame = tm.Frame, true
		if tm.InputNano != 0 {
			validInputNano := tm.InputNano > 0
			if previousInputNano > 0 && (tm.InputNano < previousInputNano || tm.InputNano-previousInputNano > int64(2*time.Second)) {
				validInputNano = false
			}
			if !validInputNano {
				invalid.Store(true)
			} else {
				inputNanoSamples.Add(1)
			}
			if tm.InputNano > 0 {
				previousInputNano = tm.InputNano
			}
		}
		callbacks.Add(1)
	})
	if err != nil {
		t.Fatalf("open WASAPI capture stream: %v", err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			t.Errorf("deferred close: %v", err)
		}
	}()

	actual := stream.Actual()
	if actual.InChannels != input.Inputs || actual.OutChannels != 0 || actual.Period <= 0 {
		t.Fatalf("unexpected capture parameters: %+v", actual)
	}
	// WASAPI shared mode may grant a different period than the request.
	actualPeriod = actual.Period
	if err := stream.Start(); err != nil {
		t.Fatalf("start WASAPI capture stream: %v", err)
	}
	time.Sleep(time.Second)
	if err := stream.Stop(); err != nil {
		t.Fatalf("stop WASAPI capture stream: %v", err)
	}
	if err := stream.Stop(); err != nil {
		t.Fatalf("repeat stop WASAPI capture stream: %v", err)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("capture stream failed: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close WASAPI capture stream: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("repeat close WASAPI capture stream: %v", err)
	}
	if callbacks.Load() < 2 {
		t.Fatalf("capture produced only %d complete callback periods", callbacks.Load())
	}
	var stats tymbal.Stats
	stream.Stats(&stats)
	if stats.Callbacks != callbacks.Load() {
		t.Fatalf("stream reports %d callbacks, observed %d", stats.Callbacks, callbacks.Load())
	}
	if inputNanoSamples.Load() == 0 {
		t.Fatal("capture provided no valid nonzero input timestamps")
	}
	if invalid.Load() {
		t.Fatal("capture callback reported incomplete input, output data, discontinuous frame positions, or invalid input timestamps")
	}
	actual = stream.Actual()
	t.Logf("WASAPI capture: callbacks=%d dropouts=%d priority=%s valid_input_timestamps=%d", stats.Callbacks, stats.Dropouts, actual.Priority, inputNanoSamples.Load())
}
