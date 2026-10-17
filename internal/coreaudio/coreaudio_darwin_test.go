//go:build darwin && (amd64 || arm64)

package coreaudio

import (
	"encoding/binary"
	"errors"
	"math"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"m31labs.dev/tymbal/internal/driver"
)

func TestDevicesAndDefault(t *testing.T) {
	b := New()
	if b.Name() != "coreaudio" {
		t.Errorf("Name() = %q", b.Name())
	}
	devices, err := b.Devices()
	if errors.Is(err, errNoDevice) {
		t.Skipf("no audio server: %v", err)
	}
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devices) == 0 {
		t.Skip("no audio devices on this machine")
	}
	seen := map[string]bool{}
	for _, d := range devices {
		t.Logf("device %q id=%q in=%d out=%d rates=%v period=%d..%d default=%d",
			d.Name, d.ID, d.Inputs, d.Outputs, d.SampleRates, d.MinPeriod, d.MaxPeriod, d.Default)
		if d.ID == "" || d.Name == "" || d.Inputs+d.Outputs == 0 {
			t.Errorf("incomplete device description %+v", d)
		}
		if seen[d.ID] {
			t.Errorf("duplicate device ID %q", d.ID)
		}
		seen[d.ID] = true
	}

	for _, dir := range []uint8{dirOutput, dirInput} {
		def, err := b.Default(dir)
		if errors.Is(err, errNoDevice) {
			t.Logf("no default device for direction %d", dir)
			continue
		}
		if err != nil {
			t.Fatalf("Default(%d): %v", dir, err)
		}
		if def.Default&dir == 0 {
			t.Errorf("Default(%d) = %+v, which does not mark itself as that default", dir, def)
		}
		found := false
		for _, d := range devices {
			if d.ID == def.ID {
				found = d.Default&dir != 0
			}
		}
		if !found {
			t.Errorf("Default(%d) = %q is not flagged in Devices()", dir, def.ID)
		}
	}
	if _, err := b.Default(0); err == nil {
		t.Error("Default(0) succeeded")
	}
}

func TestOpenRejectsWhatItCannotDo(t *testing.T) {
	b := New()
	out := driver.Info{ID: "no-such-device", Name: "nothing", Outputs: 2, SampleRates: []int{48000}}
	for name, tc := range map[string]struct {
		req  driver.Request
		want error
	}{
		"input":       {driver.Request{Input: &out, InChannels: 1, SampleRate: 48000, Period: 128, Periods: 2}, driver.ErrUnsupported},
		"duplex":      {driver.Request{Output: &out, Input: &out, OutChannels: 2, InChannels: 1, SampleRate: 48000, Period: 128, Periods: 2}, driver.ErrUnsupported},
		"no output":   {driver.Request{SampleRate: 48000, Period: 128, Periods: 2}, driver.ErrUnsupported},
		"exclusive":   {driver.Request{Output: &out, OutChannels: 2, SampleRate: 48000, Period: 128, Periods: 2, Exclusive: true}, driver.ErrUnsupported},
		"no channels": {driver.Request{Output: &out, SampleRate: 48000, Period: 128, Periods: 2}, driver.ErrFormat},
		"no rate":     {driver.Request{Output: &out, OutChannels: 2, Period: 128, Periods: 2}, driver.ErrFormat},
		"unknown id":  {driver.Request{Output: &out, OutChannels: 2, SampleRate: 48000, Period: 128, Periods: 2}, driver.ErrLost},
		"huge period": {driver.Request{Output: &out, OutChannels: 2, SampleRate: 48000, Period: 1 << 30, Periods: 2}, driver.ErrFormat},
		"huge depth":  {driver.Request{Output: &out, OutChannels: 2, SampleRate: 48000, Period: 128, Periods: 1 << 30}, driver.ErrFormat},
	} {
		s, err := b.Open(tc.req)
		if err == nil {
			_ = s.Close()
			t.Errorf("%s: Open succeeded", name)
			continue
		}
		if errors.Is(err, errNoDevice) && tc.want == driver.ErrLost {
			t.Logf("%s: no audio server here (%v)", name, err)
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: error %v, want %v", name, err, tc.want)
		}
	}
}

