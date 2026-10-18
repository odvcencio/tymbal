package coreaudio

import (
	"encoding/binary"
	"math"
	"testing"
)

func sineRing(frames, channels, rate int, hz float64) []byte {
	data := make([]byte, frames*channels*4)
	for f := 0; f < frames; f++ {
		for ch := 0; ch < channels; ch++ {
			v := float32(math.Sin(2*math.Pi*hz*float64(f)/float64(rate))) * float32(ch+1) / float32(channels)
			binary.LittleEndian.PutUint32(data[(f*channels+ch)*4:], math.Float32bits(v))
		}
	}
	return data
}

func TestInputEqualRatePreservesPCMAndNoCallbackAllocations(t *testing.T) {
	data := sineRing(4096, 2, 48000, 750)
	r := newInputReader(data, 2, 64, 48000, 48000, 256)
	out := make([]byte, 64*2*4)
	for b := 0; b < 80; b++ {
		from := int(r.phase)
		r.read(out, uint64(from+256))
		for i := 0; i < len(out); i++ {
			if out[i] != data[(from*8+i)%len(data)] {
				t.Fatalf("PCM changed at block%d byte%d", b, i)
			}
		}
	}
	allocs := testing.AllocsPerRun(1000, func() { r.read(out, uint64(r.phase)+256) })
	if allocs != 0 {
		t.Fatalf("input callback allocations=%g", allocs)
	}
}

func TestInputSampleRateConversionQuality(t *testing.T) {
	for _, tc := range []struct {
		in, out int
		hz      float64
		reject  bool
	}{
		{44100, 48000, 1000, false}, {48000, 44100, 1000, false}, {16000, 48000, 1000, false},
		{192000, 48000, 32000, true}, {96000, 48000, 36000, true},
	} {
		data := sineRing(1<<18, 1, tc.in, tc.hz)
		r := newInputReader(data, 1, 64, tc.in, tc.out, 512)
		r.phase = 1024
		buf := make([]byte, 64*4)
		peak := 0.0
		maxError := 0.0
		for b := 0; b < 80; b++ {
			phase := r.phase
			r.read(buf, uint64(phase)+512)
			for f := 0; f < 64; f++ {
				v := float64(math.Float32frombits(binary.LittleEndian.Uint32(buf[f*4:])))
				peak = math.Max(peak, math.Abs(v))
				want := math.Sin(2 * math.Pi * tc.hz * (phase + float64(f)*r.ratio) / float64(tc.in))
				maxError = math.Max(maxError, math.Abs(v-want))
			}
		}
		if tc.reject {
			if peak > 0.0001 {
				t.Errorf("%d→%d %gHz alias peak%g exceeds -80dBFS", tc.in, tc.out, tc.hz, peak)
			}
		} else if maxError > 0.001 {
			t.Errorf("%d→%d signal error%g", tc.in, tc.out, maxError)
		}
	}
}

func TestInputIndependentClockDriftStaysBounded(t *testing.T) {
	for _, ppm := range []float64{-150, 150} {
		r := newInputReader(sineRing(4096, 1, 48000, 750), 1, 128, 48000, 48000, 512)
		out := make([]byte, 128*4)
		for b := 0; b < 4000; b++ {
			head := uint64(512 + float64(b*128)*(1+ppm/1e6))
			if head < r.required() {
				t.Fatalf("%gppm input starved at block%d", ppm, b)
			}
			r.read(out, head)
			fill := float64(head) - r.phase
			if fill < 128 || fill > 1024 {
				t.Fatalf("%gppm unbounded clock queue%g at%d", ppm, fill, b)
			}
		}
	}
}
