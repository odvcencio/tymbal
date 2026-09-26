//go:build linux && (amd64 || arm64)

package rt

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/dbus"
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
	getLimit                      func(int, *syscall.Rlimit) error
	setLimit                      func(int, *syscall.Rlimit) error
	getAttr                       func(*linuxSchedAttr) error
	setAttr                       func(*linuxSchedAttr) error
	getTID                        func() int
	getPID                        func() int
	connect                       func(string, time.Time) (linuxPriorityBus, error)
	sessionAddress, systemAddress string
	getBusAddresses               func() (string, string)
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
		getPID:   os.Getpid,
		connect: func(address string, deadline time.Time) (linuxPriorityBus, error) {
			return dbus.Dial(address, deadline)
		},
		getBusAddresses: func() (string, string) {
			return os.Getenv("DBUS_SESSION_BUS_ADDRESS"), linuxSystemBusAddress()
		},
	})
}

func raiseLinuxPriority(c linuxPriorityCalls) Grant {
	previous := linuxSchedAttr{Size: uint32(unsafe.Sizeof(linuxSchedAttr{}))}
	if err := c.getAttr(&previous); err != nil {
		return Grant{Kind: "normal"}
	}
	fallback := Grant{Kind: "normal"}
	switch previous.Policy {
	case 0:
		if previous.Nice < 0 {
			fallback = Grant{Kind: fmt.Sprintf("nice %d", previous.Nice)}
		}
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
	rollback := func() {
		if c.getTID() != tid {
			return // Never change a different thread if the caller violates the contract.
		}
		if err := c.setAttr(&previous); err == syscall.EPERM && previous.Flags&linuxResetOnFork == 0 {
			// Unprivileged RLIMIT_RTPRIO grants cannot clear RESET_ON_FORK.
			// Retain that safety flag while restoring the old policy/priority.
			previous.Flags |= linuxResetOnFork
			_ = c.setAttr(&previous)
		}
	}
	restore := func() {
		if c.getTID() == tid {
			once.Do(rollback)
		}
	}
	attr := linuxSchedAttr{
		Size: previous.Size, Policy: linuxSchedFIFO,
		Flags: linuxResetOnFork, Nice: previous.Nice, Priority: linuxFIFOPriority,
	}
	err := c.setAttr(&attr)
	if err == syscall.EPERM {
		var limit syscall.Rlimit
		if c.getLimit(linuxRlimitRTPrio, &limit) == nil && limit.Cur != 0 {
			attr.Priority = uint32(min(limit.Cur, uint64(linuxFIFOPriority)))
			if wasRT && attr.Priority < previous.Priority {
				return fallback
			}
			err = c.setAttr(&attr) // One retry, never above the requested priority.
		}
	}
	if err != nil {
		if !wasRT && (errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)) {
			return raiseLinuxServicePriority(c, previous, tid, rollback, restore, fallback)
		}
		return fallback
	}
	return Grant{Kind: "SCHED_FIFO", Priority: int(attr.Priority), restore: restore}
}

func capLinuxRTTime(c linuxPriorityCalls) bool {
	return capLinuxRTTimeAt(c, linuxRTTimeCapUS)
}

func capLinuxRTTimeAt(c linuxPriorityCalls, maximum uint64) bool {
	linuxRTTimeMu.Lock()
	defer linuxRTTimeMu.Unlock()
	var old syscall.Rlimit
	if c.getLimit(linuxRlimitRTTime, &old) != nil {
		return false
	}
	limit := syscall.Rlimit{
		Cur: min(old.Cur, maximum, uint64(linuxRTTimeCapUS)),
		Max: min(old.Max, maximum, uint64(linuxRTTimeCapUS)),
	}
	if limit == old {
		return true
	}
	// Do not restore this process-wide safety cap in Lower or raise either limit.
	return c.setLimit(linuxRlimitRTTime, &limit) == nil
}

// The bus seam is per invocation, just like the syscall seam. No host bus is
// accessed by syscall-only tests.
type linuxPriorityBus interface {
	Get(destination, path, iface, property string) (dbus.Variant, error)
	Call(destination, path, iface, member, input, output string, args ...any) ([]any, error)
	Close() error
}

func linuxSystemBusAddress() string {
	if address := os.Getenv("DBUS_SYSTEM_BUS_ADDRESS"); address != "" {
		return address
	}
	return "unix:path=/run/dbus/system_bus_socket"
}

