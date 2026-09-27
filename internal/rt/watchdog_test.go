package rt

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

var watchdogSink *[4]uint64

// maxTotal keeps the largest total a watchdog reports.
type maxTotal struct{ v atomic.Uint64 }

func (m *maxTotal) report(total uint64) {
	for {
		old := m.v.Load()
		if total <= old || m.v.CompareAndSwap(old, total) {
			return
		}
	}
}

// Stop must take a final reading. Without one, a stream that stops before the
// first tick reports none of its allocations.
func TestWatchdogStopCountsAllocationsBeforeFirstTick(t *testing.T) {
	var total maxTotal
	stop := StartWatchdog(time.Hour, total.report)
	const allocations = 1000
	for i := 0; i < allocations; i++ {
		watchdogSink = new([4]uint64)
	}
	stop()
	if got := total.v.Load(); got < allocations {
		t.Fatalf("watchdog reported %d allocations after stop, want at least %d", got, allocations)
	}
}

// The runtime publishes small-object counts only when a P releases a cached
// span. Objects allocated just before StartWatchdog stay unpublished in their
// span; allocations after it refill that span and publish them. They must not
// be counted.
func TestWatchdogExcludesAllocationsMadeBeforeStart(t *testing.T) {
	runtime.GC()
	const before, during = 100, 1000
	for i := 0; i < before; i++ {
		watchdogSink = new([4]uint64)
	}
	var total maxTotal
	stop := StartWatchdog(time.Millisecond, total.report)
	for i := 0; i < during; i++ {
		watchdogSink = new([4]uint64)
	}
	SleepThread(20 * time.Millisecond)
	stop()
	// The slack covers incidental runtime allocations, such as a new thread.
	if got := total.v.Load(); got < during || got >= during+before/2 {
		t.Fatalf("watchdog reported %d allocations, want about the %d made after it started", got, during)
	}
}

// After stop reports the exact total, a sampler reading never raises it.
func TestWatchdogSamplerNeverReportsAfterStop(t *testing.T) {
	var total maxTotal
	stop := StartWatchdog(time.Millisecond, total.report)
	stop()
	atStop := total.v.Load()
	for i := 0; i < 1000; i++ {
		watchdogSink = new([4]uint64)
	}
	runtime.GC()
	SleepThread(20 * time.Millisecond)
	if got := total.v.Load(); got != atStop {
		t.Fatalf("total changed from %d to %d after stop", atStop, got)
	}
}
