//go:build windows

package wasapi

import (
	"errors"
	"testing"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
)

func TestWindowsCoreAudioABILayout(t *testing.T) {
	if got := unsafe.Sizeof(guid{}); got != 16 {
		t.Fatalf("GUID size = %d, want 16", got)
	}
	if got := unsafe.Sizeof(propVariant{}); got != 24 {
		t.Fatalf("PROPVARIANT size = %d, want 24", got)
	}
	if got := unsafe.Offsetof(waveFormatEx{}.samplesPerSec); got != 4 {
		t.Fatalf("WAVEFORMATEX.nSamplesPerSec offset = %d, want 4", got)
	}
	if got := unsafe.Offsetof(waveFormatEx{}.cbSize); got != 16 {
		t.Fatalf("WAVEFORMATEX.cbSize offset = %d, want 16", got)
	}
}

func TestWindowsEndpointEnumeration(t *testing.T) {
	h := New()
	devices, err := h.Devices()
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) == 0 {
		t.Skip("Windows exposes no active audio endpoints in this environment")
	}
	t.Logf("enumerated %d active endpoint(s)", len(devices))

	seen := make(map[string]driver.Info, len(devices))
	for _, device := range devices {
		if device.ID == "" || device.Name == "" {
			t.Fatalf("endpoint has empty ID or name: %+v", device)
		}
		if (device.Inputs > 0) == (device.Outputs > 0) {
			t.Fatalf("endpoint should describe exactly one data flow: %+v", device)
		}
		if len(device.SampleRates) != 1 || device.SampleRates[0] <= 0 {
			t.Fatalf("endpoint has invalid mix-rate metadata: %+v", device)
		}
		seen[device.ID] = device
	}

	for _, dir := range []uint8{1, 2} {
		foundFlow := false
		for _, device := range devices {
			if dir == 1 && device.Outputs > 0 || dir == 2 && device.Inputs > 0 {
				foundFlow = true
				break
			}
		}
		if !foundFlow {
			continue
		}
		device, err := h.Default(dir)
		if err != nil {
			if isNotFound(err) {
				t.Logf("Default(%d): no endpoint is assigned to the console role", dir)
				continue
			}
			t.Errorf("Default(%d): %v", dir, err)
			continue
		}
		if device.Default != dir || device.ID == "" {
			t.Errorf("Default(%d) returned invalid metadata: %+v", dir, device)
		}
		if _, ok := seen[device.ID]; !ok {
			t.Errorf("Default(%d) returned endpoint %q absent from active enumeration", dir, device.ID)
		}
	}
}

func TestOpenRejectsMissingRenderDirection(t *testing.T) {
	if _, err := New().Open(driver.Request{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("Open error = %v, want %v", err, driver.ErrUnsupported)
	}
}

func TestDeviceInvalidationMapsToDriverLost(t *testing.T) {
	err := checkHRESULT("test", audclntErrDeviceInvalidated)
	if !errors.Is(err, driver.ErrLost) {
		t.Fatalf("device invalidation error = %v, want %v", err, driver.ErrLost)
	}
}
