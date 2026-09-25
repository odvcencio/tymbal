// Package format converts planar float32 audio to and from interleaved device
// sample formats.
package format

import (
	"errors"
	"math"
)

// Format is a little-endian PCM sample format.
type Format string

const (
	F32LE   Format = "F32LE"
	S32LE   Format = "S32LE"
	S24_3LE Format = "S24_3LE"
	S16LE   Format = "S16LE"
)

var (
	ErrFormat       = errors.New("format: unsupported sample format")
	ErrBufferLength = errors.New("format: incompatible buffer length")
	ErrChannels     = errors.New("format: at least one channel is required")
)

// BytesPerSample returns the encoded width of f, or zero for an unknown format.
func BytesPerSample(f Format) int {
	switch f {
	case F32LE, S32LE:
		return 4
	case S24_3LE:
		return 3
	case S16LE:
		return 2
	default:
		return 0
	}
}

// Encode converts one channel of samples into little-endian device bytes.
// dst must have exactly len(src)*BytesPerSample(f) bytes.
func Encode(dst []byte, src []float32, f Format) error {
	bps := BytesPerSample(f)
	if bps == 0 {
		return ErrFormat
	}
	if len(src) > int(^uint(0)>>1)/bps || len(dst) != len(src)*bps {
		return ErrBufferLength
	}
	for i, x := range src {
		encodeSample(dst, i*bps, x, f)
	}
	return nil
}

// Decode converts one channel of little-endian device bytes into float32
// samples. Integer formats are normalized by their negative full-scale value.
// dst must have exactly len(src)/BytesPerSample(f) samples.
func Decode(dst []float32, src []byte, f Format) error {
	bps := BytesPerSample(f)
	if bps == 0 {
		return ErrFormat
	}
	if len(src)%bps != 0 || len(dst) != len(src)/bps {
		return ErrBufferLength
	}
	for i := range dst {
		dst[i] = decodeSample(src, i*bps, f)
	}
	return nil
}

// EncodeInterleaved converts planar channels to interleaved device bytes.
// Every channel must have the same frame count, and dst must have exactly the
// size required by the channel and frame counts.
func EncodeInterleaved(dst []byte, planar [][]float32, f Format) error {
	bps := BytesPerSample(f)
	if bps == 0 {
		return ErrFormat
	}
	if len(planar) == 0 {
		return ErrChannels
	}
	frames := len(planar[0])
	for ch := 1; ch < len(planar); ch++ {
		if len(planar[ch]) != frames {
			return ErrBufferLength
		}
	}
	if frames > int(^uint(0)>>1)/len(planar) {
		return ErrBufferLength
	}
	values := frames * len(planar)
	if values > int(^uint(0)>>1)/bps || len(dst) != values*bps {
		return ErrBufferLength
	}
	for frame := 0; frame < frames; frame++ {
		for ch := range planar {
			encodeSample(dst, (frame*len(planar)+ch)*bps, planar[ch][frame], f)
		}
	}
	return nil
}

// DecodeInterleaved de-interleaves device bytes into planar channels. Every
// destination channel must have the same frame count, and src must have
// exactly the required byte length.
func DecodeInterleaved(planar [][]float32, src []byte, f Format) error {
	bps := BytesPerSample(f)
	if bps == 0 {
		return ErrFormat
	}
	if len(planar) == 0 {
		return ErrChannels
	}
	frames := len(planar[0])
	for ch := 1; ch < len(planar); ch++ {
		if len(planar[ch]) != frames {
			return ErrBufferLength
		}
	}
	if frames > int(^uint(0)>>1)/len(planar) {
		return ErrBufferLength
	}
	values := frames * len(planar)
	if values > int(^uint(0)>>1)/bps || len(src) != values*bps {
		return ErrBufferLength
	}
	for frame := 0; frame < frames; frame++ {
		for ch := range planar {
			planar[ch][frame] = decodeSample(src, (frame*len(planar)+ch)*bps, f)
		}
	}
	return nil
}

func encodeSample(dst []byte, off int, x float32, f Format) {
	switch f {
	case F32LE:
		if math.IsNaN(float64(x)) {
			x = 0
		}
		v := math.Float32bits(x)
		dst[off] = byte(v)
		dst[off+1] = byte(v >> 8)
		dst[off+2] = byte(v >> 16)
		dst[off+3] = byte(v >> 24)
	case S16LE:
		v := int16(integerSample(x, 1<<15, -1<<15, 1<<15-1))
		u := uint16(v)
		dst[off] = byte(u)
		dst[off+1] = byte(u >> 8)
	case S24_3LE:
		v := uint32(int32(integerSample(x, 1<<23, -1<<23, 1<<23-1)))
		dst[off] = byte(v)
		dst[off+1] = byte(v >> 8)
		dst[off+2] = byte(v >> 16)
	case S32LE:
		v := uint32(int32(integerSample(x, 1<<31, -1<<31, 1<<31-1)))
		dst[off] = byte(v)
		dst[off+1] = byte(v >> 8)
		dst[off+2] = byte(v >> 16)
		dst[off+3] = byte(v >> 24)
	}
}

func integerSample(x float32, scale, min, max float64) int64 {
	v := float64(x)
	if math.IsNaN(v) {
		return 0
	}
	if v > 1 {
		v = 1
	} else if v < -1 {
		v = -1
	}
	v = math.Round(v * scale)
	if v < min {
		return int64(min)
	}
	if v > max {
		return int64(max)
	}
	return int64(v)
}

func decodeSample(src []byte, off int, f Format) float32 {
	switch f {
	case F32LE:
		v := uint32(src[off]) | uint32(src[off+1])<<8 | uint32(src[off+2])<<16 | uint32(src[off+3])<<24
		return math.Float32frombits(v)
	case S16LE:
		v := int16(uint16(src[off]) | uint16(src[off+1])<<8)
		return float32(v) / (1 << 15)
	case S24_3LE:
		v := uint32(src[off]) | uint32(src[off+1])<<8 | uint32(src[off+2])<<16
		if v&0x800000 != 0 {
			v |= 0xff000000
		}
		return float32(int32(v)) / (1 << 23)
	case S32LE:
		v := uint32(src[off]) | uint32(src[off+1])<<8 | uint32(src[off+2])<<16 | uint32(src[off+3])<<24
		return float32(int32(v)) / (1 << 31)
	default:
		return 0 // Callers validate the format before entering the sample loop.
	}
}
