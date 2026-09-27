//go:build !linux

package rt

import "time"

// SleepThread sleeps for about d. On this platform it uses time.Sleep, whose
// runtime timer can allocate the first time a goroutine sleeps on a P.
func SleepThread(d time.Duration) { time.Sleep(d) }
