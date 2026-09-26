//go:build windows

package rt

import (
	"errors"
	"runtime"
	"testing"
	"time"
)

type fakeMMCSSAPI struct {
	loadErr     error
	thread      uintptr
	loadCalls   int
	setCalls    int
	revertCalls int
	lastTask    uint16
	lastIndex   uint32
	lastThread  uintptr
}

func (f *fakeMMCSSAPI) load() error {
	f.loadCalls++
	return f.loadErr
}

func (f *fakeMMCSSAPI) setCharacteristics(taskName *uint16, taskIndex *uint32) uintptr {
	f.setCalls++
	if taskName != nil {
		f.lastTask = *taskName
	}
	if taskIndex != nil {
		*taskIndex = 7
	}
	f.lastIndex = 7
	return f.thread
}

func (f *fakeMMCSSAPI) revert(thread uintptr) bool {
	f.revertCalls++
	f.lastThread = thread
	return true
}

func TestRaisePriorityFallsBackOnMMCSSFailure(t *testing.T) {
	t.Run("load failure", func(t *testing.T) {
		api := &fakeMMCSSAPI{loadErr: errors.New("avrt unavailable")}
		grant := raisePriorityWithAPI(time.Millisecond, api)
		if grant.Kind != "normal" || grant.Priority != 0 || grant.restore != nil {
			t.Fatalf("grant = %+v, want normal", grant)
		}
		if api.loadCalls != 1 || api.setCalls != 0 || api.revertCalls != 0 {
			t.Fatalf("API calls after load failure = %+v", api)
		}
	})

	t.Run("characteristics failure", func(t *testing.T) {
		api := &fakeMMCSSAPI{}
		grant := raisePriorityWithAPI(time.Millisecond, api)
		if grant.Kind != "normal" || grant.Priority != 0 || grant.restore != nil {
			t.Fatalf("grant = %+v, want normal", grant)
		}
		if api.loadCalls != 1 || api.setCalls != 1 || api.revertCalls != 0 {
			t.Fatalf("API calls after characteristics failure = %+v", api)
		}
	})
}

func TestRaisePrioritySuccessRevertsOnLower(t *testing.T) {
	api := &fakeMMCSSAPI{thread: 77}
	grant := raisePriorityWithAPI(time.Millisecond, api)
	if grant.Kind != "MMCSS Pro Audio" || grant.Priority != 0 || grant.restore == nil {
		t.Fatalf("grant = %+v, want MMCSS Pro Audio", grant)
	}
	if api.lastTask != 'P' || api.lastIndex != 7 || api.revertCalls != 0 {
		t.Fatalf("API calls before Lower = %+v, want Pro Audio task and no revert", api)
	}
	Lower(grant)
	if api.revertCalls != 1 || api.lastThread != 77 {
		t.Fatalf("API calls after Lower = %+v, want one revert of 77", api)
	}
}

func TestNativeRaisePriorityAndLowerOnLockedThread(t *testing.T) {
	type result struct {
		grant Grant
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		grant := Raise(10 * time.Millisecond)
		Lower(grant)
		done <- result{grant: grant}
	}()

	select {
	case got := <-done:
		if got.grant.String() == "" {
			t.Fatal("Raise returned an empty priority report")
		}
		t.Logf("priority grant: %s", got.grant.String())
	case <-time.After(3 * time.Second):
		t.Fatal("native MMCSS Raise/Lower timed out")
	}
}
