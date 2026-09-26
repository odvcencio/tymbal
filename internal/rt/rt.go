// Package rt contains portable real-time helpers and platform thread priority.
package rt

import (
	"errors"
	"runtime"
	"runtime/metrics"
	"sync"
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
func Now() int64 { return time.Since(processStart).Nanoseconds() }

// LockProcessMemory is an opt-in engine-process tactic. M0 does not implement
// platform memory locking.
func LockProcessMemory() error { return ErrUnsupported }

// StartWatchdog samples process-wide heap allocation metrics. These counters
// are diagnostics and do not attribute allocations to a particular stream.
func StartWatchdog(every time.Duration, report func(allocs uint64)) (stop func()) {
	if every <= 0 {
		every = time.Second
	}
	stopCh := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		var samples = []metrics.Sample{{Name: "/gc/heap/allocs:objects"}, {Name: "/gc/heap/tiny/allocs:objects"}}
		var previous uint64
		metrics.Read(samples)
		previous = samples[0].Value.Uint64() + samples[1].Value.Uint64()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				metrics.Read(samples)
				current := samples[0].Value.Uint64() + samples[1].Value.Uint64()
				delta := uint64(0)
				if current >= previous {
					delta = current - previous
				}
				previous = current
				if report != nil {
					report(delta)
				}
			}
		}
	}()
	return func() {
		once.Do(func() { close(stopCh) })
		<-done
	}
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
