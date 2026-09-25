// Package tymbaltest contains portable conformance tools for Tymbal hosts.
package tymbaltest

import (
	"math"

	"m31labs.dev/tymbal"
)

const (
	testToneHz       = 997.0
	testToneDB       = -6.0
	continuityWindow = 256
	continuityHop    = 64
)

// Sine is a phase-continuous signal generator. Its Render method is suitable
// for a stream callback after construction; it does not allocate.
type Sine struct {
	Rate      int
	Frequency float64
	Amplitude float64
	Phase     float64
}

// NewSine returns a sine generator with the requested frequency and level.
func NewSine(rate int, frequency, amplitude float64) Sine {
	return Sine{Rate: rate, Frequency: frequency, Amplitude: amplitude}
}

// Render fills every output channel with one phase-continuous sine wave.
func (s *Sine) Render(_ tymbal.Time, _ [][]float32, out [][]float32) {
	if s == nil || s.Rate <= 0 || s.Frequency <= 0 {
		return
	}
	step := 2 * math.Pi * s.Frequency / float64(s.Rate)
	phase := s.Phase
	for frame := 0; frame < len(outChannel(out)); frame++ {
		v := float32(s.Amplitude * math.Sin(phase))
		for ch := range out {
			if frame < len(out[ch]) {
				out[ch][frame] = v
			}
		}
		phase += step
		if phase >= 2*math.Pi {
			phase -= 2 * math.Pi
		}
	}
	s.Phase = phase
}

// Callback adapts Render to tymbal.Callback.
func (s *Sine) Callback(t tymbal.Time, in, out [][]float32) { s.Render(t, in, out) }

// ToneCallback returns a callback that renders a -6 dBFS 997 Hz sine.
func ToneCallback(rate int) tymbal.Callback {
	g := NewSine(rate, testToneHz, math.Pow(10, testToneDB/20))
	return g.Callback
}

// MLS returns one period of a maximum-length sequence for x^15+x^14+1.
// Samples are normalized to the requested peak amplitude.
func MLS(amplitude float32) []float32 {
	const length = (1 << 15) - 1
	seq := make([]float32, length)
	state := uint16(0x7fff)
	for i := range seq {
		if state&1 != 0 {
			seq[i] = amplitude
		} else {
			seq[i] = -amplitude
		}
		feedback := (state ^ (state >> 1)) & 1
		state = (state >> 1) | (feedback << 14)
	}
	return seq
}

// Break marks a detected discontinuity at the start of a capture window.
type Break struct {
	Frame uint64 `json:"frame"`
}

// DetectContinuity checks a captured -6 dBFS 997 Hz sine for phase jumps and
// amplitude losses exceeding 6 dB. Adjacent findings within one window merge.
func DetectContinuity(samples []float32, rate int) []Break {
	return DetectContinuityAt(samples, rate, testToneHz)
}

// DetectContinuityAt is DetectContinuity with an explicit signal frequency.
func DetectContinuityAt(samples []float32, rate int, frequency float64) []Break {
	if rate <= 0 || frequency <= 0 || len(samples) < continuityWindow {
		return nil
	}
	nWindows := (len(samples)-continuityWindow)/continuityHop + 1
	cosTable := make([]float64, continuityWindow)
	sinTable := make([]float64, continuityWindow)
	omega := 2 * math.Pi * frequency / float64(rate)
	for i := range cosTable {
		angle := omega * float64(i)
		cosTable[i], sinTable[i] = math.Cos(angle), math.Sin(angle)
	}
	phases := make([]float64, nWindows)
	amplitudes := make([]float64, nWindows)
	maxAmp := 0.0
	for w := 0; w < nWindows; w++ {
		start := w * continuityHop
		re, im := 0.0, 0.0
		for i := 0; i < continuityWindow; i++ {
			x := float64(samples[start+i])
			re += x * cosTable[i]
			im -= x * sinTable[i]
		}
		phases[w] = math.Atan2(im, re)
		amplitudes[w] = 2 * math.Hypot(re, im) / continuityWindow
		if amplitudes[w] > maxAmp {
			maxAmp = amplitudes[w]
		}
	}
	if maxAmp < 1e-6 {
		return nil
	}
	// Ignore startup windows until the capture has reached a stable, useful
	// signal level. This prevents initial loopback silence from being called a
	// break while retaining later level changes as findings.
	activeFloor := maxAmp * 0.5
	levelFloor := maxAmp * math.Pow(10, -6.0/20.0)
	expected := omega * continuityHop
	findings := make([]Break, 0, 4)
	active := false
	prevPhase := 0.0
	for w := range phases {
		amp := amplitudes[w]
		if !active {
			if amp >= activeFloor {
				active, prevPhase = true, phases[w]
			}
			continue
		}
		bad := amp < levelFloor
		if amp >= activeFloor {
			delta := wrapPhase(phases[w] - prevPhase - expected)
			if math.Abs(delta) > 0.1 {
				bad = true
			}
		}
		if bad {
			frame := uint64(w * continuityHop)
			if len(findings) == 0 || frame-findings[len(findings)-1].Frame > continuityWindow {
				findings = append(findings, Break{Frame: frame})
			}
		}
		prevPhase = phases[w]
	}
	return findings
}

