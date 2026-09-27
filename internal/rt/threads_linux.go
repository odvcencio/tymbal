//go:build linux && (amd64 || arm64)

package rt

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// RaiseProcessThreads gives every thread of this process that runs the normal
// SCHED_OTHER policy the SCHED_FIFO priority p. It returns a function that
// puts back those threads and any thread that inherited p since, and the
// number of threads raised. Threads created later inherit the policy of the
// thread that creates them. A thread that already runs a real-time policy,
// such as a stream thread, is left alone.
//
// The stream thread takes Go runtime locks around its system calls, for
// example the scheduler lock when it wakes the runtime's monitor thread. If a
// normal-priority runtime thread holds such a lock when CPU load preempts it,
// the stream thread waits until that thread runs again, which can take
// milliseconds. Raising the other threads of an engine process removes that
// inversion. Choose p below the stream thread's priority.
//
// This is an opt-in engine-process tactic. Afterwards every goroutine in the
// process runs at a real-time priority, and the process's RLIMIT_RTTIME cap
// applies to all of its threads.
func RaiseProcessThreads(p int) (restore func(), raised int, err error) {
	return raiseProcessThreads(linuxThreadCalls{
		list:    listProcessThreads,
		getAttr: linuxGetThreadSchedAttr,
		setAttr: linuxSetThreadSchedAttr,
	}, p)
}

type linuxThreadCalls struct {
	list    func() ([]int, error)
	getAttr func(tid int, attr *linuxSchedAttr) error
	setAttr func(tid int, attr *linuxSchedAttr) error
}

func raiseProcessThreads(c linuxThreadCalls, p int) (func(), int, error) {
	if p < 1 || p > 99 {
		return func() {}, 0, fmt.Errorf("rt: invalid SCHED_FIFO priority %d", p)
	}
	size := uint32(unsafe.Sizeof(linuxSchedAttr{}))
	previous := make(map[int]linuxSchedAttr)
	var firstErr error
	// A thread created during a pass inherits its creator's policy. Repeat until
	// a pass raises nothing, so a thread started by an unraised thread is caught.
	for pass := 0; pass < 8; pass++ {
		tids, err := c.list()
		if err != nil {
			firstErr = err
			break
		}
		changed := false
		for _, tid := range tids {
			if _, done := previous[tid]; done {
				continue
			}
			attr := linuxSchedAttr{Size: size}
			if c.getAttr(tid, &attr) != nil || attr.Policy != 0 {
				continue // the thread exited, or it already has a real-time policy
			}
			next := linuxSchedAttr{Size: size, Policy: linuxSchedFIFO, Priority: uint32(p)}
			if err := c.setAttr(tid, &next); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			previous[tid] = attr
			changed = true
		}
		if !changed {
			break
		}
	}
	restore := func() {
		tids, err := c.list()
		if err != nil {
			return
		}
		for _, tid := range tids {
			attr := linuxSchedAttr{Size: size}
			if c.getAttr(tid, &attr) != nil || attr.Policy != linuxSchedFIFO || attr.Priority != uint32(p) || attr.Flags&linuxResetOnFork != 0 {
				continue
			}
			back, ok := previous[tid]
			if !ok {
				back = linuxSchedAttr{Size: size} // inherited p after the raise
			}
			_ = c.setAttr(tid, &back)
		}
	}
	return restore, len(previous), firstErr
}

func listProcessThreads() ([]int, error) {
	entries, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil, err
	}
	tids := make([]int, 0, len(entries))
	for _, e := range entries {
		if tid, err := strconv.Atoi(e.Name()); err == nil {
			tids = append(tids, tid)
		}
	}
	return tids, nil
}

func linuxGetThreadSchedAttr(tid int, attr *linuxSchedAttr) error {
	_, get := linuxSchedSyscalls()
	_, _, errno := syscall.RawSyscall6(get, uintptr(tid), uintptr(unsafe.Pointer(attr)), unsafe.Sizeof(*attr), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func linuxSetThreadSchedAttr(tid int, attr *linuxSchedAttr) error {
	set, _ := linuxSchedSyscalls()
	_, _, errno := syscall.RawSyscall(set, uintptr(tid), uintptr(unsafe.Pointer(attr)), 0)
	if errno != 0 {
		return errno
	}
	return nil
}
