//go:build windows

package wasapi

import (
	"fmt"
	"math"
	"runtime"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
)

const (
	audioClientGetStreamLatency = 5
	nanosecondsPerHNS           = 100
)

// getStreamLatency returns the latency reported by IAudioClient in nanoseconds.
// It must be called on the thread that owns client, after Initialize succeeds.
func getStreamLatency(client uintptr) (time.Duration, error) {
	if client == 0 {
		return 0, fmt.Errorf("%w: nil IAudioClient for GetStreamLatency", driver.ErrFormat)
	}

	var latencyHNS int64
	hr := comCall1(client, audioClientGetStreamLatency, uintptr(unsafe.Pointer(&latencyHNS)))
	runtime.KeepAlive(&latencyHNS)
	if err := checkHRESULT("IAudioClient.GetStreamLatency", hr); err != nil {
		return 0, err
	}
	return streamLatencyFromHNS(latencyHNS)
}

func streamLatencyFromHNS(latencyHNS int64) (time.Duration, error) {
	if latencyHNS < 0 || latencyHNS > math.MaxInt64/nanosecondsPerHNS {
		return 0, fmt.Errorf("%w: invalid IAudioClient stream latency %d HNS", driver.ErrFormat, latencyHNS)
	}
	return time.Duration(latencyHNS * nanosecondsPerHNS), nil
}
