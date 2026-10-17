//go:build darwin && (amd64 || arm64)

package rt

import (
	"math"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// The two timebases macOS has: Intel runs the clock in nanoseconds, Apple
// silicon in 24 MHz ticks of 125/3 ns.
var timebases = []struct {
	name         string
	numer, denom uint32
}{
	{"intel 1/1", 1, 1},
	{"apple silicon 125/3", 125, 3},
}

// framesToDuration is a render period of frames at rate, as Stream.run works it out.
func framesToDuration(frames, rate int) time.Duration {
	return time.Duration(int64(frames) * int64(time.Second) / int64(rate))
}

func TestTimeConstraintForTheDesignSpecShape(t *testing.T) {
	// 128 frames at 44.1 kHz, the period that glitched at normal priority.
	period := framesToDuration(128, 44100) // 2902494 ns
	for _, tc := range []struct {
		name         string
		numer, denom uint32
		want         timeConstraint
	}{
		{"intel", 1, 1, timeConstraint{period: 2902494, computation: 1451247, constraint: 2902494, preemptible: 1}},
		{"apple silicon", 125, 3, timeConstraint{period: 69659, computation: 34829, constraint: 69659, preemptible: 1}},
	} {
		got, ok := timeConstraintFor(period, tc.numer, tc.denom)
		if !ok || got != tc.want {
			t.Errorf("%s: timeConstraintFor(%s) = %+v, %v; want %+v", tc.name, period, got, ok, tc.want)
		}
	}
}

func TestTimeConstraintForRejectsNonsense(t *testing.T) {
	for name, tc := range map[string]struct {
		period       time.Duration
		numer, denom uint32
	}{
		"zero period":      {0, 125, 3},
		"negative period":  {-time.Millisecond, 125, 3},
		"no numerator":     {time.Millisecond, 0, 3},
		"no denominator":   {time.Millisecond, 125, 0},
		"no timebase":      {time.Millisecond, 0, 0},
		"less than a tick": {time.Nanosecond, 125, 3}, // 0.024 ticks
	} {
		if got, ok := timeConstraintFor(tc.period, tc.numer, tc.denom); ok {
			t.Errorf("%s: timeConstraintFor accepted it: %+v", name, got)
		}
	}
}

// Whatever the period, the policy must satisfy what the kernel enforces
// (constraint >= computation, computation inside its limits), and the shape the
// design asks for wherever clamping does not apply.
func TestTimeConstraintForInvariants(t *testing.T) {
	periods := []time.Duration{
		time.Microsecond, 20 * time.Microsecond, 99 * time.Microsecond, 100 * time.Microsecond, 101 * time.Microsecond,
		framesToDuration(16, 192000), framesToDuration(32, 96000), framesToDuration(64, 48000), framesToDuration(128, 44100),
		framesToDuration(128, 48000), framesToDuration(256, 48000), framesToDuration(1024, 44100), framesToDuration(4096, 44100),
		99 * time.Millisecond, 100 * time.Millisecond, 101 * time.Millisecond, 500 * time.Millisecond,
		time.Second, 5 * time.Second,
	}
	for _, tb := range timebases {
		ticks := func(d time.Duration) uint64 { return uint64(d) * uint64(tb.denom) / uint64(tb.numer) }
		for _, period := range periods {
			p, ok := timeConstraintFor(period, tb.numer, tb.denom)
			if !ok {
				t.Errorf("%s, %s: no policy", tb.name, period)
				continue
			}
			if p.computation > p.constraint || p.constraint > p.period {
				t.Errorf("%s, %s: computation %d, constraint %d, period %d are out of order", tb.name, period, p.computation, p.constraint, p.period)
			}
			if uint64(p.computation) < ticks(minComputation) || uint64(p.computation) > ticks(maxComputation) {
				t.Errorf("%s, %s: computation %d ticks is outside the kernel's %d..%d", tb.name, period, p.computation, ticks(minComputation), ticks(maxComputation))
			}
			if p.preemptible != 1 {
				t.Errorf("%s, %s: preemptible = %d, want 1", tb.name, period, p.preemptible)
			}
			// Unclamped: one period, half of it for computation.
			if half := ticks(min(period, maxPeriod)) / 2; half > ticks(minComputation)+1 && half < ticks(maxComputation)-1 {
				if p.period != p.constraint || p.computation != p.constraint/2 {
					t.Errorf("%s, %s: got %+v, want period == constraint and computation == constraint/2", tb.name, period, p)
				}
			}
		}
	}
}

func TestTimeConstraintForClampsInsteadOfOverflowing(t *testing.T) {
	// A timebase of 1/10 makes a second 10^10 ticks, past what 32 bits hold.
	p, ok := timeConstraintFor(time.Second, 1, 10)
	if !ok || p.period != math.MaxUint32 || p.constraint != math.MaxUint32 || p.computation > p.constraint || p.computation == 0 {
		t.Errorf("timeConstraintFor(1s, 1/10) = %+v, %v; want period and constraint saturated", p, ok)
	}
	// A period past maxPeriod is treated as maxPeriod.
	long, _ := timeConstraintFor(time.Hour, 1, 1)
	capped, _ := timeConstraintFor(maxPeriod, 1, 1)
	if long != capped {
		t.Errorf("an hour gave %+v, want the same as %s: %+v", long, maxPeriod, capped)
	}
	// A period too short for the kernel's minimum computation still gets a
	// valid policy: the computation floor drags constraint and period up.
	short, ok := timeConstraintFor(10*time.Microsecond, 1, 1)
	if !ok || short.computation <= 10_000/2 || short.computation > short.constraint || short.constraint > short.period {
		t.Errorf("timeConstraintFor(10us) = %+v, %v", short, ok)
	}
}

// fakeMach is a machAPI that records what the priority code asks of it.
type fakeMach struct {
	numer, denom uint32
	id           uintptr
	port         uint32
	isRT, known  bool
	setOK        bool

	setCalls      []timeConstraint
	standardCalls int
	lastPort      uint32
}

func newFakeMach() *fakeMach {
	return &fakeMach{numer: 125, denom: 3, id: 1, port: 259, known: true, setOK: true}
}

func (f *fakeMach) timebase() (uint32, uint32)    { return f.numer, f.denom }
func (f *fakeMach) thisThread() (uintptr, uint32) { return f.id, f.port }
func (f *fakeMach) realtime(uint32) (bool, bool)  { return f.isRT, f.known }
func (f *fakeMach) setRealtime(port uint32, p timeConstraint) bool {
	f.lastPort = port
	f.setCalls = append(f.setCalls, p)
	return f.setOK
}
func (f *fakeMach) setStandard(port uint32) bool {
	f.lastPort = port
	f.standardCalls++
	return true
}

func TestRaiseAppliesThePolicyAndLowerUndoesItOnce(t *testing.T) {
	f := newFakeMach()
	g := raiseDarwinPriority(f, framesToDuration(128, 44100))
	if g.Kind != "time-constraint" || g.Priority != 0 || g.String() != "time-constraint" {
		t.Fatalf("grant = %+v (%q), want time-constraint", g, g.String())
	}
	want := timeConstraint{period: 69659, computation: 34829, constraint: 69659, preemptible: 1}
	if len(f.setCalls) != 1 || f.setCalls[0] != want || f.lastPort != 259 {
		t.Fatalf("setRealtime calls = %+v on port %d, want one call of %+v on port 259", f.setCalls, f.lastPort, want)
	}
	if f.standardCalls != 0 {
		t.Fatal("the thread was lowered before Lower")
	}
	Lower(g)
	Lower(g)
	if f.standardCalls != 1 {
		t.Errorf("setStandard calls = %d after two Lowers, want 1", f.standardCalls)
	}
}

func TestRaiseProceedsWhenThePolicyCannotBeRead(t *testing.T) {
	f := newFakeMach()
	f.known = false
	g := raiseDarwinPriority(f, time.Millisecond)
	if g.Kind != "time-constraint" || len(f.setCalls) != 1 {
		t.Errorf("grant %+v after %d sets; the kernel's answer to set decides, not a failed read", g, len(f.setCalls))
	}
}

func TestRaiseFailureIsNormalAndLeavesNothingToLower(t *testing.T) {
	f := newFakeMach()
	f.setOK = false
	g := raiseDarwinPriority(f, time.Millisecond)
	if g.Kind != "normal" || g.String() != "normal" {
		t.Errorf("a refused policy gave %+v, want normal", g)
	}
	Lower(g)
	if f.standardCalls != 0 {
		t.Error("Lower reset a thread that was never raised")
	}
}

func TestRaiseLeavesAnAlreadyRealtimeThreadAlone(t *testing.T) {
	f := newFakeMach()
	f.isRT = true
	g := raiseDarwinPriority(f, time.Millisecond)
	if g.Kind != "time-constraint" {
		t.Errorf("grant = %+v; the thread is real-time, so say so", g)
	}
	Lower(g)
	if len(f.setCalls) != 0 || f.standardCalls != 0 {
		t.Errorf("touched a thread someone else made real-time: %d sets, %d resets", len(f.setCalls), f.standardCalls)
	}
}

func TestRaiseNeedsATimebaseAThreadAndAPeriod(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*fakeMach)
		period time.Duration
	}{
		"no timebase": {func(f *fakeMach) { f.numer, f.denom = 0, 0 }, time.Millisecond},
		"no port":     {func(f *fakeMach) { f.port = 0 }, time.Millisecond},
		"no period":   {func(*fakeMach) {}, 0},
	} {
		f := newFakeMach()
		tc.mutate(f)
		if g := raiseDarwinPriority(f, tc.period); g.Kind != "normal" || len(f.setCalls) != 0 {
			t.Errorf("%s: grant %+v after %d sets, want normal and none", name, g, len(f.setCalls))
		}
	}
}

