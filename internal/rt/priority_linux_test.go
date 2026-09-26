//go:build linux && (amd64 || arm64)

package rt

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

type priorityFake struct {
	previous                                       linuxSchedAttr
	timeLimit, prioLimit                           syscall.Rlimit
	getTimeErr, setTimeErr, getPrioErr, getAttrErr error
	setErrors                                      []error
	setAttrs                                       []linuxSchedAttr
	setLimits                                      []syscall.Rlimit
	events                                         []string
	tid                                            int
}

func newPriorityFake() *priorityFake {
	return &priorityFake{
		previous:  linuxSchedAttr{Size: 56, Policy: 0, Nice: 5},
		timeLimit: syscall.Rlimit{Cur: ^uint64(0), Max: ^uint64(0)},
		tid:       123,
	}
}

func (f *priorityFake) calls() linuxPriorityCalls {
	return linuxPriorityCalls{
		getLimit: func(resource int, dst *syscall.Rlimit) error {
			switch resource {
			case linuxRlimitRTTime:
				f.events = append(f.events, "gettime")
				*dst = f.timeLimit
				return f.getTimeErr
			case linuxRlimitRTPrio:
				f.events = append(f.events, "getprio")
				*dst = f.prioLimit
				return f.getPrioErr
			default:
				panic("unexpected resource")
			}
		},
		setLimit: func(resource int, limit *syscall.Rlimit) error {
			if resource != linuxRlimitRTTime {
				panic("unexpected resource")
			}
			f.events = append(f.events, "settime")
			f.setLimits = append(f.setLimits, *limit)
			return f.setTimeErr
		},
		getAttr: func(dst *linuxSchedAttr) error {
			f.events = append(f.events, "getattr")
			*dst = f.previous
			return f.getAttrErr
		},
		setAttr: func(attr *linuxSchedAttr) error {
			f.events = append(f.events, "setattr")
			f.setAttrs = append(f.setAttrs, *attr)
			if len(f.setErrors) > 0 {
				err := f.setErrors[0]
				f.setErrors = f.setErrors[1:]
				return err
			}
			return nil
		},
		getTID: func() int { return f.tid },
	}
}

func TestLinuxRTTimeCap(t *testing.T) {
	for _, tc := range []struct {
		name      string
		old, want syscall.Rlimit
	}{
		{"unlimited", syscall.Rlimit{Cur: ^uint64(0), Max: ^uint64(0)}, syscall.Rlimit{Cur: 200_000, Max: 200_000}},
		{"stricter soft", syscall.Rlimit{Cur: 10_000, Max: 500_000}, syscall.Rlimit{Cur: 10_000, Max: 200_000}},
		{"stricter both", syscall.Rlimit{Cur: 10_000, Max: 20_000}, syscall.Rlimit{Cur: 10_000, Max: 20_000}},
		{"zero soft", syscall.Rlimit{Cur: 0, Max: 500_000}, syscall.Rlimit{Cur: 0, Max: 200_000}},
		{"zero both", syscall.Rlimit{}, syscall.Rlimit{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPriorityFake()
			f.timeLimit = tc.old
			if !capLinuxRTTime(f.calls()) {
				t.Fatal("cap failed")
			}
			if tc.old == tc.want {
				if len(f.setLimits) != 0 {
					t.Fatal("rewrote an already safe limit")
				}
			} else if len(f.setLimits) != 1 || f.setLimits[0] != tc.want {
				t.Fatalf("limits = %v, want %v", f.setLimits, tc.want)
			}
		})
	}
}

func TestLinuxPrioritySafetyFailures(t *testing.T) {
	for _, step := range []string{"snapshot", "read cap", "write cap"} {
		t.Run(step, func(t *testing.T) {
			f := newPriorityFake()
			switch step {
			case "snapshot":
				f.getAttrErr = syscall.ENOSYS
			case "read cap":
				f.getTimeErr = syscall.EPERM
			case "write cap":
				f.setTimeErr = syscall.EPERM
			}
			g := raiseLinuxPriority(f.calls())
			if g.String() != "normal" || g.restore != nil || len(f.setAttrs) != 0 {
				t.Fatalf("unsafe attempt: grant %v, attrs %v", g, f.setAttrs)
			}
		})
	}
}

func TestLinuxPriorityPermissionFallback(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		limit                       uint64
		readErr, firstErr, retryErr error
		wantPriority, wantAttempts  int
	}{
		{"limited grant", 20, nil, syscall.EPERM, nil, 20, 2},
		{"bounded grant", ^uint64(0), nil, syscall.EPERM, nil, 70, 2},
		{"retry denied", 20, nil, syscall.EPERM, syscall.EPERM, 0, 2},
		{"no allowance", 0, nil, syscall.EPERM, nil, 0, 1},
		{"unreadable allowance", 20, syscall.EPERM, syscall.EPERM, nil, 0, 1},
		{"unsupported syscall", 20, nil, syscall.ENOSYS, nil, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPriorityFake()
			f.prioLimit = syscall.Rlimit{Cur: tc.limit, Max: ^uint64(0)}
			f.getPrioErr = tc.readErr
			f.setErrors = []error{tc.firstErr, tc.retryErr}
			g := raiseLinuxPriority(f.calls())
			if g.Priority != tc.wantPriority || len(f.setAttrs) != tc.wantAttempts {
				t.Fatalf("grant %v, attempts %v", g, f.setAttrs)
			}
			if tc.wantPriority == 0 && (g.Kind != "normal" || g.restore != nil) {
				t.Fatalf("fabricated grant: %v", g)
			}
			if tc.wantPriority != 0 && (g.Kind != "SCHED_FIFO" || g.restore == nil) {
				t.Fatalf("missing grant: %v", g)
			}
			if f.setAttrs[0].Priority != 70 || f.setAttrs[0].Flags != linuxResetOnFork {
				t.Fatalf("first attempt = %+v", f.setAttrs[0])
			}
			if tc.wantAttempts == 2 && f.setAttrs[1].Priority != uint32(min(tc.limit, uint64(70))) {
				t.Fatalf("retry exceeded soft limit: %+v", f.setAttrs[1])
			}
		})
	}
}

