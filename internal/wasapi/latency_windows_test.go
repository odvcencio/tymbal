//go:build windows

package wasapi

import (
	"errors"
	"math"
	"testing"
	"time"

	"m31labs.dev/tymbal/internal/driver"
)

func TestStreamLatencyFromHNS(t *testing.T) {
	for _, test := range []struct {
		name string
		hns  int64
		want time.Duration
	}{
		{name: "zero", hns: 0, want: 0},
		{name: "one unit", hns: 1, want: 100 * time.Nanosecond},
		{name: "typical", hns: 225_000, want: 22_500_000 * time.Nanosecond},
		{name: "largest representable", hns: math.MaxInt64 / nanosecondsPerHNS, want: time.Duration((math.MaxInt64 / nanosecondsPerHNS) * nanosecondsPerHNS)},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := streamLatencyFromHNS(test.hns)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("streamLatencyFromHNS(%d) = %v, want %v", test.hns, got, test.want)
			}
		})
	}
}

func TestStreamLatencyFromHNSRejectsMalformedValues(t *testing.T) {
	for _, hns := range []int64{-1, math.MaxInt64/nanosecondsPerHNS + 1} {
		if _, err := streamLatencyFromHNS(hns); !errors.Is(err, driver.ErrFormat) {
			t.Errorf("streamLatencyFromHNS(%d) error = %v, want ErrFormat", hns, err)
		}
	}
}