func wrapPhase(x float64) float64 {
	for x > math.Pi {
		x -= 2 * math.Pi
	}
	for x < -math.Pi {
		x += 2 * math.Pi
	}
	return x
}

// MeasureLatencyFrames finds the positive delay of probe within capture. The
// probeStart argument gives the probe's output frame and maxDelay bounds the
// search. Linear correlation uses a radix-2 FFT with sufficient zero padding.
func MeasureLatencyFrames(capture, probe []float32, probeStart, maxDelay int) (int, error) {
	if len(probe) == 0 || probeStart < 0 || maxDelay < 0 || probeStart+len(probe) > len(capture) {
		return 0, errInvalidCorrelation
	}
	need := len(capture) + len(probe) - 1
	n := 1
	for n < need {
		n <<= 1
	}
	x := make([]complex128, n)
	p := make([]complex128, n)
	for i, v := range capture {
		x[i] = complex(float64(v), 0)
	}
	for i, v := range probe {
		p[i] = complex(float64(v), 0)
	}
	fft(x, false)
	fft(p, false)
	for i := range x {
		x[i] *= cmplxConj(p[i])
	}
	fft(x, true)
	lo := probeStart
	hi := probeStart + maxDelay
	if hi >= len(capture) {
		hi = len(capture) - 1
	}
	peakIndex := -1
	peak := -1.0
	for i := lo; i <= hi; i++ {
		v := math.Abs(real(x[i]))
		if v > peak {
			peak, peakIndex = v, i
		}
	}
	if peakIndex < 0 || peak <= 0 {
		return 0, errNoCorrelation
	}
	return peakIndex - probeStart, nil
}

var (
	errInvalidCorrelation = simpleError("tymbaltest: invalid correlation input")
	errNoCorrelation      = simpleError("tymbaltest: no correlation peak found")
)

type errorString string

func (e errorString) Error() string { return string(e) }
func simpleError(s string) error    { return errorString(s) }

func cmplxConj(z complex128) complex128 { return complex(real(z), -imag(z)) }

func fft(a []complex128, inverse bool) {
	n := len(a)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		angle := 2 * math.Pi / float64(size)
		if !inverse {
			angle = -angle
		}
		root := complex(math.Cos(angle), math.Sin(angle))
		half := size >> 1
		for base := 0; base < n; base += size {
			w := complex(1, 0)
			for i := 0; i < half; i++ {
				even := a[base+i]
				odd := a[base+i+half] * w
				a[base+i], a[base+i+half] = even+odd, even-odd
				w *= root
			}
		}
	}
	if inverse {
		scale := complex(float64(n), 0)
		for i := range a {
			a[i] /= scale
		}
	}
}

// outChannel gives Render the number of frames available in the first channel.
func outChannel(out [][]float32) []float32 {
	if len(out) == 0 {
		return nil
	}
	return out[0]
}
