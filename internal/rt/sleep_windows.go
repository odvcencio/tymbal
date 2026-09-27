package rt

import (
	"syscall"
	"time"
)

var procSleep = syscall.NewLazyDLL("kernel32.dll").NewProc("Sleep")

func init() {
	// Resolve now, so the first SleepThread call does not allocate.
	_ = procSleep.Find()
}

// SleepThread blocks the calling thread in kernel32 Sleep for about d, rounded
// up to whole milliseconds; the system timer can round it up further. Unlike
// time.Sleep, it touches no Go timer or channel, so it allocates nothing.
func SleepThread(d time.Duration) {
	if d <= 0 {
		return
	}
	ms := (d + time.Millisecond - 1) / time.Millisecond
	_, _, _ = syscall.Syscall(procSleep.Addr(), 1, uintptr(ms), 0, 0)
}
