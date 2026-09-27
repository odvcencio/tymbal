//go:build linux && (amd64 || arm64)

package rt

import (
	"errors"
	"syscall"
	"testing"
	"unsafe"
)

type threadFake struct {
	attrs     map[int]linuxSchedAttr
	order     []int
	onSet     func(tid int)
	setErr    error
	setCalled []int
}

func (f *threadFake) calls() linuxThreadCalls {
	return linuxThreadCalls{
		list: func() ([]int, error) { return append([]int(nil), f.order...), nil },
		getAttr: func(tid int, attr *linuxSchedAttr) error {
			a, ok := f.attrs[tid]
			if !ok {
				return syscall.ESRCH
			}
			*attr = a
			return nil
		},
		setAttr: func(tid int, attr *linuxSchedAttr) error {
			f.setCalled = append(f.setCalled, tid)
			if f.setErr != nil {
				return f.setErr
			}
			f.attrs[tid] = *attr
			if f.onSet != nil {
				f.onSet(tid)
			}
			return nil
		},
	}
}

func (f *threadFake) add(tid int, a linuxSchedAttr) {
	f.attrs[tid] = a
	f.order = append(f.order, tid)
}

func TestRaiseProcessThreadsRaisesOnlyNormalThreadsAndRestores(t *testing.T) {
	size := uint32(unsafe.Sizeof(linuxSchedAttr{}))
	f := &threadFake{attrs: map[int]linuxSchedAttr{}}
	f.add(1, linuxSchedAttr{Size: size, Nice: 3})
	f.add(2, linuxSchedAttr{Size: size, Policy: linuxSchedFIFO, Priority: 70, Flags: linuxResetOnFork}) // stream thread
	f.add(3, linuxSchedAttr{Size: size})
	f.add(6, linuxSchedAttr{Size: size, Policy: linuxSchedFIFO, Priority: 50}) // an existing real-time thread at p
	// An unraised thread starts thread 4 during the first pass.
	f.onSet = func(tid int) {
		if tid == 3 {
			f.add(4, linuxSchedAttr{Size: size})
			f.onSet = nil
		}
	}
	restore, raised, err := raiseProcessThreads(f.calls(), 50)
	if err != nil || raised != 3 {
		t.Fatalf("raised=%d err=%v, want 3 threads and no error", raised, err)
	}
	for _, tid := range []int{1, 3, 4} {
		if a := f.attrs[tid]; a.Policy != linuxSchedFIFO || a.Priority != 50 || a.Flags&linuxResetOnFork != 0 {
			t.Fatalf("thread %d attr = %+v, want SCHED_FIFO 50 without reset-on-fork", tid, a)
		}
	}
	if a := f.attrs[2]; a.Priority != 70 {
		t.Fatalf("stream thread changed: %+v", a)
	}
	// Thread 5 inherits the raised policy after the raise.
	f.add(5, linuxSchedAttr{Size: size, Policy: linuxSchedFIFO, Priority: 50})
	restore()
	if a := f.attrs[1]; a.Policy != 0 || a.Nice != 3 {
		t.Fatalf("thread 1 not restored: %+v", a)
	}
	for _, tid := range []int{3, 4, 5} {
		if a := f.attrs[tid]; a.Policy != 0 || a.Priority != 0 {
			t.Fatalf("thread %d not restored: %+v", tid, a)
		}
	}
	if a := f.attrs[2]; a.Policy != linuxSchedFIFO || a.Priority != 70 {
		t.Fatalf("restore touched the stream thread: %+v", a)
	}
	if a := f.attrs[6]; a.Policy != linuxSchedFIFO || a.Priority != 50 {
		t.Fatalf("restore demoted a thread that was real-time before the raise: %+v", a)
	}
}

func TestRaiseProcessThreadsReportsPermissionAndBadPriority(t *testing.T) {
	f := &threadFake{attrs: map[int]linuxSchedAttr{}, setErr: syscall.EPERM}
	f.add(1, linuxSchedAttr{Size: uint32(unsafe.Sizeof(linuxSchedAttr{}))})
	if _, raised, err := raiseProcessThreads(f.calls(), 50); !errors.Is(err, syscall.EPERM) || raised != 0 {
		t.Fatalf("raised=%d err=%v, want EPERM and none raised", raised, err)
	}
	for _, p := range []int{0, 100} {
		if _, _, err := raiseProcessThreads(f.calls(), p); err == nil {
			t.Fatalf("priority %d accepted", p)
		}
	}
}

func TestRaiseProcessThreadsOnThisProcess(t *testing.T) {
	restore, raised, err := RaiseProcessThreads(1)
	if errors.Is(err, syscall.EPERM) && raised == 0 {
		t.Skip("real-time scheduling is not permitted here")
	}
	if err != nil || raised == 0 {
		restore()
		t.Fatalf("raised=%d err=%v", raised, err)
	}
	attr := linuxSchedAttr{Size: uint32(unsafe.Sizeof(linuxSchedAttr{}))}
	if err := linuxGetThreadSchedAttr(syscall.Gettid(), &attr); err != nil || attr.Policy != linuxSchedFIFO || attr.Priority != 1 {
		restore()
		t.Fatalf("calling thread attr = %+v, err %v", attr, err)
	}
	restore()
	if err := linuxGetThreadSchedAttr(syscall.Gettid(), &attr); err != nil || attr.Policy != 0 {
		t.Fatalf("calling thread not restored: %+v, err %v", attr, err)
	}
}