// Lower from a thread other than the one raised must not touch anything, and
// must not use up the real Lower either.
func TestLowerFromAnotherThreadDoesNothing(t *testing.T) {
	f := newFakeMach()
	g := raiseDarwinPriority(f, time.Millisecond)
	f.id = 2 // a different OS thread
	Lower(g)
	if f.standardCalls != 0 {
		t.Fatal("Lower reset the thread from a different one")
	}
	f.id = 1
	Lower(g)
	if f.standardCalls != 1 {
		t.Errorf("setStandard calls = %d once back on the raised thread, want 1", f.standardCalls)
	}
}

// translated reports whether this process runs under Rosetta.
func translated() bool {
	v, err := syscall.Sysctl("sysctl.proc_translated")
	return err == nil && len(v) > 0 && v[0] == 1
}

// within reports whether a and b are the same policy to within slack ticks in
// each of their times.
func within(a, b timeConstraint, slack int64) bool {
	near := func(x, y uint32) bool { return max(int64(x)-int64(y), int64(y)-int64(x)) <= slack }
	return near(a.period, b.period) && near(a.computation, b.computation) && near(a.constraint, b.constraint) &&
		a.preemptible == b.preemptible
}

// policyOf reads the calling thread's time-constraint policy from the kernel.
// It is safe on any goroutine; readPolicy is the version for the test's own.
func policyOf() (p timeConstraint, isRT bool, kr int32) {
	_, port := machKernel{}.thisThread()
	count := uint32(threadTimeConstraintCount)
	var isDefault int32
	kr = threadPolicyGet(port, threadTimeConstraintPolicy, unsafe.Pointer(&p), &count, &isDefault)
	return p, isDefault == 0, kr
}

