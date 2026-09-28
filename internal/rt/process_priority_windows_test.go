//go:build windows

package rt

import (
	"errors"
	"testing"
)

type fakeProcessPriorityAPI struct {
	class   uint32
	setErr  error
	setCall []uint32
}

func (f *fakeProcessPriorityAPI) currentClass() (uint32, error) { return f.class, nil }

func (f *fakeProcessPriorityAPI) setClass(class uint32) error {
	f.setCall = append(f.setCall, class)
	if f.setErr != nil {
		return f.setErr
	}
	f.class = class
	return nil
}

func TestRaiseProcessPriorityRaisesAndRestores(t *testing.T) {
	api := &fakeProcessPriorityAPI{class: normalPriorityClass}
	restore, priority, err := raiseProcessPriority(api)
	if err != nil || priority != "HIGH_PRIORITY_CLASS" || api.class != highPriorityClass {
		t.Fatalf("raise = (%q, %v), class=0x%X; want high class", priority, err, api.class)
	}
	if err := restore(); err != nil || api.class != normalPriorityClass {
		t.Fatalf("restore = %v, class=0x%X; want normal class", err, api.class)
	}
	if err := restore(); err != nil || len(api.setCall) != 2 {
		t.Fatalf("second restore = %v, set calls=%v; want idempotent restore", err, api.setCall)
	}
}

func TestRaiseProcessPriorityPreservesExistingRealtimeClass(t *testing.T) {
	api := &fakeProcessPriorityAPI{class: realtimePriorityClass}
	restore, priority, err := raiseProcessPriority(api)
	if err != nil || priority != "REALTIME_PRIORITY_CLASS (unchanged)" || len(api.setCall) != 0 {
		t.Fatalf("raise = (%q, %v), set calls=%v; want unchanged", priority, err, api.setCall)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
}

func TestRaiseProcessPriorityReportsSetFailure(t *testing.T) {
	wantErr := errors.New("denied")
	api := &fakeProcessPriorityAPI{class: normalPriorityClass, setErr: wantErr}
	_, priority, err := raiseProcessPriority(api)
	if !errors.Is(err, wantErr) || priority != "" {
		t.Fatalf("raise = (%q, %v), want set error", priority, err)
	}
}
