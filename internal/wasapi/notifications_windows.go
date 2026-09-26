//go:build windows

package wasapi

import (
	"errors"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
)

const (
	coinitMultithreaded = 0

	immDeviceEnumeratorRegisterNotificationCallback   = 6
	immDeviceEnumeratorUnregisterNotificationCallback = 7

	watchQueueSize = 1

	eNoInterface = 0x80004002
	ePointer     = 0x80004003
)

var (
	iidIUnknown              = guid{data4: [8]byte{0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	iidIMMNotificationClient = guid{
		data1: 0x7991eec9,
		data2: 0x7e89,
		data3: 0x4d85,
		data4: [8]byte{0x83, 0x90, 0x6c, 0x70, 0x3c, 0xec, 0x60, 0xc0},
	}

	procCoTaskMemAlloc = ole32.NewProc("CoTaskMemAlloc")

	immNotificationClientVTable = [8]uintptr{
		syscall.NewCallback(notificationQueryInterface),
		syscall.NewCallback(notificationAddRef),
		syscall.NewCallback(notificationRelease),
		syscall.NewCallback(notificationOnDeviceStateChanged),
		syscall.NewCallback(notificationOnDeviceAdded),
		syscall.NewCallback(notificationOnDeviceRemoved),
		syscall.NewCallback(notificationOnDefaultDeviceChanged),
		syscall.NewCallback(notificationOnPropertyValueChanged),
	}

	notificationRegistryMu sync.Mutex
	notificationRegistry   atomic.Pointer[map[uintptr]*endpointWatch]
)

// notificationObject lives in CoTaskMem. It contains no Go pointers: callbacks
// find their Go state in an immutable, atomically published registry instead.
type notificationObject struct {
	vtable uintptr
	refs   uint32
	pad    uint32
}

type notificationQueue struct {
	wake chan struct{}
}

func newNotificationQueue() notificationQueue {
	return notificationQueue{wake: make(chan struct{}, watchQueueSize)}
}

// signal is nonblocking. A full queue already contains a request for a full
// endpoint snapshot, which reconciles every notification coalesced into it.
func (q notificationQueue) signal() bool {
	select {
	case q.wake <- struct{}{}:
		return true
	default:
		return false
	}
}

type endpointWatch struct {
	fn func(driver.Event)

	queue notificationQueue

	ready        chan watchSetup
	startLoop    chan struct{}
	stopLoop     chan struct{}
	dispatchStop chan struct{}
	ownerDone    chan struct{}
	dispatchWake chan struct{}
	dispatchDone chan struct{}

	snapshotMu sync.Mutex
	latest     []driver.Info

	dispatchState atomic.Uint32

	stopOnce sync.Once
}

type watchSetup struct {
	baseline   []driver.Info
	current    []driver.Info
	registered bool
	setupErr   error
}

func init() {
	empty := make(map[uintptr]*endpointWatch)
	notificationRegistry.Store(&empty)
}

func watchEndpoints(fn func(driver.Event)) func() {
	if fn == nil {
		return func() {}
	}

	w := &endpointWatch{
		fn:           fn,
		queue:        newNotificationQueue(),
		ready:        make(chan watchSetup, 1),
		startLoop:    make(chan struct{}),
		stopLoop:     make(chan struct{}),
		dispatchStop: make(chan struct{}),
		ownerDone:    make(chan struct{}),
		dispatchWake: make(chan struct{}, 1),
		dispatchDone: make(chan struct{}),
	}
	go w.runCOMOwner()

	setup := <-w.ready
	if !setup.registered {
		<-w.ownerDone
		return func() {}
	}

	go w.dispatch(setup.baseline)
	w.publish(setup.current)
	close(w.startLoop)
	return w.stop
}

func (w *endpointWatch) runCOMOwner() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(w.ownerDone)

	if err := initializeMTA(); err != nil {
		w.ready <- watchSetup{setupErr: err}
		return
	}
	defer uninitializeCOM()

	enumerator, err := createEnumerator()
	if err != nil {
		w.ready <- watchSetup{setupErr: err}
		return
	}
	defer release(enumerator)

	baseline, baselineErr := endpointSnapshot()
	if baselineErr != nil {
		// Keep watching even if the initial enumeration fails. A later full
		// rescan will make currently active endpoints discoverable.
		baseline = nil
	}

	client, err := newNotificationClient(w)
	if err != nil {
		w.ready <- watchSetup{setupErr: err}
		return
	}
	hr := comCall1(enumerator, immDeviceEnumeratorRegisterNotificationCallback, client)
	runtime.KeepAlive(client)
	if err := checkHRESULT("IMMDeviceEnumerator.RegisterEndpointNotificationCallback", hr); err != nil {
		release(client)
		w.ready <- watchSetup{setupErr: err}
		return
	}

	current, snapshotErr := endpointSnapshot()
	if snapshotErr != nil {
		current = baseline
	}
	w.ready <- watchSetup{baseline: cloneDeviceInfos(baseline), current: cloneDeviceInfos(current), registered: true}
	<-w.startLoop

	for {
		select {
		case <-w.stopLoop:
			w.unregisterAndRelease(enumerator, client)
			return
		default:
		}

		select {
		case <-w.stopLoop:
			w.unregisterAndRelease(enumerator, client)
			return
		case <-w.queue.wake:
			current, err := endpointSnapshot()
			if err == nil {
				w.publish(current)
				continue
			}
			// Enumeration can briefly fail while a device is changing state.
			// Retry a complete snapshot without blocking notification callbacks.
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-w.stopLoop:
				timer.Stop()
				w.unregisterAndRelease(enumerator, client)
				return
			case <-timer.C:
				w.queue.signal()
			}
		}
	}
}

