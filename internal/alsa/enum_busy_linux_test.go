//go:build linux && (amd64 || arm64)

package alsa

import (
	"syscall"
	"testing"
)

func TestBusyPCMDoesNotHideOtherDevices(t *testing.T) {
	b := &backend{}
	endpoints := []endpoint{
		{cardNumber: 0, cardID: "first", pcmName: "First", playback: true},
		{cardNumber: 1, cardID: "second", pcmName: "Second", playback: true, capture: true},
	}
	busy := false
	probe := func(path string) (pcmCapability, error) {
		if path == endpoints[0].pcmPath(false) && busy {
			return pcmCapability{}, syscall.EBUSY
		}
		return pcmCapability{channels: 2, minPeriod: 64, maxPeriod: 1024, rates: []int{48000}}, nil
	}
	if _, err := b.devicesWithProbe(endpoints, probe); err != nil {
		t.Fatal(err)
	}
	busy = true
	devices, err := b.devicesWithProbe(endpoints, probe)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 || devices[0].Outputs != 2 || devices[0].MinPeriod != 64 || len(devices[0].SampleRates) != 1 || devices[1].Inputs != 2 {
		t.Fatalf("busy enumeration lost cached or available capabilities: %+v", devices)
	}
}

func TestBusyPCMWithoutCacheRetainsDirection(t *testing.T) {
	b := &backend{}
	devices, err := b.devicesWithProbe([]endpoint{{cardID: "busy", playback: true, capture: true}}, func(string) (pcmCapability, error) {
		return pcmCapability{}, syscall.EBUSY
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].Inputs != 1 || devices[0].Outputs != 1 || len(devices[0].SampleRates) != 0 || devices[0].MinPeriod != 0 || devices[0].MaxPeriod != 0 {
		t.Fatalf("busy endpoint metadata = %+v", devices)
	}
}