func TestLinuxPriorityRestore(t *testing.T) {
	f := newPriorityFake()
	f.previous = linuxSchedAttr{Size: 56, Policy: linuxSchedRR, Flags: linuxResetOnFork, Nice: -3, Priority: 15, UtilMin: 12, UtilMax: 900}
	g := raiseLinuxPriority(f.calls())
	if g.String() != "SCHED_FIFO 70" {
		t.Fatalf("grant = %v", g)
	}
	if strings.Join(f.events, ",") != "getattr,gettime,settime,setattr" {
		t.Fatalf("cap must precede FIFO: %v", f.events)
	}
	f.tid++
	Lower(g)
	if len(f.setAttrs) != 1 {
		t.Fatal("Lower changed the wrong calling thread")
	}
	f.tid--
	Lower(g)
	Lower(g)
	if len(f.setAttrs) != 2 || f.setAttrs[1] != f.previous {
		t.Fatalf("restoration = %v, want %+v once", f.setAttrs, f.previous)
	}
	if len(f.setLimits) != 1 {
		t.Fatal("Lower changed the process safety cap")
	}
}

func TestLinuxPriorityRestoreRetainsRequiredResetFlag(t *testing.T) {
	f := newPriorityFake()
	f.setErrors = []error{nil, syscall.EPERM, nil}
	g := raiseLinuxPriority(f.calls())
	Lower(g)
	Lower(g)
	want := f.previous
	want.Flags |= linuxResetOnFork
	if len(f.setAttrs) != 3 || f.setAttrs[1] != f.previous || f.setAttrs[2] != want {
		t.Fatalf("restoration = %v", f.setAttrs)
	}
}

func TestLinuxPriorityPreservesStrongerExistingGrant(t *testing.T) {
	for _, tc := range []struct {
		policy, priority uint32
		want             string
		attempts         int
	}{
		{linuxSchedFIFO, 80, "SCHED_FIFO 80", 0},
		{linuxSchedRR, 65, "SCHED_RR 65", 1},
		{linuxSchedDeadline, 0, "SCHED_DEADLINE", 0},
	} {
		t.Run(tc.want, func(t *testing.T) {
			f := newPriorityFake()
			f.previous.Policy, f.previous.Priority = tc.policy, tc.priority
			f.prioLimit = syscall.Rlimit{Cur: 20, Max: 20}
			f.setErrors = []error{syscall.EPERM}
			g := raiseLinuxPriority(f.calls())
			Lower(g)
			if g.String() != tc.want || g.restore != nil || len(f.setAttrs) != tc.attempts {
				t.Fatalf("grant %v, attempts %v", g, f.setAttrs)
			}
		})
	}
}

func TestLinuxSchedAttrABI(t *testing.T) {
	var attr linuxSchedAttr
	if unsafe.Sizeof(attr) != 56 || unsafe.Offsetof(attr.Flags) != 8 || unsafe.Offsetof(attr.Priority) != 20 || unsafe.Offsetof(attr.Runtime) != 24 || unsafe.Offsetof(attr.UtilMax) != 52 {
		t.Fatal("sched_attr ABI mismatch")
	}
	set, get := linuxSchedSyscalls()
	if (runtime.GOARCH == "amd64" && (set != 314 || get != 315)) || (runtime.GOARCH == "arm64" && (set != 274 || get != 275)) {
		t.Fatalf("syscalls = %d/%d", set, get)
	}
}

func TestLinuxUnprivilegedThread(t *testing.T) {
	// Isolate the irreversible process-wide RTTIME cap from the other tests.
	if os.Getenv("TYMBAL_RT_UNPRIVILEGED_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxUnprivilegedThread$", "-test.v")
		cmd.Env = append(os.Environ(), "TYMBAL_RT_UNPRIVILEGED_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		t.Logf("%s", out)
		return
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skip(err)
	}
	found := false
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "CapEff:") {
			caps, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "CapEff:")), 16, 64)
			if err != nil || caps&(1<<23) != 0 {
				t.Skip("do not attempt host FIFO with CAP_SYS_NICE")
			}
			found = true
		}
	}
	if !found {
		t.Skip("cannot establish unprivileged scheduling")
	}
	var limit syscall.Rlimit
	if syscall.Getrlimit(linuxRlimitRTPrio, &limit) != nil || limit.Cur != 0 {
		t.Skip("do not attempt host FIFO with an RT allowance")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	before := linuxSchedAttr{Size: 56}
	if err := linuxGetSchedAttr(&before); err != nil {
		t.Skip(err)
	}
	if before.Policy != 0 {
		t.Skip("test requires a normal scheduling thread")
	}
	g := Raise(time.Millisecond)
	defer Lower(g)
	if g.String() != "normal" {
		t.Fatalf("unprivileged grant = %v", g)
	}
	Lower(g)
	after := linuxSchedAttr{Size: 56}
	if err := linuxGetSchedAttr(&after); err != nil || before != after {
		t.Fatalf("scheduling changed: before %+v, after %+v, error %v", before, after, err)
	}
	if err := syscall.Getrlimit(linuxRlimitRTTime, &limit); err != nil || limit.Cur > 200_000 || limit.Max > 200_000 {
		t.Fatalf("unsafe RTTIME limit %+v, error %v", limit, err)
	}
}
