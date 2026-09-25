package tymbaltest

import "m31labs.dev/tymbal"

// FakeConfig and FakeControl are aliases to the stream core's deterministic
// test host surface. They are re-exported here for callers that want all test
// tools under tymbaltest.
type FakeConfig = tymbal.FakeConfig
type FakeControl = tymbal.FakeControl

// NewFakeHost constructs Tymbal's virtual host and its fault controller.
func NewFakeHost(cfg FakeConfig) (tymbal.Host, *FakeControl) {
	return tymbal.NewFakeHost(cfg)
}