func TestOpenRejectsAForeignSampleRate(t *testing.T) {
	b := New()
	out, err := b.Default(dirOutput)
	if errors.Is(err, errNoDevice) {
		t.Skipf("no audio output: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(out.SampleRates) == 0 {
		t.Skip("default output reports no sample rate")
	}
	s, err := b.Open(driver.Request{Output: &out, OutChannels: 1, SampleRate: out.SampleRates[0] + 1000, Period: 256, Periods: 2})
	if err == nil {
		_ = s.Close()
		t.Fatal("Open at a rate the device is not running succeeded")
	}
	if !errors.Is(err, driver.ErrFormat) {
		t.Errorf("error %v, want ErrFormat", err)
	}
	_, err = b.Open(driver.Request{Output: &out, OutChannels: out.Outputs + 1, SampleRate: out.SampleRates[0], Period: 256, Periods: 2})
	if !errors.Is(err, driver.ErrFormat) {
		t.Errorf("more channels than the device has: error %v, want ErrFormat", err)
	}
}

// TestSmokeSine opens the default output device, plays half a second of a
// 1 kHz sine through the real stream path (Open, Start, then the Wait, Buffers
// and Commit loop the stream core runs, then Stop and Close), and checks that
// Core Audio's render callback really pulled every frame of it.
//
// It is audible: a quiet tone (-20 dBFS, with 10 ms fades) for half a second.
// A machine with no output device skips it.
func TestSmokeSine(t *testing.T) {
	// The stream core calls Wait, Buffers and Commit from one locked thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	b := New()
	out, err := b.Default(dirOutput)
	if errors.Is(err, errNoDevice) {
		t.Skipf("no audio output device: %v", err)
	}
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if out.Outputs == 0 || len(out.SampleRates) == 0 {
		t.Skipf("default output %q has no usable format: %+v", out.Name, out)
	}
	channels := min(2, out.Outputs)
	rate := out.SampleRates[0]

	ds, err := b.Open(driver.Request{Output: &out, OutChannels: channels, SampleRate: rate, Period: 256, Periods: 2})
	if errors.Is(err, errNoDevice) {
		t.Skipf("no audio output device: %v", err)
	}
	if err != nil {
		t.Fatalf("Open %q: %v", out.Name, err)
	}
	s := ds.(*stream)
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	p := s.Params()
	t.Logf("%q: %d Hz, %d channels, period %d, %d periods, %s, output latency %s",
		out.Name, p.SampleRate, p.OutChannels, p.Period, p.Periods, p.OutFormat, p.LatencyOut)
	if p.SampleRate != rate || p.OutChannels != channels || p.Period != 256 || p.Periods < 2 || p.OutFormat != "F32LE" || p.LatencyOut <= 0 {
		t.Fatalf("unexpected parameters %+v", p)
	}

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	const (
		freq      = 1000.0
		amplitude = 0.1 // -20 dBFS
	)
	sineFrames := rate / 2 // 500 ms
	fade := rate / 100     // 10 ms
	flush := int(p.LatencyOut.Seconds()*float64(rate)) + 2*p.Period
	var frame int
	var longestWait time.Duration
	for frame < sineFrames+flush {
		before := time.Now()
		if err := s.Wait(); err != nil {
			t.Fatalf("Wait at frame %d: %v", frame, err)
		}
		if d := time.Since(before); d > longestWait {
			longestWait = d
		}
		_, buf := s.Buffers()
		for f := 0; f < p.Period; f++ {
			var v float32
			if i := frame + f; i < sineFrames {
				gain := 1.0
				if i < fade {
					gain = float64(i) / float64(fade)
				} else if i >= sineFrames-fade {
					gain = float64(sineFrames-i) / float64(fade)
				}
				v = float32(amplitude * gain * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
			}
			for ch := 0; ch < channels; ch++ {
				binary.LittleEndian.PutUint32(buf[(f*channels+ch)*bytesPerSample:], math.Float32bits(v))
			}
		}
		if err := s.Commit(); err != nil {
			t.Fatalf("Commit at frame %d: %v", frame, err)
		}
		frame += p.Period
	}

	// Wait wakes when the callback has made room, so by now the callback has
	// consumed everything up to the flush: the whole tone went to Core Audio.
	calls, under, underFrames, bad := s.counters()
	tail := atomic.LoadUint64(&s.c.tail)
	t.Logf("render callbacks: %d, short: %d (%d frames), rejected: %d; consumed %d of %d frames committed; longest Wait %s",
		calls, under, underFrames, bad, tail, s.head, longestWait)
	if bad != 0 {
		t.Errorf("%d render callbacks were rejected", bad)
	}
	if want := uint64(p.Periods*p.Period + sineFrames); tail < want {
		t.Errorf("Core Audio consumed %d frames, want at least %d: the tone did not reach the device", tail, want)
	}
	if calls < 20 {
		t.Errorf("only %d render callbacks in half a second of audio", calls)
	}
	if got := s.Dropouts(); got != under {
		t.Errorf("Dropouts() = %d, want %d", got, under)
	}
	if outNano, inNano := s.Clock(); outNano <= 0 || inNano != 0 {
		t.Errorf("Clock() = %d, %d", outNano, inNano)
	}

	if err := s.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	before := calls
	time.Sleep(50 * time.Millisecond)
	if after, _, _, _ := s.counters(); after != before {
		t.Errorf("render callbacks kept running after Stop (%d -> %d)", before, after)
	}
}

// Open and Close without ever starting must release the audio unit, the ring
// and the semaphore, and so must a second Close.
func TestOpenCloseWithoutStart(t *testing.T) {
	b := New()
	out, err := b.Default(dirOutput)
	if errors.Is(err, errNoDevice) {
		t.Skipf("no audio output: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if out.Outputs == 0 || len(out.SampleRates) == 0 {
		t.Skip("default output has no usable format")
	}
	for i := 0; i < 5; i++ {
		s, err := b.Open(driver.Request{Output: &out, OutChannels: 1, SampleRate: out.SampleRates[0], Period: 512, Periods: 3})
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Errorf("Close %d: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Errorf("second Close %d: %v", i, err)
		}
	}
}

// Close must stop a stream that is still running, as the stream core's Close
// does when it interrupts a running stream, and leave the device usable.
func TestCloseStopsARunningStream(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	b := New()
	out, err := b.Default(dirOutput)
	if errors.Is(err, errNoDevice) {
		t.Skipf("no audio output: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if out.Outputs == 0 || len(out.SampleRates) == 0 {
		t.Skip("default output has no usable format")
	}
	for i := 0; i < 3; i++ {
		ds, err := b.Open(driver.Request{Output: &out, OutChannels: 1, SampleRate: out.SampleRates[0], Period: 128, Periods: 3})
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		s := ds.(*stream)
		if err := s.Start(); err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
		for p := 0; p < 8; p++ {
			if err := s.Wait(); err != nil {
				t.Fatalf("Wait %d: %v", i, err)
			}
			_, buf := s.Buffers()
			clear(buf) // silence
			if err := s.Commit(); err != nil {
				t.Fatalf("Commit %d: %v", i, err)
			}
		}
		if err := s.Close(); err != nil { // no Stop first
			t.Errorf("Close %d while running: %v", i, err)
		}
		if err := s.Stop(); err != nil {
			t.Errorf("Stop %d after Close: %v", i, err)
		}
	}
}
