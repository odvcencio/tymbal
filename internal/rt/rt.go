// Package rt contains portable real-time helpers and platform thread priority.
package rt

import (
	"errors"
	"runtime"
	"runtime/metrics"
	"sync/atomic"
	"time"
)

var processStart = time.Now()

var ErrUnsupported = errors.New("tymbal rt: unsupported on this build")

// Grant describes the priority level available to a stream thread.
type Grant struct {
	Kind     string
	Priority int
	restore  func()
}

func (g Grant) String() string {
	if g.Kind == "" {
		return "normal"
	}
	if g.Priority == 0 {
		return g.Kind
	}
	return g.Kind + " " + itoa(g.Priority)
}

// Raise attempts to raise priority on the calling, locked OS thread. Failure
// is nonfatal. The caller must keep the thread locked until after Lower.
func Raise(period time.Duration) Grant { return raisePriority(period) }

// Lower restores the thread's previous priority before UnlockOSThread.
func Lower(g Grant) {
	if g.restore != nil {
		g.restore()
	}
}

// PreGrowStack commits a 64 KiB frame before the callback loop starts.
func PreGrowStack() { growStack() }

//go:noinline
func growStack() {
	var frame [64 << 10]byte
	for i := range frame {
		frame[i] = byte(i)
	}
	runtime.KeepAlive(&frame)
}

// Now returns nanoseconds from a process-local monotonic clock origin.
//
//tymbal:rt
func Now() int64 { return time.Since(processStart).Nanoseconds() }

// LockProcessMemory is an opt-in engine-process tactic. M0 does not implement
// platform memory locking.
func LockProcessMemory() error { return ErrUnsupported }

// StartWatchdog counts heap allocations made anywhere in the process between
// its return and the return of stop. It passes the running total to report;
// totals only grow, so a consumer keeps the largest value it receives. The
// count is process-wide and includes the runtime's own allocations, such as
// thread creation. It does not attribute allocations to a stream.
//
// The runtime publishes small-object counts only when a P releases a cached
// span, so a plain metrics read lags recent allocations. StartWatchdog and stop
// therefore publish every P's counts with runtime.ReadMemStats before they
// read. That call stops the world briefly. Those two readings are exact. The
// sampler's readings between them never stop the world, so they are lower
// bounds.
//
// The sampler waits in SleepThread, not on a Go timer or channel, so on Linux
// and Windows, where SleepThread is a system call, the watchdog allocates
// nothing after StartWatchdog returns. stop does not wait for the sampler. The
// sampler exits within 100 ms and never reports more than stop's exact total.
//
// Call StartWatchdog after setup allocation and before the real-time loop.
// Call stop after the loop exits and before cleanup allocates.
func StartWatchdog(every time.Duration, report func(total uint64)) (stop func()) {
	_, stop = StartWatchdogWithScope(every, report, nil)
	return stop
}

// StartWatchdogWithScope counts the whole run as StartWatchdog does and also
// offers a second exact scope that can begin after backend setup and before
// the real-time loop. startScope must be called once before stop.
func StartWatchdogWithScope(every time.Duration, report, scopeReport func(total uint64)) (startScope func(), stop func()) {
	if every <= 0 {
		every = time.Second
	}
	w := &allocWatchdog{report: report, scopeReport: scopeReport, interval: every}
	w.samples[0].Name = "/gc/heap/allocs:objects"
	w.samples[1].Name = "/gc/heap/tiny/allocs:objects"
	metrics.Read(w.samples[:]) // The first read in a process builds the runtime's metric table.
	stop = w.stop
	startScope = w.startScope
	go w.sample()
	// The watchdog allocates nothing after this reading.
	w.baseline = publishedAllocs()
	w.counting.Store(true)
	return startScope, stop
}

type allocWatchdog struct {
	samples       [2]metrics.Sample // used by the sampler only, after StartWatchdog returns
	baseline      uint64            // written before counting is set
	scopeBaseline uint64            // written before scopeCounting is set
	counting      atomic.Bool
	scopeCounting atomic.Bool
	stopped       atomic.Bool
	reported      atomic.Uint64 // largest total passed to report
	scopeReported atomic.Uint64
	report        func(total uint64)
	scopeReport   func(total uint64)
	interval      time.Duration
}

// samplerSlice bounds how long the sampler outlives stop, holding a thread.
const samplerSlice = 100 * time.Millisecond

func (w *allocWatchdog) sample() {
	for {
		for slept := time.Duration(0); slept < w.interval; slept += samplerSlice {
			SleepThread(min(samplerSlice, w.interval-slept))
			if w.stopped.Load() {
				return
			}
		}
		if !w.counting.Load() {
			continue
		}
		metrics.Read(w.samples[:])
		current := w.samples[0].Value.Uint64() + w.samples[1].Value.Uint64()
		// A reading taken before stop set stopped cannot exceed stop's exact
		// reading, because counts only grow. Later readings are discarded.
		if w.stopped.Load() {
			return
		}
		w.update(current)
		if w.scopeCounting.Load() {
			w.updateScope(current)
		}
	}
}

// stop records the exact total. It is safe to call more than once.
func (w *allocWatchdog) stop() {
	if w.stopped.Swap(true) {
		return
	}
	current := publishedAllocs()
	w.update(current)
	if w.scopeCounting.Load() {
		w.updateScope(current)
	}
}

func (w *allocWatchdog) startScope() {
	if w.scopeCounting.Load() {
		return
	}
	w.scopeBaseline = publishedAllocs()
	w.scopeCounting.Store(true)
}

// update reports a larger total than any reported before.
func (w *allocWatchdog) update(current uint64) {
	if current <= w.baseline {
		return
	}
	total := current - w.baseline
	for {
		old := w.reported.Load()
		if total <= old {
			return
		}
		if w.reported.CompareAndSwap(old, total) {
			if w.report != nil {
				w.report(total)
			}
			return
		}
	}
}

func (w *allocWatchdog) updateScope(current uint64) {
	if current <= w.scopeBaseline {
		return
	}
	total := current - w.scopeBaseline
	for {
		old := w.scopeReported.Load()
		if total <= old {
			return
		}
		if w.scopeReported.CompareAndSwap(old, total) {
			if w.scopeReport != nil {
				w.scopeReport(total)
			}
			return
		}
	}
}

// publishedAllocs publishes every P's cached allocation counts, then returns
// the process-wide count of heap objects allocated so far. MemStats.Mallocs is
// the sum of /gc/heap/allocs:objects and /gc/heap/tiny/allocs:objects.
func publishedAllocs() uint64 {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.Mallocs
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
