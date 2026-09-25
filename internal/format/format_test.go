package format

import (
	"math"
	"testing"
)

func TestIntegerEdges(t *testing.T) {
	tests := []struct {
		format Format
		input  []float32
		want   []int64
	}{
		{
			format: S16LE,
			input:  []float32{-1, 1, -2, 2, 1.0 / 65536, -1.0 / 65536, float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))},
			want:   []int64{-32768, 32767, -32768, 32767, 1, -1, 0, 32767, -32768},
		},
		{
			format: S24_3LE,
			input:  []float32{-1, 1, -1.5 / (1 << 23), 1.5 / (1 << 23), float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))},
			want:   []int64{-1 << 23, 1<<23 - 1, -2, 2, 0, 1<<23 - 1, -1 << 23},
		},
		{
			format: S32LE,
			input:  []float32{-1, 1, -2, 2, float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))},
			want:   []int64{-1 << 31, 1<<31 - 1, -1 << 31, 1<<31 - 1, 0, 1<<31 - 1, -1 << 31},
		},
	}

	for _, tt := range tests {
		t.Run(string(tt.format), func(t *testing.T) {
			bps := BytesPerSample(tt.format)
			got := make([]byte, len(tt.input)*bps)
			if err := Encode(got, tt.input, tt.format); err != nil {
				t.Fatal(err)
			}
			for i, want := range tt.want {
				if sample := unpackInteger(got[i*bps:], tt.format); sample != want {
					t.Errorf("sample %d encoded as %d, want %d", i, sample, want)
				}
			}
		})
	}
}

func TestFloat32NaNAndCopy(t *testing.T) {
	input := []float32{0, -1, 0.25, float32(math.Inf(1)), float32(math.Inf(-1)), float32(math.NaN())}
	encoded := make([]byte, len(input)*4)
	if err := Encode(encoded, input, F32LE); err != nil {
		t.Fatal(err)
	}
	got := make([]float32, len(input))
	if err := Decode(got, encoded, F32LE); err != nil {
		t.Fatal(err)
	}
	want := []float32{0, -1, 0.25, float32(math.Inf(1)), float32(math.Inf(-1)), 0}
	for i := range want {
		if math.Float32bits(got[i]) != math.Float32bits(want[i]) {
			t.Errorf("sample %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestS24SignExtension(t *testing.T) {
	tests := []struct {
		encoded [3]byte
		want    float32
	}{
		{encoded: [3]byte{0x00, 0x00, 0x80}, want: -1},
		{encoded: [3]byte{0xff, 0xff, 0x7f}, want: float32(1<<23-1) / (1 << 23)},
		{encoded: [3]byte{0xff, 0xff, 0xff}, want: -1.0 / (1 << 23)},
	}
	for _, tt := range tests {
		got := make([]float32, 1)
		if err := Decode(got, tt.encoded[:], S24_3LE); err != nil {
			t.Fatal(err)
		}
		if got[0] != tt.want {
			t.Errorf("Decode(% x) = %g, want %g", tt.encoded, got[0], tt.want)
		}
	}
}

func TestInterleaveOrderAndRoundTrip(t *testing.T) {
	planar := [][]float32{{-1, -0.25, 0.5}, {1, 0.25, -0.5}}
	encoded := make([]byte, 6*2)
	if err := EncodeInterleaved(encoded, planar, S16LE); err != nil {
		t.Fatal(err)
	}
	wantBytes := []byte{
		0x00, 0x80, 0xff, 0x7f,
		0x00, 0xe0, 0x00, 0x20,
		0x00, 0x40, 0x00, 0xc0,
	}
	if string(encoded) != string(wantBytes) {
		t.Fatalf("interleaved bytes % x, want % x", encoded, wantBytes)
	}
	decoded := [][]float32{make([]float32, 3), make([]float32, 3)}
	if err := DecodeInterleaved(decoded, encoded, S16LE); err != nil {
		t.Fatal(err)
	}
	for ch := range planar {
		for i, sample := range planar[ch] {
			if math.Abs(float64(decoded[ch][i]-sample)) > 1.0/32768 {
				t.Errorf("channel %d sample %d round trip = %g, want near %g", ch, i, decoded[ch][i], sample)
			}
		}
	}
}

func TestNonMultipleLengthsAndInvalidFormats(t *testing.T) {
	if err := Encode(make([]byte, 3), []float32{1}, S16LE); err != ErrBufferLength {
		t.Fatalf("short output: got %v, want ErrBufferLength", err)
	}
	if err := Decode(make([]float32, 1), []byte{1}, S16LE); err != ErrBufferLength {
		t.Fatalf("partial sample: got %v, want ErrBufferLength", err)
	}
	if err := Encode(make([]byte, 4), []float32{1}, Format("other")); err != ErrFormat {
		t.Fatalf("unknown format: got %v, want ErrFormat", err)
	}
	if err := EncodeInterleaved(make([]byte, 8), [][]float32{{1, 2}, {3}}, S16LE); err != ErrBufferLength {
		t.Fatalf("unequal channels: got %v, want ErrBufferLength", err)
	}
	if err := EncodeInterleaved(nil, nil, S16LE); err != ErrChannels {
		t.Fatalf("empty channels: got %v, want ErrChannels", err)
	}
}

func TestEncodeNoAlloc(t *testing.T) {
	planar := [][]float32{make([]float32, 257), make([]float32, 257)}
	dst := make([]byte, 257*2*3)
	allocs := testing.AllocsPerRun(100, func() {
		if err := EncodeInterleaved(dst, planar, S24_3LE); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("EncodeInterleaved allocated %g times per call", allocs)
	}
}

func BenchmarkEncodeInterleavedS24(b *testing.B) {
	planar := [][]float32{make([]float32, 256), make([]float32, 256), make([]float32, 256), make([]float32, 256)}
	dst := make([]byte, 256*4*3)
	b.SetBytes(int64(len(dst)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := EncodeInterleaved(dst, planar, S24_3LE); err != nil {
			b.Fatal(err)
		}
	}
}

func unpackInteger(encoded []byte, f Format) int64 {
	switch f {
	case S16LE:
		return int64(int16(uint16(encoded[0]) | uint16(encoded[1])<<8))
	case S24_3LE:
		v := uint32(encoded[0]) | uint32(encoded[1])<<8 | uint32(encoded[2])<<16
		if v&0x800000 != 0 {
			v |= 0xff000000
		}
		return int64(int32(v))
	case S32LE:
		v := uint32(encoded[0]) | uint32(encoded[1])<<8 | uint32(encoded[2])<<16 | uint32(encoded[3])<<24
		return int64(int32(v))
	default:
		return 0
	}
}
