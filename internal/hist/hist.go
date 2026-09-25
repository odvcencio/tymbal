// Package hist records durations in a fixed-size, logarithmic histogram.
package hist

import (
	"math"
	"math/bits"
	"sync/atomic"
	"time"
)

// Histogram buckets durations by powers of two in microseconds. Bucket 0 is
// below 1 µs; bucket i covers [2^(i-1), 2^i) µs for i in 1..31. Values beyond
// the last boundary remain in bucket 31. Buckets are public for stable stats
// snapshots; concurrent access must use Record, Percentile, or Snapshot.
type Histogram struct{ Buckets [32]uint64 }

// Record adds one observation. Negative and sub-microsecond durations use
// bucket 0. Counters saturate at MaxUint64 instead of wrapping.
func (h *Histogram) Record(d time.Duration) {
	bucket := 0
	if d >= time.Microsecond {
		// The integer division preserves the half-open bucket boundaries: an
		// exact power-of-two number of microseconds moves into the next bucket.
		bucket = bits.Len64(uint64(d) / uint64(time.Microsecond))
		if bucket >= len(h.Buckets) {
			bucket = len(h.Buckets) - 1
		}
	}
	saturatingIncrement(&h.Buckets[bucket])
}

// Percentile returns the upper bound of the bucket containing p. p is a
// fraction in [0,1]; values below 0 and above 1 clamp to the nearest endpoint.
// An empty histogram or NaN percentile returns zero. For p<=0, the first
// non-empty bucket is returned.
func (h *Histogram) Percentile(p float64) time.Duration {
	if math.IsNaN(p) {
		return 0
	}
	var buckets [32]uint64
	var total uint64
	for i := range buckets {
		v := atomic.LoadUint64(&h.Buckets[i])
		buckets[i] = v
		if math.MaxUint64-total < v {
			total = math.MaxUint64
		} else {
			total += v
		}
	}
	if total == 0 {
		return 0
	}
	if p >= 1 {
		for i := len(buckets) - 1; i >= 0; i-- {
			if buckets[i] != 0 {
				return bucketUpperBound(i)
			}
		}
		return 0
	}
	var target uint64
	if p <= 0 {
		target = 1
	} else {
		target = uint64(math.Ceil(float64(total) * p))
		if target == 0 {
			target = 1
		}
	}
	var cumulative uint64
	for i, v := range buckets {
		if math.MaxUint64-cumulative < v {
			cumulative = math.MaxUint64
		} else {
			cumulative += v
		}
		if cumulative >= target {
			return bucketUpperBound(i)
		}
	}
	return bucketUpperBound(len(h.Buckets) - 1)
}

// Snapshot returns a race-safe copy of all bucket counters. A snapshot is not
// transactional: concurrent Record calls may be reflected in only some bins.
func (h *Histogram) Snapshot() Histogram {
	var out Histogram
	for i := range h.Buckets {
		out.Buckets[i] = atomic.LoadUint64(&h.Buckets[i])
	}
	return out
}

func bucketUpperBound(i int) time.Duration {
	return time.Duration(uint64(1)<<uint(i)) * time.Microsecond
}

func saturatingIncrement(p *uint64) {
	old := atomic.LoadUint64(p)
	for old != math.MaxUint64 {
		if atomic.CompareAndSwapUint64(p, old, old+1) {
			return
		}
		old = atomic.LoadUint64(p)
	}
}
