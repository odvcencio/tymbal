//go:build darwin && (amd64 || arm64)

package rt

import (
	"math"
	"syscall"
	"time"
	"unsafe"
)

// These are public Mach interfaces. No Go callback is registered with Mach.
//go:cgo_import_dynamic tymbal_rt_mach_thread_self mach_thread_self "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic tymbal_rt_mach_task_self mach_task_self "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic tymbal_rt_mach_port_deallocate mach_port_deallocate "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic tymbal_rt_mach_timebase_info mach_timebase_info "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic tymbal_rt_thread_policy_get thread_policy_get "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic tymbal_rt_thread_policy_set thread_policy_set "/usr/lib/libSystem.B.dylib"

var fnRTThreadSelf, fnRTTaskSelf, fnRTPortDeallocate, fnRTTimebase, fnRTPolicyGet, fnRTPolicySet uintptr

//go:noescape
func darwinSyscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:noescape
func darwinSyscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:linkname darwinSyscall syscall.syscall
//go:linkname darwinSyscall6 syscall.syscall6

type timeConstraintPolicy struct{ period, computation, constraint, preemptible uint32 }
type darwinPriorityCalls struct {
	thread   func() uint32
	timebase func() (uint32, uint32)
	get      func(uint32, uint32, unsafe.Pointer, *uint32, *uint32) int32
	set      func(uint32, uint32, unsafe.Pointer, uint32) int32
	release  func(uint32)
}

func raisePriority(period time.Duration) Grant {
	return raiseDarwinPriority(period, darwinPriorityCalls{
		thread: func() uint32 { r, _, _ := darwinSyscall(fnRTThreadSelf, 0, 0, 0); return uint32(r) },
		timebase: func() (uint32, uint32) {
			var v struct{ n, d uint32 }
			r, _, _ := darwinSyscall(fnRTTimebase, uintptr(unsafe.Pointer(&v)), 0, 0)
			if r != 0 {
				return 0, 0
			}
			return v.n, v.d
		},
		get: func(thread, flavor uint32, p unsafe.Pointer, count, def *uint32) int32 {
			r, _, _ := darwinSyscall6(fnRTPolicyGet, uintptr(thread), uintptr(flavor), uintptr(p), uintptr(unsafe.Pointer(count)), uintptr(unsafe.Pointer(def)), 0)
			return int32(r)
		},
		set: func(thread, flavor uint32, p unsafe.Pointer, count uint32) int32 {
			r, _, _ := darwinSyscall6(fnRTPolicySet, uintptr(thread), uintptr(flavor), uintptr(p), uintptr(count), 0, 0)
			return int32(r)
		},
		release: func(thread uint32) {
			task, _, _ := darwinSyscall(fnRTTaskSelf, 0, 0, 0)
			darwinSyscall(fnRTPortDeallocate, task, uintptr(thread), 0)
		},
	})
}

func raiseDarwinPriority(period time.Duration, c darwinPriorityCalls) Grant {
	normal := Grant{Kind: "normal"}
	if period <= 0 || period > time.Second {
		return normal
	}
	numer, denom := c.timebase()
	if numer == 0 || denom == 0 {
		return normal
	}
	ticks := math.Round(float64(period) * float64(denom) / float64(numer))
	if ticks < 2 || ticks > float64(^uint32(0)) {
		return normal
	}
	thread := c.thread()
	if thread == 0 {
		return normal
	}
	var previous timeConstraintPolicy
	count, def := uint32(4), uint32(0)
	if c.get(thread, 2, unsafe.Pointer(&previous), &count, &def) != 0 || count != 4 {
		c.release(thread)
		return normal
	}
	timeshare := uint32(1)
	if def != 0 {
		n, d := uint32(1), uint32(0)
		if c.get(thread, 1, unsafe.Pointer(&timeshare), &n, &d) != 0 || n != 1 {
			c.release(thread)
			return normal
		}
	}
	p := timeConstraintPolicy{period: uint32(ticks), computation: uint32(ticks / 2), constraint: uint32(ticks), preemptible: 1}
	if c.set(thread, 2, unsafe.Pointer(&p), 4) != 0 {
		c.release(thread)
		return normal
	}
	return Grant{Kind: "coreaudio time-constraint", restore: func() {
		if def != 0 {
			c.set(thread, 1, unsafe.Pointer(&timeshare), 1)
		} else {
			c.set(thread, 2, unsafe.Pointer(&previous), 4)
		}
		c.release(thread)
	}}
}