func readPolicy(t *testing.T) (timeConstraint, bool) {
	t.Helper()
	p, isRT, kr := policyOf()
	if kr != 0 {
		t.Fatalf("thread_policy_get: kern_return_t %d", kr)
	}
	return p, isRT
}

// The real kernel, on this thread: the policy lands, reads back as designed,
// and Lower puts the thread back among the timesharing threads.
func TestRaiseAndLowerOnARealThread(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread() // registered first, so it runs after the deferred Lower below

	if _, isRT := readPolicy(t); isRT {
		t.Skip("this test thread is already real-time")
	}
	period := framesToDuration(128, 44100)
	g := Raise(period)
	defer Lower(g)
	if g.Kind != "time-constraint" {
		t.Fatalf("Raise(%s) = %q; the kernel refused the policy", period, g.String())
	}
	got, isRT := readPolicy(t)
	if !isRT {
		t.Fatal("the thread is not real-time after Raise")
	}
	numer, denom := machKernel{}.timebase()
	want, _ := timeConstraintFor(period, numer, denom)
	// A translated (Rosetta) process sees a nanosecond clock while an Apple
	// silicon kernel counts 125/3 ns ticks, so the kernel holds what was asked
	// for to the nearest of its own ticks (41.7 ns) and reads it back rounded.
	slack := int64(0)
	if translated() {
		slack = 42
	}
	if !within(got, want, slack) {
		t.Errorf("kernel policy = %+v, want %+v (give or take %d)", got, want, slack)
	}

	Lower(g)
	if _, isRT := readPolicy(t); isRT {
		t.Error("the thread is still real-time after Lower")
	}
	Lower(g) // idempotent
	if _, isRT := readPolicy(t); isRT {
		t.Error("the thread became real-time again after a second Lower")
	}
}

