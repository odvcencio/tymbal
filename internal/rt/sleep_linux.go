//go:build linux

package rt

import (
	"syscall"
	"time"
)

// SleepThread blocks the calling thread in nanosleep for about d. Unlike
// time.Sleep, it touches no Go timer or channel, so it allocates nothing. A
// signal can end the sleep early.
func SleepThread(d time.Duration) {
	if d <= 0 {
		return
	}
	ts := syscall.NsecToTimespec(int64(d))
	_ = syscall.Nanosleep(&ts, nil)
}