// Each phase has an absolute share of a single one-second budget. A stalled
// session bus cannot consume the system-bus or final nice fallback's budget.
// No goroutine is used: verification and restoration stay on the locked thread.
func raiseLinuxServicePriority(c linuxPriorityCalls, previous linuxSchedAttr, tid int, rollback, restore func(), fallback Grant) Grant {
	if c.connect == nil || c.getPID == nil {
		return fallback
	}
	start := time.Now()
	if c.getBusAddresses != nil {
		c.sessionAddress, c.systemAddress = c.getBusAddresses()
	}
	type service struct{ address, destination, path, iface string }
	portal := service{c.sessionAddress, "org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop", "org.freedesktop.portal.Realtime"}
	rtkit := service{c.systemAddress, "org.freedesktop.RealtimeKit1", "/org/freedesktop/RealtimeKit1", "org.freedesktop.RealtimeKit1"}
	phases := []struct {
		service service
		nice    bool
		budget  time.Duration
	}{
		{portal, false, 300 * time.Millisecond},
		{rtkit, false, 700 * time.Millisecond},
		{portal, true, 850 * time.Millisecond},
		{rtkit, true, time.Second},
	}
	for _, phase := range phases {
		s := phase.service
		deadline := start.Add(phase.budget)
		if s.address == "" || !time.Now().Before(deadline) {
			continue
		}
		bus, err := c.connect(s.address, deadline)
		if err != nil {
			continue
		}
		attempted := func() bool {
			defer bus.Close()
			var priority any
			// Portal Realtime v1 and RTKit both expose WithPID methods.
			// https://flatpak.github.io/xdg-desktop-portal/docs/doc-org.freedesktop.portal.Realtime.html
			member, signature := "MakeThreadRealtimeWithPID", "ttu"
			if phase.nice {
				v, err := bus.Get(s.destination, s.path, s.iface, "MinNiceLevel")
				level, ok := v.Value.(int32)
				// Never worsen a preexisting nice level; RTKit's nice method
				// also changes policy, so only SCHED_OTHER is eligible.
				if err != nil || v.Signature != "i" || !ok || level < -20 || level >= 0 || level >= previous.Nice || previous.Policy != 0 {
					return false
				}
				priority, member, signature = level, "MakeThreadHighPriorityWithPID", "tti"
			} else {
				v, err := bus.Get(s.destination, s.path, s.iface, "MaxRealtimePriority")
				maximum, ok := v.Value.(int32)
				if err != nil || v.Signature != "i" || !ok || maximum <= 0 || maximum > 99 {
					return false
				}
				v, err = bus.Get(s.destination, s.path, s.iface, "RTTimeUSecMax")
				timeMax, ok := v.Value.(int64)
				if err != nil || v.Signature != "x" || !ok || timeMax <= 0 || !capLinuxRTTimeAt(c, uint64(timeMax)) {
					return false
				}
				priority = uint32(min(maximum, int32(linuxFIFOPriority)))
			}
			if c.getTID() != tid || !time.Now().Before(deadline) {
				return false
			}
			// Inspect the kernel even after an error: a lost reply can follow
			// a successful service mutation. A successful reply alone proves
			// nothing about what scheduling the calling thread now has.
			_, _ = bus.Call(s.destination, s.path, s.iface, member, signature, "", uint64(c.getPID()), uint64(tid), priority)
			return true
		}()
		if !attempted {
			continue
		}
		// Retain restoration even if the immediate kernel check is unchanged:
		// a service request with a lost reply may finish later in the stream.
		fallback.restore = restore
		actual := linuxSchedAttr{Size: previous.Size}
		if c.getAttr(&actual) != nil {
			rollback()
			return fallback
		}
		if actual.Policy == linuxSchedFIFO || actual.Policy == linuxSchedRR {
			if actual.Priority == 0 || actual.Priority > 99 || actual.Flags&linuxResetOnFork == 0 {
				rollback()
				return fallback
			}
			kind := "SCHED_RR"
			if actual.Policy == linuxSchedFIFO {
				kind = "SCHED_FIFO"
			}
			return Grant{Kind: kind, Priority: int(actual.Priority), restore: restore}
		}
		if actual.Policy == 0 && actual.Nice < previous.Nice && actual.Nice >= -20 {
			// Grant.String's common integer formatter handles only positive
			// values; keep the signed nice level in Kind without changing it.
			return Grant{Kind: fmt.Sprintf("nice %d", actual.Nice), restore: restore}
		}
		if actual.Policy != previous.Policy || actual.Nice != previous.Nice {
			rollback()
			return fallback
		}
	}
	return fallback
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
