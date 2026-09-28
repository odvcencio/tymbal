package tymbal

import (
	"errors"

	"m31labs.dev/tymbal/internal/rt"
)

// RaiseProcessThreads gives every normal-priority thread of this process the
// real-time SCHED_FIFO priority p (1 to 99) and returns the number of threads
// it raised. The returned function puts them back. Choose p below the priority
// of the stream thread (Tymbal uses 70 on Linux).
//
// An engine process calls this once, before it opens a stream, when CPU load
// makes the Go runtime's other threads preempt the stream thread while it
// holds a runtime lock. It is opt-in and needs the same privilege as a
// real-time stream (an rtprio limit or RealtimeKit). On hosts other than
// Linux amd64 and arm64 it does nothing and returns ErrUnsupported. If some
// threads could not be raised, it returns the count it raised and the first
// error; call the returned function either way.
func RaiseProcessThreads(p int) (restore func(), raised int, err error) {
	restore, raised, err = rt.RaiseProcessThreads(p)
	if errors.Is(err, rt.ErrUnsupported) {
		err = ErrUnsupported
	}
	if restore == nil {
		restore = func() {}
	}
	return restore, raised, err
}
