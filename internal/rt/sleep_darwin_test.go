//go:build darwin

package rt

import (
	"testing"
	"time"
)

func TestDarwinSleepThreadDoesNotAllocate(t *testing.T) {
	if got := testing.AllocsPerRun(100, func() { SleepThread(time.Microsecond) }); got != 0 {
		t.Fatalf("SleepThread allocations=%g, want 0", got)
	}
}
