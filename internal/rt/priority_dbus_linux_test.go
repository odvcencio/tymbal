//go:build linux && (amd64 || arm64)

package rt

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"m31labs.dev/tymbal/internal/dbus"
)

type serviceMethod struct {
	destination, path, iface, member, input, output string
	args                                            []any
}
type serviceBusFake struct {
	properties map[string]dbus.Variant
	getErrors  map[string]error
	calls      []serviceMethod
	onCall     func(serviceMethod)
	callErr    error
	closed     int
	events     *[]string
}

func newServiceBus() *serviceBusFake {
	return &serviceBusFake{properties: map[string]dbus.Variant{
		"MaxRealtimePriority": {Signature: "i", Value: int32(20)}, "RTTimeUSecMax": {Signature: "x", Value: int64(100000)}, "MinNiceLevel": {Signature: "i", Value: int32(-10)},
	}, getErrors: map[string]error{}}
}
func (b *serviceBusFake) Get(destination, path, iface, property string) (dbus.Variant, error) {
	if b.events != nil {
		*b.events = append(*b.events, "property:"+property)
	}
	return b.properties[property], b.getErrors[property]
}
func (b *serviceBusFake) Call(destination, path, iface, member, input, output string, args ...any) ([]any, error) {
	m := serviceMethod{destination, path, iface, member, input, output, args}
	b.calls = append(b.calls, m)
	if b.events != nil {
		*b.events = append(*b.events, "method:"+member)
	}
	if b.onCall != nil {
		b.onCall(m)
	}
	return nil, b.callErr
}
func (b *serviceBusFake) Close() error { b.closed++; return nil }

type servicePriorityFake struct {
	*priorityFake
	actual          linuxSchedAttr
	attrReads       int
	verificationErr error
	addresses       []string
	deadlines       []time.Time
	dial            func(string, time.Time) (linuxPriorityBus, error)
}

func newServicePriorityFake() *servicePriorityFake {
	f := newPriorityFake()
	f.setErrors = []error{syscall.EPERM}
	return &servicePriorityFake{priorityFake: f, actual: f.previous}
}
func (f *servicePriorityFake) calls() linuxPriorityCalls {
	c := f.priorityFake.calls()
	getAttr, setAttr, setLimit := c.getAttr, c.setAttr, c.setLimit
	c.getAttr = func(dst *linuxSchedAttr) error {
		if err := getAttr(dst); err != nil {
			return err
		}
		f.attrReads++
		if f.attrReads > 1 && f.verificationErr != nil {
			return f.verificationErr
		}
		*dst = f.actual
		return nil
	}
	c.setAttr = func(attr *linuxSchedAttr) error {
		if err := setAttr(attr); err != nil {
			return err
		}
		f.actual = *attr
		return nil
	}
	c.setLimit = func(resource int, limit *syscall.Rlimit) error {
		if err := setLimit(resource, limit); err != nil {
			return err
		}
		f.timeLimit = *limit
		return nil
	}
	c.getPID = func() int { return 456 }
	c.connect = func(address string, deadline time.Time) (linuxPriorityBus, error) {
		f.addresses = append(f.addresses, address)
		f.deadlines = append(f.deadlines, deadline)
		f.events = append(f.events, "dial:"+address)
		return f.dial(address, deadline)
	}
	c.sessionAddress = "session"
	c.systemAddress = "system"
	return c
}
func (f *servicePriorityFake) grantRT(m serviceMethod) {
	f.actual.Policy = linuxSchedRR
	f.actual.Priority = m.args[2].(uint32)
	f.actual.Flags |= linuxResetOnFork
}

