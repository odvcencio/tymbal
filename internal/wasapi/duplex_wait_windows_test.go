//go:build windows

package wasapi

import (
	"strings"
	"testing"
)

func TestDuplexWaitRejectsUncommittedPeriod(t *testing.T) {
	cases := []struct {
		name         string
		captureReady bool
		renderReady  bool
	}{
		{"capture pending", true, false},
		{"render pending", false, true},
		{"both pending", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := &captureStream{}
			capture.stager.ready = tc.captureReady
			render := &wasapiStream{ready: tc.renderReady}
			s := &duplexStream{capture: capture, render: render, started: true}
			err := s.Wait()
			if err == nil || !strings.Contains(err.Error(), "before committing the previous duplex period") {
				t.Fatalf("Wait() error = %v, want uncommitted-period error", err)
			}
		})
	}
}
