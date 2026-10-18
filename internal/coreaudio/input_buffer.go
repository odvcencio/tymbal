package coreaudio

import (
	"encoding/binary"
	"math"
)

// inputReader converts an independent capture clock to the render clock. The
// ring is single-producer/single-consumer and all storage is prepared at Open.
// The native producer publishes head only after copying complete frames.
const inputTaps = 128
const inputPhases = 1024

type inputReader struct {
	data                  []byte
	mask                  uint64
	channels, period      int
	inputRate, outputRate int
	phase                 float64
	origin                int64
	started               bool
	target                float64
	filteredError         float64
	ratio                 float64
	coeff                 []float32
}

func newInputReader(data []byte, channels, period, inputRate, outputRate, target int) *inputReader {
	r := &inputReader{data: data, mask: uint64(len(data)/(channels*4) - 1), channels: channels, period: period,
		inputRate: inputRate, outputRate: outputRate, target: float64(target), ratio: float64(inputRate) / float64(outputRate)}
	r.coeff = make([]float32, (inputPhases+1)*inputTaps)
	cutoff := 0.94 * math.Min(1, float64(outputRate)/float64(inputRate))
	for p := 0; p <= inputPhases; p++ {
		frac := float64(p) / inputPhases
		sum := 0.0
		for j := 0; j < inputTaps; j++ {
			x := float64(j-inputTaps/2+1) - frac
			y := math.Pi * x * cutoff
			v := cutoff
			if math.Abs(y) > 1e-12 {
				v *= math.Sin(y) / y
			}
			// Blackman-Harris window: low sidelobes, normalized per phase.
			w := float64(j) / float64(inputTaps-1) * 2 * math.Pi
			v *= 0.35875 - 0.48829*math.Cos(w) + 0.14128*math.Cos(2*w) - 0.01168*math.Cos(3*w)
			r.coeff[p*inputTaps+j] = float32(v)
			sum += v
		}
		for j := 0; j < inputTaps; j++ {
			r.coeff[p*inputTaps+j] /= float32(sum)
		}
	}
	return r
}

func (r *inputReader) reset(head uint64) {
	r.phase = float64(head)
	r.origin = int64(head)
	r.started = false
	r.filteredError = 0
}

func (r *inputReader) required() uint64 {
	return uint64(math.Ceil(r.phase+float64(r.period)*r.ratio*1.002)) + inputTaps/2 + 2
}

// read returns the oldest ring frame still needed by the next interpolation.
// An exact, undrifted equal-rate stream retains the device's float32 bits.
func (r *inputReader) read(dst []byte, head uint64) uint64 {
	if !r.started {
		r.started = true
		r.filteredError = 0
	}
	err := float64(head) - r.phase - r.target
	// A slow, bounded PLL follows separate device clocks without discontinuous
	// frame insertion/deletion. Packet-size jitter is averaged over ~2 seconds.
	alpha := math.Min(0.01, float64(r.period)/float64(r.outputRate)/2)
	r.filteredError += alpha * (err - r.filteredError)
	adjust := math.Max(-0.001, math.Min(0.001, r.filteredError/(float64(r.inputRate)*4)))
	if math.Abs(r.filteredError) < 2 {
		adjust = 0
	}
	step := r.ratio * (1 + adjust)
	for frame := 0; frame < r.period; frame++ {
		pos := int64(r.phase)
		frac := r.phase - float64(pos)
		for ch := 0; ch < r.channels; ch++ {
			var sample float32
			if r.inputRate == r.outputRate && frac < 1e-12 {
				off := (uint64(pos)&r.mask)*uint64(r.channels*4) + uint64(ch*4)
				sample = math.Float32frombits(binary.LittleEndian.Uint32(r.data[off:]))
			} else {
				fp := frac * inputPhases
				p := int(fp)
				blend := float32(fp - float64(p))
				for j := 0; j < inputTaps; j++ {
					at := pos + int64(j-inputTaps/2+1)
					if at < r.origin {
						continue
					}
					off := (uint64(at)&r.mask)*uint64(r.channels*4) + uint64(ch*4)
					v := math.Float32frombits(binary.LittleEndian.Uint32(r.data[off:]))
					c := r.coeff[p*inputTaps+j]
					c += blend * (r.coeff[(p+1)*inputTaps+j] - c)
					sample += v * c
				}
			}
			binary.LittleEndian.PutUint32(dst[(frame*r.channels+ch)*4:], math.Float32bits(sample))
		}
		r.phase += step
	}
	if r.phase < inputTaps/2 {
		return 0
	}
	return uint64(r.phase) - inputTaps/2
}