func TestLinuxServiceRTOrderAndRestore(t *testing.T) {
	f := newServicePriorityFake()
	portal, kit := newServiceBus(), newServiceBus()
	portal.getErrors["MaxRealtimePriority"] = syscall.ENOENT
	kit.onCall = f.grantRT
	kit.events = &f.events
	f.dial = func(address string, _ time.Time) (linuxPriorityBus, error) {
		if address == "session" {
			return portal, nil
		}
		return kit, nil
	}
	g := raiseLinuxPriority(f.calls())
	if g.String() != "SCHED_RR 20" || g.restore == nil {
		t.Fatalf("grant %+v", g)
	}
	if !reflect.DeepEqual(f.addresses, []string{"session", "system"}) {
		t.Fatalf("order %v", f.addresses)
	}
	if len(f.setLimits) != 2 || f.setLimits[0] != (syscall.Rlimit{Cur: 200000, Max: 200000}) || f.setLimits[1] != (syscall.Rlimit{Cur: 100000, Max: 100000}) {
		t.Fatalf("limit order %v", f.setLimits)
	}
	events := strings.Join(f.events, ",")
	if !strings.Contains(events, "property:MaxRealtimePriority,property:RTTimeUSecMax,gettime,settime,method:MakeThreadRealtimeWithPID,getattr") {
		t.Fatalf("unsafe service order %s", events)
	}
	m := kit.calls[0]
	if m.destination != "org.freedesktop.RealtimeKit1" || m.path != "/org/freedesktop/RealtimeKit1" || m.iface != "org.freedesktop.RealtimeKit1" || m.input != "ttu" || m.output != "" || !reflect.DeepEqual(m.args, []any{uint64(456), uint64(123), uint32(20)}) {
		t.Fatalf("protocol %+v", m)
	}
	if portal.closed != 1 || kit.closed != 1 {
		t.Fatal("bus connection leaked")
	}
	// Restoration is scoped to the calling tid and occurs once.
	f.tid++
	Lower(g)
	if len(f.setAttrs) != 1 {
		t.Fatal("changed another thread")
	}
	f.tid--
	Lower(g)
	Lower(g)
	if f.actual != f.previous || len(f.setAttrs) != 2 || len(f.setLimits) != 2 {
		t.Fatalf("restore %+v, attrs %v, limits %v", f.actual, f.setAttrs, f.setLimits)
	}
}
func TestLinuxPortalProtocolAndActualGrant(t *testing.T) {
	f := newServicePriorityFake()
	bus := newServiceBus()
	bus.properties["MaxRealtimePriority"] = dbus.Variant{Signature: "i", Value: int32(90)}
	bus.onCall = func(m serviceMethod) { f.grantRT(m); f.actual.Priority = 17 }
	f.dial = func(string, time.Time) (linuxPriorityBus, error) { return bus, nil }
	g := raiseLinuxPriority(f.calls())
	if g.String() != "SCHED_RR 17" {
		t.Fatalf("reported requested priority instead of actual: %v", g)
	}
	m := bus.calls[0]
	if m.destination != "org.freedesktop.portal.Desktop" || m.path != "/org/freedesktop/portal/desktop" || m.iface != "org.freedesktop.portal.Realtime" || m.member != "MakeThreadRealtimeWithPID" || m.args[2] != uint32(70) {
		t.Fatalf("portal protocol %+v", m)
	}
	Lower(g)
}
func TestLinuxServiceReplyDoesNotFabricatePriority(t *testing.T) {
	for _, callErr := range []error{nil, syscall.EPERM} {
		f := newServicePriorityFake()
		bus := newServiceBus()
		bus.callErr = callErr
		f.dial = func(string, time.Time) (linuxPriorityBus, error) { return bus, nil }
		g := raiseLinuxPriority(f.calls())
		if g.String() != "normal" || g.restore != nil || len(bus.calls) != 4 || f.attrReads != 5 {
			t.Fatalf("fabricated grant %v, methods %d, reads %d", g, len(bus.calls), f.attrReads)
		}
	}
}
func TestLinuxServiceLostReplyStillVerifies(t *testing.T) {
	f := newServicePriorityFake()
	bus := newServiceBus()
	bus.callErr = errors.New("lost reply")
	bus.onCall = f.grantRT
	f.dial = func(string, time.Time) (linuxPriorityBus, error) { return bus, nil }
	g := raiseLinuxPriority(f.calls())
	if g.String() != "SCHED_RR 20" || f.attrReads != 2 {
		t.Fatalf("grant %v reads %d", g, f.attrReads)
	}
	Lower(g)
}
func TestLinuxServiceVerificationFailureAndUnsafeGrant(t *testing.T) {
	for _, mode := range []string{"getattr error", "missing reset flag", "zero priority", "out of range priority", "changed policy"} {
		t.Run(mode, func(t *testing.T) {
			f := newServicePriorityFake()
			bus := newServiceBus()
			bus.onCall = func(m serviceMethod) {
				f.grantRT(m)
				switch mode {
				case "getattr error":
					f.verificationErr = syscall.EIO
				case "missing reset flag":
					f.actual.Flags = 0
				case "zero priority":
					f.actual.Priority = 0
				case "out of range priority":
					f.actual.Priority = 100
				case "changed policy":
					f.actual.Policy = 5
				}
			}
			f.dial = func(string, time.Time) (linuxPriorityBus, error) { return bus, nil }
			g := raiseLinuxPriority(f.calls())
			if g.String() != "normal" || f.actual != f.previous || len(f.setAttrs) != 2 {
				t.Fatalf("unsafe grant %+v, actual %+v, sets %v", g, f.actual, f.setAttrs)
			}
		})
	}
}
func TestLinuxServiceCapAndProperties(t *testing.T) {
	for _, mode := range []string{"stricter limits", "cap failure", "wrong max type", "max negative", "max huge", "wrong time type", "time negative", "time zero", "property error"} {
		t.Run(mode, func(t *testing.T) {
			f := newServicePriorityFake()
			bus := newServiceBus()
			bus.onCall = f.grantRT
			switch mode {
			case "stricter limits":
				f.timeLimit = syscall.Rlimit{Cur: 2000, Max: 3000}
			case "cap failure":
				bus.properties["RTTimeUSecMax"] = dbus.Variant{Signature: "x", Value: int64(1000)}
				f.timeLimit = syscall.Rlimit{Cur: 2000, Max: 3000}
				f.setTimeErr = syscall.EPERM
			case "wrong max type":
				bus.properties["MaxRealtimePriority"] = dbus.Variant{Signature: "u", Value: uint32(20)}
			case "max negative":
				bus.properties["MaxRealtimePriority"] = dbus.Variant{Signature: "i", Value: int32(-1)}
			case "max huge":
				bus.properties["MaxRealtimePriority"] = dbus.Variant{Signature: "i", Value: int32(100)}
			case "wrong time type":
				bus.properties["RTTimeUSecMax"] = dbus.Variant{Signature: "t", Value: uint64(100000)}
			case "time negative":
				bus.properties["RTTimeUSecMax"] = dbus.Variant{Signature: "x", Value: int64(-1)}
			case "time zero":
				bus.properties["RTTimeUSecMax"] = dbus.Variant{Signature: "x", Value: int64(0)}
			case "property error":
				bus.getErrors["RTTimeUSecMax"] = syscall.EIO
			}
			// Only RT negotiation here; don't let a nice request mutate this fake.
			bus.getErrors["MinNiceLevel"] = syscall.ENOENT
			f.dial = func(string, time.Time) (linuxPriorityBus, error) { return bus, nil }
			g := raiseLinuxPriority(f.calls())
			if mode == "stricter limits" {
				if g.String() != "SCHED_RR 20" || len(f.setLimits) != 0 {
					t.Fatalf("weakened cap %v", f.setLimits)
				}
				Lower(g)
			} else if g.String() != "normal" || len(bus.calls) != 0 {
				t.Fatalf("unsafe attempt %v, calls %v", g, bus.calls)
			}
		})
	}
}
func TestLinuxNiceFallbackTargetsCallingTID(t *testing.T) {
	f := newServicePriorityFake()
	bus := newServiceBus()
	bus.onCall = func(m serviceMethod) {
		if m.member == "MakeThreadHighPriorityWithPID" {
			f.actual.Nice = -7
			f.actual.Flags = linuxResetOnFork
		}
	}
	f.dial = func(string, time.Time) (linuxPriorityBus, error) { return bus, nil }
	g := raiseLinuxPriority(f.calls())
	if g.String() != "nice -7" || g.restore == nil || len(bus.calls) != 3 {
		t.Fatalf("nice grant %v calls %+v", g, bus.calls)
	}
	m := bus.calls[2]
	if m.input != "tti" || !reflect.DeepEqual(m.args, []any{uint64(456), uint64(123), int32(-10)}) {
		t.Fatalf("nice method %+v", m)
	}
	f.setErrors = []error{syscall.EPERM, nil}
	Lower(g)
	want := f.previous
	want.Flags |= linuxResetOnFork
	if f.actual != want || len(f.setAttrs) != 3 {
		t.Fatalf("restore %+v", f.actual)
	}
}
func TestLinuxNiceSystemFallbackAndPreservesStrongerNice(t *testing.T) {
	for _, level := range []int32{-21, -20, -10, 0, 1} {
		t.Run("level "+strconv.Itoa(int(level)), func(t *testing.T) {
			f := newServicePriorityFake()
			f.previous.Nice = -15
			f.actual = f.previous
			bus := newServiceBus()
			bus.getErrors["MaxRealtimePriority"] = syscall.ENOENT
			bus.properties["MinNiceLevel"] = dbus.Variant{Signature: "i", Value: level}
			bus.onCall = func(m serviceMethod) { f.actual.Nice = m.args[2].(int32) }
			f.dial = func(address string, _ time.Time) (linuxPriorityBus, error) {
				if address == "session" {
					return nil, syscall.ENOENT
				}
				return bus, nil
			}
			g := raiseLinuxPriority(f.calls())
			if level == -20 {
				if g.String() != "nice -20" || len(bus.calls) != 1 || bus.calls[0].iface != "org.freedesktop.RealtimeKit1" {
					t.Fatalf("nice %v calls %v", g, bus.calls)
				}
				Lower(g)
			} else if len(bus.calls) != 0 || g.String() != "nice -15" {
				t.Fatalf("weakened existing nice %v calls %v", g, bus.calls)
			}
		})
	}
}
func TestLinuxServicePreservesExistingRT(t *testing.T) {
	for _, policy := range []uint32{linuxSchedFIFO, linuxSchedRR, linuxSchedDeadline} {
		f := newServicePriorityFake()
		f.previous.Policy = policy
		f.previous.Priority = 15
		f.actual = f.previous
		f.dial = func(string, time.Time) (linuxPriorityBus, error) {
			t.Fatal("existing RT was sent to a service that may lower it")
			return nil, syscall.EPERM
		}
		g := raiseLinuxPriority(f.calls())
		if g.restore != nil || len(f.addresses) != 0 {
			t.Fatalf("changed existing grant %v", g)
		}
	}
}
func TestLinuxServiceAbsoluteBudget(t *testing.T) {
	f := newServicePriorityFake()
	f.dial = func(_ string, deadline time.Time) (linuxPriorityBus, error) {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		<-timer.C
		return nil, errors.New("timeout")
	}
	start := time.Now()
	g := raiseLinuxPriority(f.calls())
	elapsed := time.Since(start)
	if g.String() != "normal" || len(f.deadlines) != 4 || elapsed > 1300*time.Millisecond {
		t.Fatalf("unbounded fallback: %v (%v), deadlines %v", g, elapsed, f.deadlines)
	}
	for i, want := range []time.Duration{300 * time.Millisecond, 700 * time.Millisecond, 850 * time.Millisecond, time.Second} {
		if delta := f.deadlines[i].Sub(start) - want; delta < 0 || delta > 30*time.Millisecond {
			t.Fatalf("phase deadline %v, want %v", f.deadlines[i].Sub(start), want)
		}
	}
}
func TestLinuxDirectFastPathDoesNotDial(t *testing.T) {
	f := newServicePriorityFake()
	f.setErrors = nil
	f.dial = func(string, time.Time) (linuxPriorityBus, error) {
		t.Fatal("direct success accessed D-Bus")
		return nil, syscall.EPERM
	}
	c := f.calls()
	c.getBusAddresses = func() (string, string) { t.Fatal("direct success looked up bus addresses"); return "", "" }
	g := raiseLinuxPriority(c)
	if g.String() != "SCHED_FIFO 70" {
		t.Fatal(g)
	}
	Lower(g)
}

func TestLinuxNoGrantStillRestoresServiceFlags(t *testing.T) {
	f := newServicePriorityFake()
	bus := newServiceBus()
	bus.callErr = syscall.EPERM
	bus.onCall = func(serviceMethod) { f.actual.Flags |= linuxResetOnFork }
	f.dial = func(string, time.Time) (linuxPriorityBus, error) { return bus, nil }
	g := raiseLinuxPriority(f.calls())
	if g.String() != "normal" || g.restore == nil {
		t.Fatalf("unrestorable service side effect: %+v", g)
	}
	f.setErrors = []error{syscall.EPERM, nil}
	Lower(g)
	want := f.previous
	want.Flags |= linuxResetOnFork
	if f.actual != want {
		t.Fatalf("reset restoration %+v", f.actual)
	}
}
