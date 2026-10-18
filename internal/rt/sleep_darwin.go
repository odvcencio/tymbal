//go:build darwin

package rt

import (
	"syscall"
	"time"
)

// SleepThread blocks the calling thread in select for about d. Unlike
// time.Sleep, the first wait cannot allocate a Go timer inside a stream's
// allocation-counting window. A signal can end the sleep early.
func SleepThread(d time.Duration) {
	if d <= 0 {
		return
	}
	timeout := syscall.NsecToTimeval(int64(d))
	_ = syscall.Select(0, nil, nil, nil, &timeout)
}
