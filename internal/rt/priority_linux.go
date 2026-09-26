//go:build linux && (amd64 || arm64)

package rt

import (
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	linuxRlimitRTPrio  = 14
	linuxRlimitRTTime  = 15
	linuxRTTimeCapUS   = 200_000
	linuxFIFOPriority  = 70
	linuxSchedFIFO     = 1
	linuxSchedRR       = 2
	linuxSchedDeadline = 6
	linuxResetOnFork   = 1
)

// linuxSchedAttr matches Linux's 56-byte sched_attr ABI on amd64 and arm64.
// Keeping all fields lets Lower restore the prior policy and its parameters.
type linuxSchedAttr struct {
	Size     uint32
	Policy   uint32
	Flags    uint64
	Nice     int32
	Priority uint32
	Runtime  uint64
	Deadline uint64
	Period   uint64
	UtilMin  uint32
	UtilMax  uint32
}

// The seam is passed by value: tests never replace process-wide syscall hooks.
type linuxPriorityCalls struct {
	getLimit func(int, *syscall.Rlimit) error
	setLimit func(int, *syscall.Rlimit) error
	getAttr  func(*linuxSchedAttr) error
	setAttr  func(*linuxSchedAttr) error
	getTID   func() int
}

// Resource limits are shared by the process. Serialize our read/lower pairs so
// simultaneous stream startup cannot relax a limit another stream just lowered.
var linuxRTTimeMu sync.Mutex

func raisePriority(_ time.Duration) Grant {
	return raiseLinuxPriority(linuxPriorityCalls{
		getLimit: syscall.Getrlimit,
		setLimit: syscall.Setrlimit,
		getAttr:  linuxGetSchedAttr,
		setAttr:  linuxSetSchedAttr,
		getTID:   syscall.Gettid,
	})
}

func raiseLinuxPriority(c linuxPriorityCalls) Grant {
	previous := linuxSchedAttr{Size: uint32(unsafe.Sizeof(linuxSchedAttr{}))}
	if err := c.getAttr(&previous); err != nil {
		return Grant{Kind: "normal"}
	}
	fallback := Grant{Kind: "normal"}
	switch previous.Policy {
	case linuxSchedFIFO:
		fallback = Grant{Kind: "SCHED_FIFO", Priority: int(previous.Priority)}
	case linuxSchedRR:
		fallback = Grant{Kind: "SCHED_RR", Priority: int(previous.Priority)}
	case linuxSchedDeadline:
		fallback = Grant{Kind: "SCHED_DEADLINE"}
	}
	if !capLinuxRTTime(c) {
		return fallback
	}
	// Do not lower an existing stronger RT grant: raising it back could require
	// privileges the caller does not have. Leave deadline scheduling intact too.
	wasRT := previous.Policy == linuxSchedFIFO || previous.Policy == linuxSchedRR
	if (wasRT && previous.Priority > linuxFIFOPriority) || previous.Policy == linuxSchedDeadline {
		return fallback
	}

	tid := c.getTID()
	var once sync.Once
	// Prepare restoration before granting FIFO; nothing is allocated by Lower.
	restore := func() {
		if c.getTID() != tid {
			return // Never change a different thread if the caller violates the contract.
		}
		once.Do(func() {
			if err := c.setAttr(&previous); err == syscall.EPERM && previous.Flags&linuxResetOnFork == 0 {
				// Unprivileged RLIMIT_RTPRIO grants cannot clear RESET_ON_FORK.
				// Retain that safety flag while restoring the old policy/priority.
				previous.Flags |= linuxResetOnFork
				_ = c.setAttr(&previous)
			}
		})
	}
	attr := linuxSchedAttr{
		Size: previous.Size, Policy: linuxSchedFIFO,
		Flags: linuxResetOnFork, Nice: previous.Nice, Priority: linuxFIFOPriority,
	}
	err := c.setAttr(&attr)
	if err == syscall.EPERM {
		var limit syscall.Rlimit
		if c.getLimit(linuxRlimitRTPrio, &limit) != nil || limit.Cur == 0 {
			return fallback
		}
		attr.Priority = uint32(min(limit.Cur, uint64(linuxFIFOPriority)))
		if wasRT && attr.Priority < previous.Priority {
			return fallback
		}
		err = c.setAttr(&attr) // One retry, never above the requested priority.
	}
	if err != nil {
		// D-Bus portal, RealtimeKit, and nice fallbacks are unimplemented.
		return fallback
	}
	return Grant{Kind: "SCHED_FIFO", Priority: int(attr.Priority), restore: restore}
}

func capLinuxRTTime(c linuxPriorityCalls) bool {
	linuxRTTimeMu.Lock()
	defer linuxRTTimeMu.Unlock()
	var old syscall.Rlimit
	if c.getLimit(linuxRlimitRTTime, &old) != nil {
		return false
	}
	limit := syscall.Rlimit{
		Cur: min(old.Cur, uint64(linuxRTTimeCapUS)),
		Max: min(old.Max, uint64(linuxRTTimeCapUS)),
	}
	if limit == old {
		return true
	}
	// Do not restore this process-wide safety cap in Lower or raise either limit.
	return c.setLimit(linuxRlimitRTTime, &limit) == nil
}

func linuxSchedSyscalls() (set, get uintptr) {
	if runtime.GOARCH == "arm64" {
		return 274, 275
	}
	return 314, 315 // amd64; all other architectures use priority_other.go.
}

func linuxGetSchedAttr(attr *linuxSchedAttr) error {
	_, get := linuxSchedSyscalls()
	// pid=0 means only the calling OS thread, never the process leader.
	_, _, errno := syscall.RawSyscall6(get, 0, uintptr(unsafe.Pointer(attr)), unsafe.Sizeof(*attr), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func linuxSetSchedAttr(attr *linuxSchedAttr) error {
	set, _ := linuxSchedSyscalls()
	_, _, errno := syscall.RawSyscall(set, 0, uintptr(unsafe.Pointer(attr)), 0)
	if errno != 0 {
		return errno
	}
	return nil
}