func (w *endpointWatch) unregisterAndRelease(enumerator, client uintptr) {
	hr := comCall1(enumerator, immDeviceEnumeratorUnregisterNotificationCallback, client)
	err := checkHRESULT("IMMDeviceEnumerator.UnregisterEndpointNotificationCallback", hr)
	if err == nil || isNotFound(err) {
		// The SDK contract requires the client to hold its own reference until
		// after unregistering. Register and Unregister do not own that reference.
		release(client)
	}
	// On an unexpected unregister failure, keep the callback object rooted and
	// allocated. Releasing it could leave the enumerator with a dangling vtable.
}

func (w *endpointWatch) stop() {
	w.stopOnce.Do(func() {
		w.cancelDispatch()
		close(w.stopLoop)
		<-w.ownerDone
		close(w.dispatchStop)
	})
}

func (w *endpointWatch) cancelDispatch() {
	for {
		state := w.dispatchState.Load()
		if state&dispatchCancelled != 0 || w.dispatchState.CompareAndSwap(state, state|dispatchCancelled) {
			return
		}
	}
}

func (w *endpointWatch) publish(devices []driver.Info) {
	w.snapshotMu.Lock()
	w.latest = cloneDeviceInfos(devices)
	w.snapshotMu.Unlock()
	select {
	case w.dispatchWake <- struct{}{}:
	default:
	}
}

func (w *endpointWatch) currentSnapshot() []driver.Info {
	w.snapshotMu.Lock()
	defer w.snapshotMu.Unlock()
	return cloneDeviceInfos(w.latest)
}

func (w *endpointWatch) dispatch(baseline []driver.Info) {
	defer close(w.dispatchDone)
	previous := cloneDeviceInfos(baseline)
	for {
		select {
		case <-w.dispatchStop:
			return
		case <-w.dispatchWake:
		}
		current := w.currentSnapshot()
		for _, event := range diffEndpointSnapshots(previous, current) {
			if !w.dispatchEvent(event) {
				return
			}
		}
		previous = current
	}
}

