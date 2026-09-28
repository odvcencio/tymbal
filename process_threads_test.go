package tymbal_test

import (
	"testing"

	"m31labs.dev/tymbal"
)

func TestRaiseProcessThreadsPublicAPIRejectsInvalidPriority(t *testing.T) {
	restore, raised, err := tymbal.RaiseProcessThreads(0)
	if restore == nil || err == nil || raised != 0 {
		t.Fatalf("RaiseProcessThreads(0) = (restore nil=%t, raised=%d, err=%v), want a no-op restore, no threads, and an error", restore == nil, raised, err)
	}
	restore()
}
