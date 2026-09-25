package hist

import (
	"math"
	"sync"
	"testing"
	"time"
)

func TestRecordBoundaries(t *testing.T) {
	var h Histogram
	for _, d := range []time.Duration{-time.Second, -1, 0, 999 * time.Nanosecond} {
		h.Record(d)
	}
	for _, d := range []time.Duration{time.Microsecond, 1999 * time.Nanosecond} {
		h.Record(d)
	}
	h.Record(2 * time.Microsecond)
	h.Record(3 * time.Microsecond)
	h.Record(4 * time.Microsecond)
	h.Record((1 << 31) * time.Microsecond)

	want := [32]uint64{4, 2, 2, 1}
	want[31] = 1
	got := h.Snapshot()
	if got.Buckets != want {
		t.Fatalf("buckets = %v, want %v", got.Buckets, want)
	}
}

func TestPercentileBounds(t *testing.T) {
	var empty Histogram
	for _, p := range []float64{0, 0.5, 1, math.NaN()} {
		if got := empty.Percentile(p); got != 0 {
			t.Errorf("empty.Percentile(%v) = %v, want 0", p, got)
		}
	}

	var h Histogram
	for i := 0; i < 7; i++ {
		h.Record(100 * time.Nanosecond)
	}
	h.Record(time.Microsecond)
	h.Record(2 * time.Microsecond)
	tests := []struct {
		p    float64
		want time.Duration
	}{
		{p: -1, want: time.Microsecond},
		{p: 0, want: time.Microsecond},
		{p: 0.5, want: time.Microsecond},
		{p: 0.8, want: 2 * time.Microsecond},
		{p: 1, want: 4 * time.Microsecond},
		{p: 2, want: 4 * time.Microsecond},
	}
	for _, tt := range tests {
		if got := h.Percentile(tt.p); got != tt.want {
			t.Errorf("Percentile(%v) = %v, want %v", tt.p, got, tt.want)
		}
	}
}

func TestCountersSaturate(t *testing.T) {
	var h Histogram
	h.Buckets[0] = math.MaxUint64
	h.Record(0)
	if got := h.Snapshot().Buckets[0]; got != math.MaxUint64 {
		t.Fatalf("saturated bucket = %d, want %d", got, uint64(math.MaxUint64))
	}
	h.Buckets[3] = 1
	if got := h.Percentile(1); got != 8*time.Microsecond {
		t.Fatalf("maximum percentile after total saturation = %v, want 8µs", got)
	}
}

func TestConcurrentRecordAndRead(t *testing.T) {
	var h Histogram
	const writers = 8
	const records = 2000
	var wg sync.WaitGroup
	wg.Add(writers)
	for range writers {
		go func() {
			defer wg.Done()
			for i := 0; i < records; i++ {
				h.Record(time.Duration(i%64+1) * time.Microsecond)
			}
		}()
	}
	for i := 0; i < 100; i++ {
		_ = h.Percentile(0.99)
		_ = h.Snapshot()
	}
	wg.Wait()
	var total uint64
	for _, count := range h.Snapshot().Buckets {
		total += count
	}
	if total != writers*records {
		t.Fatalf("recorded %d observations, want %d", total, writers*records)
	}
}

func TestRecordAndReadNoAlloc(t *testing.T) {
	var h Histogram
	allocs := testing.AllocsPerRun(100, func() {
		h.Record(123 * time.Microsecond)
		_ = h.Percentile(0.99)
		_ = h.Snapshot()
	})
	if allocs != 0 {
		t.Fatalf("histogram operations allocated %g times per call", allocs)
	}
}
