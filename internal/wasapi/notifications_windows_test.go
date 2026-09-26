//go:build windows

package wasapi

import (
	"runtime"
	"testing"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
)

func TestDiffEndpointSnapshots(t *testing.T) {
	previous := []driver.Info{
		{ID: "render-a", Name: "Speakers", Outputs: 2, SampleRates: []int{48000}, Default: 1},
		{ID: "render-b", Name: "Headset", Outputs: 1, SampleRates: []int{48000}},
		{ID: "capture-a", Name: "Microphone", Inputs: 1, SampleRates: []int{48000}, Default: 2},
	}
	current := []driver.Info{
		{ID: "render-a", Name: "Speakers", Outputs: 2, SampleRates: []int{48000}},
		{ID: "render-b", Name: "Headset", Outputs: 2, SampleRates: []int{96000}, Default: 1},
		{ID: "capture-b", Name: "USB Microphone", Inputs: 2, SampleRates: []int{44100}, Default: 2},
	}

	events := diffEndpointSnapshots(previous, current)
	if len(events) != 5 {
		t.Fatalf("got %d events, want 5: %+v", len(events), events)
	}
	want := []struct {
		kind uint8
		id   string
	}{
		{eventDeviceRemoved, "capture-a"},
		{eventDeviceAdded, "capture-b"},
		{eventFormatChanged, "render-b"},
		{eventDefaultChanged, "render-b"},
		{eventDefaultChanged, "capture-b"},
	}
	for i := range want {
		if events[i].Kind != want[i].kind || events[i].Device.ID != want[i].id {
			t.Errorf("event %d = kind %d device %q, want kind %d device %q", i,
				events[i].Kind, events[i].Device.ID, want[i].kind, want[i].id)
		}
	}
}

func TestNotificationQueueOverflowCoalescesIntoFullSnapshot(t *testing.T) {
	queue := newNotificationQueue()
	if !queue.signal() {
		t.Fatal("first notification should be queued")
	}
	for i := 0; i < 100; i++ {
		if queue.signal() {
			t.Fatal("bounded queue unexpectedly accepted a second pending wake")
		}
	}
	if got := len(queue.wake); got != 1 {
		t.Fatalf("pending wake count = %d, want 1", got)
	}
	<-queue.wake

	// A single queued wake causes the owner to enumerate the complete current
	// endpoint set, so notifications dropped while the queue was full remain
	// discoverable in the reconciliation diff.
	current := []driver.Info{
		{ID: "render-a", Outputs: 2, SampleRates: []int{48000}},
		{ID: "capture-a", Inputs: 1, SampleRates: []int{44100}},
	}
	events := diffEndpointSnapshots(nil, current)
	if len(events) != len(current) {
		t.Fatalf("full-snapshot reconciliation returned %d events, want %d", len(events), len(current))
	}
	for _, event := range events {
		if event.Kind != eventDeviceAdded {
			t.Errorf("event kind = %d, want DeviceAdded", event.Kind)
		}
	}
}

func TestWatchStopFromCallbackDoesNotJoinCallback(t *testing.T) {
	w := &endpointWatch{
		stopLoop:     make(chan struct{}),
		dispatchStop: make(chan struct{}),
		ownerDone:    make(chan struct{}),
		dispatchWake: make(chan struct{}, 1),
		dispatchDone: make(chan struct{}),
	}
	go func() {
		<-w.stopLoop
		close(w.ownerDone)
	}()

	callbackReturned := make(chan struct{})
	w.fn = func(driver.Event) {
		w.stop()
		close(callbackReturned)
	}
	go w.dispatch(nil)
	w.publish([]driver.Info{{ID: "render-a", Outputs: 2, SampleRates: []int{48000}}})

	select {
	case <-w.dispatchDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stop deadlocked when called from its user callback")
	}
	select {
	case <-callbackReturned:
	default:
		t.Fatal("callback did not return from stop")
	}
	if w.dispatchEvent(driver.Event{Kind: eventDeviceRemoved}) {
		t.Fatal("callback admission succeeded after stop")
	}
	w.stop()
}

func TestNativeWatchStartStopLifecycle(t *testing.T) {
	stop := watchEndpoints(func(driver.Event) {})
	done := make(chan struct{})
	go func() {
		stop()
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("native Watch cancellation did not finish")
	}
}

func TestNativeNotificationRegisterUnregister(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := initializeMTA(); err != nil {
		t.Skipf("COM initialization is unavailable: %v", err)
	}
	defer uninitializeCOM()

	enumerator, err := createEnumerator()
	if err != nil {
		t.Skipf("MMDeviceEnumerator is unavailable in this Windows environment: %v", err)
	}
	defer release(enumerator)

	w := &endpointWatch{queue: newNotificationQueue()}
	client, err := newNotificationClient(w)
	if err != nil {
		t.Fatal(err)
	}
	registered := false
	defer func() {
		if registered {
			err := checkHRESULT("IMMDeviceEnumerator.UnregisterEndpointNotificationCallback(test cleanup)",
				comCall1(enumerator, immDeviceEnumeratorUnregisterNotificationCallback, client))
			if err != nil && !isNotFound(err) {
				t.Errorf("cleanup unregister failed; retaining callback object to avoid a dangling registration: %v", err)
				return
			}
		}
		release(client)
	}()

	if err := checkHRESULT("IMMDeviceEnumerator.RegisterEndpointNotificationCallback(test)",
		comCall1(enumerator, immDeviceEnumeratorRegisterNotificationCallback, client)); err != nil {
		t.Skipf("Core Audio endpoint notifications are unavailable: %v", err)
	}
	registered = true
	if err := checkHRESULT("IMMDeviceEnumerator.UnregisterEndpointNotificationCallback(test)",
		comCall1(enumerator, immDeviceEnumeratorUnregisterNotificationCallback, client)); err != nil {
		t.Fatalf("unregister endpoint notification callback: %v", err)
	}
	registered = false
}

func TestNotificationClientQueryInterfaceABI(t *testing.T) {
	w := &endpointWatch{queue: newNotificationQueue()}
	client, err := newNotificationClient(w)
	if err != nil {
		t.Fatal(err)
	}
	defer release(client)

	var out uintptr
	if hr := comCall2(client, 0, uintptr(unsafe.Pointer(&iidIMMNotificationClient)), uintptr(unsafe.Pointer(&out))); int32(hr) < 0 {
		t.Fatalf("QueryInterface(IMMNotificationClient) HRESULT = 0x%08X", uint32(hr))
	}
	if out != client {
		t.Fatalf("QueryInterface returned %#x, want %#x", out, client)
	}
	if got := notificationRelease(out); got != 1 {
		t.Fatalf("Release count = %d, want 1", got)
	}
	if got := notificationAddRef(client); got != 2 {
		t.Fatalf("AddRef count = %d, want 2", got)
	}
	if got := notificationRelease(client); got != 1 {
		t.Fatalf("Release count = %d, want 1", got)
	}
}