// A second Raise on a real-time thread must not take over its policy: Lowering
// it leaves the thread as it was, and only the first Lower ends the real-time.
func TestNestedRaiseOnARealThread(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	outer := Raise(2 * time.Millisecond)
	defer Lower(outer)
	if outer.Kind != "time-constraint" {
		t.Skipf("kernel refused the first Raise: %q", outer.String())
	}
	before, _ := readPolicy(t)
	inner := Raise(5 * time.Millisecond)
	if inner.Kind != "time-constraint" {
		t.Fatalf("inner Raise = %q on a real-time thread", inner.String())
	}
	if after, _ := readPolicy(t); after != before {
		t.Errorf("inner Raise changed the policy from %+v to %+v", before, after)
	}
	Lower(inner)
	if _, isRT := readPolicy(t); !isRT {
		t.Fatal("Lowering the inner grant ended the outer one's real-time")
	}
	Lower(outer)
	if _, isRT := readPolicy(t); isRT {
		t.Error("the thread is still real-time after the outer Lower")
	}
}

// Lower called on the wrong OS thread must leave the raised thread raised.
func TestLowerFromAnotherRealThreadLeavesTheRaisedOneRaised(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	g := Raise(2 * time.Millisecond)
	defer Lower(g)
	if g.Kind != "time-constraint" {
		t.Skipf("kernel refused Raise: %q", g.String())
	}
	type result struct {
		isRT bool
		kr   int32
	}
	done := make(chan result)
	go func() {
		runtime.LockOSThread() // a thread of its own; it exits with the goroutine
		Lower(g)
		_, isRT, kr := policyOf()
		done <- result{isRT, kr}
	}()
	if other := <-done; other.kr != 0 || other.isRT {
		t.Errorf("the other thread: real-time = %v, kern_return_t %d; want a timesharing thread", other.isRT, other.kr)
	}
	if _, isRT := readPolicy(t); !isRT {
		t.Error("a Lower from another thread ended the raised thread's real-time")
	}
	Lower(g)
	if _, isRT := readPolicy(t); isRT {
		t.Error("Lower on the raised thread did not end its real-time")
	}
}