// dispatchEvent atomically admits a callback before invoking user code. A
// callback may call stop because cancellation does not wait for the admitted
// callback to return; cancellation prevents every later admission.
func (w *endpointWatch) dispatchEvent(event driver.Event) bool {
	for {
		state := w.dispatchState.Load()
		if state&dispatchCancelled != 0 || state&dispatchActive != 0 {
			return false
		}
		if w.dispatchState.CompareAndSwap(state, state|dispatchActive) {
			break
		}
	}
	defer w.finishDispatchAdmission()
	w.fn(event)
	return true
}

func (w *endpointWatch) finishDispatchAdmission() {
	for {
		state := w.dispatchState.Load()
		if w.dispatchState.CompareAndSwap(state, state&^dispatchActive) {
			return
		}
	}
}

func initializeMTA() error {
	hr, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded)
	return checkHRESULT("CoInitializeEx(MTA)", hr)
}

func endpointSnapshot() ([]driver.Info, error) {
	return withSTA(func() ([]driver.Info, error) {
		enumerator, err := createEnumerator()
		if err != nil {
			return nil, err
		}
		defer release(enumerator)

		defaults := make(map[uint8]string, 2)
		for _, direction := range []uint8{1, 2} {
			id, err := defaultEndpointID(enumerator, direction)
			if err != nil && !isNotFound(err) {
				return nil, err
			}
			defaults[direction] = id
		}

		devices := make([]driver.Info, 0)
		for _, flow := range []struct {
			dataFlow  int
			direction uint8
		}{{flowRender, 1}, {flowCapture, 2}} {
			list, err := enumerateFlow(enumerator, flow.dataFlow, flow.direction, defaults[flow.direction])
			if err != nil {
				return nil, err
			}
			devices = append(devices, list...)
		}
		return devices, nil
	})
}

func newNotificationClient(w *endpointWatch) (uintptr, error) {
	ptr, _, _ := procCoTaskMemAlloc.Call(unsafe.Sizeof(notificationObject{}))
	if ptr == 0 {
		return 0, errors.New("tymbal wasapi: CoTaskMemAlloc failed for IMMNotificationClient")
	}
	object := (*notificationObject)(unsafe.Pointer(ptr))
	object.vtable = uintptr(unsafe.Pointer(&immNotificationClientVTable[0]))
	atomic.StoreUint32(&object.refs, 1)
	registerNotificationObject(ptr, w)
	runtime.KeepAlive(&immNotificationClientVTable)
	return ptr, nil
}

func registerNotificationObject(ptr uintptr, w *endpointWatch) {
	notificationRegistryMu.Lock()
	defer notificationRegistryMu.Unlock()
	previous := notificationRegistry.Load()
	next := make(map[uintptr]*endpointWatch, len(*previous)+1)
	for key, value := range *previous {
		next[key] = value
	}
	next[ptr] = w
	notificationRegistry.Store(&next)
}

func unregisterNotificationObject(ptr uintptr) {
	notificationRegistryMu.Lock()
	defer notificationRegistryMu.Unlock()
	previous := notificationRegistry.Load()
	next := make(map[uintptr]*endpointWatch, len(*previous))
	for key, value := range *previous {
		if key != ptr {
			next[key] = value
		}
	}
	notificationRegistry.Store(&next)
}

func notificationTarget(this uintptr) *endpointWatch {
	if this == 0 {
		return nil
	}
	registry := notificationRegistry.Load()
	if registry == nil {
		return nil
	}
	return (*registry)[this]
}

func notificationQueryInterface(this, iid, out uintptr) uintptr {
	if this == 0 || iid == 0 || out == 0 {
		return uintptr(ePointer)
	}
	*(*uintptr)(unsafe.Pointer(out)) = 0
	requested := *(*guid)(unsafe.Pointer(iid))
	if requested != iidIUnknown && requested != iidIMMNotificationClient {
		return uintptr(eNoInterface)
	}
	*(*uintptr)(unsafe.Pointer(out)) = this
	notificationAddRef(this)
	return 0
}

func notificationAddRef(this uintptr) uintptr {
	if this == 0 {
		return 0
	}
	refs := (*uint32)(unsafe.Pointer(this + unsafe.Offsetof(notificationObject{}.refs)))
	return uintptr(atomic.AddUint32(refs, 1))
}

