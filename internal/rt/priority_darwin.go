//go:build darwin && (amd64 || arm64)

package rt

import (
	"math"
	"sync"
	"time"
	"unsafe"
)

// Flavors and counts from <mach/thread_policy.h>. A count is in 32-bit integers.
const (
	threadStandardPolicy       = 1 // THREAD_STANDARD_POLICY: the ordinary timesharing scheduler
	threadStandardPolicyCount  = 0
	threadTimeConstraintPolicy = 2 // THREAD_TIME_CONSTRAINT_POLICY
	threadTimeConstraintCount  = 4 // period, computation, constraint, preemptible
)

// The kernel accepts a computation of 50 us to 50 ms and refuses the whole
// policy outside that range (osfmk/kern/sched_prim.c, min_rt_quantum and
// max_rt_quantum). timeConstraintFor keeps the policy inside it, so an extreme
// period gets a clamped grant rather than none.
const (
	minComputation = 50 * time.Microsecond
	maxComputation = 50 * time.Millisecond

	// maxPeriod bounds the tick arithmetic. No audio period comes near it.
	maxPeriod = time.Second
)

// timeConstraintFor is the real-time policy for a thread that wakes once every
// period, renders, and sleeps. numer and denom are the mach_timebase_info ratio
// that converts the policy's tick units to nanoseconds. It reports false when
// the inputs describe no usable policy.
//
// The values are the ones in the design spec (implementation.md, "The Go
// real-time thread"), which are also what Firefox's audio_thread_priority uses:
//
//   - period and constraint are one period: the work for a period should be
//     done before the next one starts.
//   - computation is half a period. <mach/thread_policy.h> forces anything
//     below constraint/2 up to constraint/2 anyway.
//   - preemptible is set. Core Audio's own IO thread is real-time too, and it
//     has to be able to run when the callback it delivers is due.
func timeConstraintFor(period time.Duration, numer, denom uint32) (timeConstraint, bool) {
	if period <= 0 || numer == 0 || denom == 0 {
		return timeConstraint{}, false
	}
	ticks := func(d time.Duration) uint64 { return uint64(d) * uint64(denom) / uint64(numer) }
	p := ticks(min(period, maxPeriod))
	if p == 0 {
		return timeConstraint{}, false // less than one tick of the clock
	}
	// One tick inside each bound, in case the kernel rounded its own limits
	// the other way when it converted them from time to ticks.
	computation := min(max(p/2, ticks(minComputation)+1), ticks(maxComputation)-1)
	constraint := max(p, computation) // the kernel needs constraint >= computation
	p = max(p, constraint)            // and a period no shorter than that
	clamp := func(v uint64) uint32 { return uint32(min(v, math.MaxUint32)) }
	return timeConstraint{
		period:      clamp(p),
		computation: clamp(computation),
		constraint:  clamp(constraint),
		preemptible: 1,
	}, true
}

// machAPI is the part of Mach the priority code uses. Production calls the
// kernel. Tests pass a stand-in by value, so none of them changes a real
// thread's scheduling unless it means to.
type machAPI interface {
	// timebase is the mach_timebase_info ratio, or 0, 0 if it is unavailable.
	timebase() (numer, denom uint32)
	// thisThread names the calling OS thread: an identity that differs
	// between threads, and the Mach port the kernel's thread calls take. The
	// port is 0 if it cannot be had.
	thisThread() (id uintptr, port uint32)
	// realtime reports whether the thread has a time-constraint policy. known
	// is false when the kernel could not say.
	realtime(port uint32) (isRT, known bool)
	// setRealtime applies p to the thread, and setStandard returns it to the
	// timesharing scheduler. Both report whether the kernel agreed.
	setRealtime(port uint32, p timeConstraint) bool
	setStandard(port uint32) bool
}

type machKernel struct{}

var (
	machTimebaseOnce sync.Once
	cachedTimebase   machTimebase // zero if mach_timebase_info failed; fixed until reboot
)

func (machKernel) timebase() (numer, denom uint32) {
	machTimebaseOnce.Do(func() {
		var tb machTimebase
		if machTimebaseInfo(&tb) == 0 {
			cachedTimebase = tb
		}
	})
	return cachedTimebase.numer, cachedTimebase.denom
}

func (machKernel) thisThread() (uintptr, uint32) {
	self := pthreadSelf()
	return self, pthreadMachThread(self)
}

func (machKernel) realtime(port uint32) (isRT, known bool) {
	var p timeConstraint
	count := uint32(threadTimeConstraintCount)
	var isDefault int32 // false on entry: ask for the thread's own policy, not the default
	if threadPolicyGet(port, threadTimeConstraintPolicy, unsafe.Pointer(&p), &count, &isDefault) != 0 {
		return false, false
	}
	// The kernel sets isDefault when the thread is not in real-time mode, and
	// then fills p with what a time-sharing thread would get.
	return isDefault == 0, true
}

func (machKernel) setRealtime(port uint32, p timeConstraint) bool {
	return threadPolicySet(port, threadTimeConstraintPolicy, unsafe.Pointer(&p), threadTimeConstraintCount) == 0
}

func (machKernel) setStandard(port uint32) bool {
	var none [1]int32 // THREAD_STANDARD_POLICY carries no data; the pointer is never read
	return threadPolicySet(port, threadStandardPolicy, unsafe.Pointer(&none[0]), threadStandardPolicyCount) == 0
}

// raisePriority gives the calling thread a time-constraint policy shaped by the
// stream's period. That moves it into the kernel's real-time scheduling band,
// above every timesharing thread, so it runs when Core Audio's callback signals
// it instead of queueing behind ordinary work.
//
// It must run on the locked stream thread, and so must Lower. Failure is never
// fatal: the grant says "normal" and the thread keeps the priority it had.
func raisePriority(period time.Duration) Grant { return raiseDarwinPriority(machKernel{}, period) }

func raiseDarwinPriority(api machAPI, period time.Duration) Grant {
	normal := Grant{Kind: "normal"}
	numer, denom := api.timebase()
	policy, ok := timeConstraintFor(period, numer, denom)
	if !ok {
		return normal
	}
	id, port := api.thisThread()
	if port == 0 {
		return normal
	}
	// A thread that is already real-time belongs to whoever made it so. Leave
	// its policy alone, and do not undo it in Lower. If the kernel cannot say,
	// carry on: setting the policy below reports the real outcome.
	if isRT, known := api.realtime(port); known && isRT {
		return Grant{Kind: "time-constraint"}
	}
	if !api.setRealtime(port, policy) {
		return normal
	}
	var once sync.Once
	return Grant{Kind: "time-constraint", restore: func() {
		// The port names the raised thread, so the call would reach it from
		// anywhere. But pthread_mach_thread_np holds no right of its own, and
		// a port name can be reused once its thread exits. Lower is contracted
		// to run on the raised thread, and a caller that strays gets nothing.
		if current, _ := api.thisThread(); current == id {
			once.Do(func() { api.setStandard(port) })
		}
	}}
}
