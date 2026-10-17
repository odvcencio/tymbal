//go:build darwin && (amd64 || arm64)

package rt

// The Mach calls behind the macOS thread priority (priority_darwin.go), reached
// the way internal/coreaudio reaches Core Audio: load-time imports, one
// assembly trampoline per symbol, and the runtime's syscall.syscall gateways,
// with no cgo and no run-time loader. bind_darwin.go in that package explains
// the technique and why it needs neither runtime.cgocall nor iscgo.
//
// This is a second copy of the pattern rather than a shared one because
// internal/coreaudio imports this package, for Now, so it cannot be imported
// back. Every symbol here lives in libSystem, which each Go program on macOS
// already links, so this file adds no framework to the binary.

import (
	"syscall"
	"unsafe"
)

// Load-time imports. The first field is the symbol's link-time name, kept
// unique to this package; the second is the C name; the third is the install
// name. The names are long-standing public API (<pthread.h>, <mach/mach_time.h>
// and <mach/thread_act.h>).
//
//go:cgo_import_dynamic tymbal_rt_pthread_self pthread_self "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic tymbal_rt_pthread_mach_thread_np pthread_mach_thread_np "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic tymbal_rt_mach_timebase_info mach_timebase_info "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic tymbal_rt_thread_policy_set thread_policy_set "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic tymbal_rt_thread_policy_get thread_policy_get "/usr/lib/libSystem.B.dylib"

// fn* hold the address of each import's trampoline. bind_darwin.s defines
// them with DATA directives; Go never assigns them.
var (
	fnpthread_self           uintptr
	fnpthread_mach_thread_np uintptr
	fnmach_timebase_info     uintptr
	fnthread_policy_set      uintptr
	fnthread_policy_get      uintptr
)

// Runtime gateways: switch to the thread's system stack, call fn with the C
// ABI, and bracket the call with entersyscall and exitsyscall. The declarations
// have no body, so a pointer passed as uintptr(unsafe.Pointer(p)) stays alive
// for the whole call; the callees here do not retain it.

//go:noescape
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:noescape
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:linkname syscall_syscall syscall.syscall
//go:linkname syscall_syscall6 syscall.syscall6

// pthreadSelf is pthread_t pthread_self(void): the calling OS thread. The
// stream core locks its goroutine to one, so this names the thread the
// priority applies to.
func pthreadSelf() uintptr {
	r, _, _ := syscall_syscall(fnpthread_self, 0, 0, 0)
	return r
}

// pthreadMachThread is mach_port_t pthread_mach_thread_np(pthread_t): the
// thread's own Mach port, the one the kernel's thread calls take. Unlike
// mach_thread_self it adds no send right, so there is nothing to release.
func pthreadMachThread(thread uintptr) uint32 {
	r, _, _ := syscall_syscall(fnpthread_mach_thread_np, thread, 0, 0)
	return uint32(r)
}

// machTimebase is mach_timebase_info_data_t. One tick of mach_absolute_time,
// the unit of every time in a Mach scheduling policy, lasts numer/denom
// nanoseconds.
type machTimebase struct{ numer, denom uint32 }

// machTimebaseInfo is kern_return_t mach_timebase_info(mach_timebase_info_t).
func machTimebaseInfo(tb *machTimebase) int32 {
	r, _, _ := syscall_syscall(fnmach_timebase_info, uintptr(unsafe.Pointer(tb)), 0, 0)
	return int32(r)
}

// timeConstraint is thread_time_constraint_policy_data_t, the policy behind
// THREAD_TIME_CONSTRAINT_POLICY. The times are in mach_absolute_time ticks.
type timeConstraint struct {
	period      uint32
	computation uint32
	constraint  uint32
	preemptible int32 // boolean_t
}

// threadPolicySet is kern_return_t thread_policy_set(thread_act_t,
// thread_policy_flavor_t, thread_policy_t, mach_msg_type_number_t). The count
// is in 32-bit integers.
func threadPolicySet(thread, flavor uint32, policy unsafe.Pointer, count uint32) int32 {
	r, _, _ := syscall_syscall6(fnthread_policy_set, uintptr(thread), uintptr(flavor), uintptr(policy), uintptr(count), 0, 0)
	return int32(r)
}

// threadPolicyGet is kern_return_t thread_policy_get(thread_inspect_t,
// thread_policy_flavor_t, thread_policy_t, mach_msg_type_number_t *,
// boolean_t *get_default). count is the capacity of policy in integers on the
// way in and the number written on the way out. get_default is the way to ask
// whether the thread has the policy at all: with *getDefault false on entry, the
// kernel sets it to true when the thread does not.
func threadPolicyGet(thread, flavor uint32, policy unsafe.Pointer, count *uint32, getDefault *int32) int32 {
	r, _, _ := syscall_syscall6(fnthread_policy_get, uintptr(thread), uintptr(flavor), uintptr(policy),
		uintptr(unsafe.Pointer(count)), uintptr(unsafe.Pointer(getDefault)), 0)
	return int32(r)
}