func notificationRelease(this uintptr) uintptr {
	if this == 0 {
		return 0
	}
	refs := (*uint32)(unsafe.Pointer(this + unsafe.Offsetof(notificationObject{}.refs)))
	count := atomic.AddUint32(refs, ^uint32(0))
	if count == 0 {
		unregisterNotificationObject(this)
		procCoTaskMemFree.Call(this)
	}
	return uintptr(count)
}

func notificationOnDeviceStateChanged(this, _, _ uintptr) uintptr {
	return notificationSignal(this)
}

func notificationOnDeviceAdded(this, _ uintptr) uintptr {
	return notificationSignal(this)
}

func notificationOnDeviceRemoved(this, _ uintptr) uintptr {
	return notificationSignal(this)
}

func notificationOnDefaultDeviceChanged(this, _, _, _ uintptr) uintptr {
	return notificationSignal(this)
}

func notificationOnPropertyValueChanged(this, _, _ uintptr) uintptr {
	return notificationSignal(this)
}

func notificationSignal(this uintptr) uintptr {
	if w := notificationTarget(this); w != nil {
		w.queue.signal()
	}
	return 0
}

const (
	dispatchCancelled uint32 = 1 << iota
	dispatchActive

	eventDeviceAdded    uint8 = 1
	eventDeviceRemoved  uint8 = 2
	eventDefaultChanged uint8 = 3
	eventFormatChanged  uint8 = 4
)

func diffEndpointSnapshots(previous, current []driver.Info) []driver.Event {
	oldByID := endpointMap(previous)
	newByID := endpointMap(current)
	ids := make([]string, 0, len(oldByID)+len(newByID))
	seen := make(map[string]struct{}, len(oldByID)+len(newByID))
	for id := range oldByID {
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for id := range newByID {
		if _, ok := seen[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	events := make([]driver.Event, 0)
	for _, id := range ids {
		oldDevice, wasPresent := oldByID[id]
		newDevice, isPresent := newByID[id]
		switch {
		case !wasPresent:
			events = append(events, driver.Event{Kind: eventDeviceAdded, Device: newDevice})
		case !isPresent:
			events = append(events, driver.Event{Kind: eventDeviceRemoved, Device: oldDevice})
		case endpointFormatChanged(oldDevice, newDevice):
			events = append(events, driver.Event{Kind: eventFormatChanged, Device: newDevice})
		}
	}

	for _, direction := range []uint8{1, 2} {
		oldID := defaultID(previous, direction)
		newID := defaultID(current, direction)
		if oldID == newID {
			continue
		}
		device, ok := newByID[newID]
		if !ok {
			device = oldByID[oldID]
			device.Default = 0
		}
		if device.ID != "" {
			events = append(events, driver.Event{Kind: eventDefaultChanged, Device: device})
		}
	}
	return events
}

func endpointMap(devices []driver.Info) map[string]driver.Info {
	byID := make(map[string]driver.Info, len(devices))
	for _, device := range devices {
		if device.ID != "" {
			byID[device.ID] = device
		}
	}
	return byID
}

func defaultID(devices []driver.Info, direction uint8) string {
	for _, device := range devices {
		if device.Default == direction {
			return device.ID
		}
	}
	return ""
}

func endpointFormatChanged(a, b driver.Info) bool {
	if a.Inputs != b.Inputs || a.Outputs != b.Outputs || len(a.SampleRates) != len(b.SampleRates) {
		return true
	}
	for i := range a.SampleRates {
		if a.SampleRates[i] != b.SampleRates[i] {
			return true
		}
	}
	return false
}

func cloneDeviceInfos(devices []driver.Info) []driver.Info {
	if devices == nil {
		return nil
	}
	cloned := make([]driver.Info, len(devices))
	for i, device := range devices {
		cloned[i] = device
		cloned[i].SampleRates = append([]int(nil), device.SampleRates...)
	}
	return cloned
}
