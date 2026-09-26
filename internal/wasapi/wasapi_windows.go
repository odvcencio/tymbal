//go:build windows

// Package wasapi implements Windows Core Audio endpoint discovery and
// shared-mode render streams.
package wasapi

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"m31labs.dev/tymbal/internal/driver"
)

const (
	flowRender  = 0
	flowCapture = 1
	roleConsole = 0

	deviceStateActive       = 0x00000001
	clsctxAll               = 0x00000017
	stgmRead                = 0x00000000
	coinitApartmentThreaded = 0x00000002

	vtLPWStr = 31

	// Interface slots include the three IUnknown methods at offsets 0–2.
	immDeviceEnumeratorEnumAudioEndpoints        = 3
	immDeviceEnumeratorGetDefaultAudioEndpoint   = 4
	immDeviceEnumeratorGetDevice                 = 5
	immDeviceCollectionGetCount                  = 3
	immDeviceCollectionItem                      = 4
	immDeviceActivate                            = 3
	immDeviceOpenPropertyStore                   = 4
	immDeviceGetID                               = 5
	propertyStoreGetValue                        = 5
	audioClientGetBufferSize                     = 4
	audioClientGetCurrentPadding                 = 6
	audioClientGetMixFormat                      = 8
	audioClientStart                             = 10
	audioClientStop                              = 11
	audioClientSetEventHandle                    = 13
	audioClientGetService                        = 14
	audioClient3GetSharedModeEnginePeriod        = 18
	audioClient3GetCurrentSharedModeEnginePeriod = 19
	audioClient3InitializeSharedAudioStream      = 20
	audioRenderClientGetBuffer                   = 3
	audioRenderClientReleaseBuffer               = 4
	audioCaptureClientGetBuffer                  = 3
	audioCaptureClientReleaseBuffer              = 4
	audioCaptureClientGetNextPacketSize          = 5

	// AUDCLNT_ERR(0x004), expanded from the Windows SDK's facility value.
	audclntErrDeviceInvalidated = 0x88890004
	audclntShareModeShared      = 0
	audclntStreamEventCallback  = 0x00040000
	audclntBufferFlagSilent     = 0x00000002
)

var (
	clsidMMDeviceEnumerator = guid{0xbcde0395, 0xe52f, 0x467c, [8]byte{0x8e, 0x3d, 0xc4, 0x57, 0x92, 0x91, 0x69, 0x2e}}
	iidMMDeviceEnumerator   = guid{0xa95664d2, 0x9614, 0x4f35, [8]byte{0xa7, 0x46, 0xde, 0x8d, 0xb6, 0x36, 0x17, 0xe6}}
	iidAudioClient          = guid{0x1cb9ad4c, 0xdbfa, 0x4c32, [8]byte{0xb1, 0x78, 0xc2, 0xf5, 0x68, 0xa7, 0x03, 0xb2}}
	iidAudioClient3         = guid{0x7ed4ee07, 0x8e67, 0x4cd4, [8]byte{0x8c, 0x1a, 0x2b, 0x7a, 0x59, 0x87, 0xad, 0x42}}
	iidAudioRenderClient    = guid{0xf294acfc, 0x3146, 0x4483, [8]byte{0xa7, 0xbf, 0xad, 0xdc, 0xa7, 0xc2, 0x60, 0xe2}}
	iidAudioCaptureClient   = guid{0xc8adbd64, 0xe71e, 0x48a0, [8]byte{0xa4, 0xde, 0x18, 0x5c, 0x39, 0x5c, 0xd3, 0x17}}

	pkeyDeviceFriendlyName = propertyKey{
		fmtid: guid{0xa45c254e, 0xdf1c, 0x4efd, [8]byte{0x80, 0x20, 0x67, 0xd1, 0x46, 0xa8, 0x50, 0xe0}},
		pid:   14,
	}
)

var (
	ole32                = syscall.NewLazyDLL("ole32.dll")
	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")
	procPropVariantClear = ole32.NewProc("PropVariantClear")
)

type guid struct {
	data1 uint32
	data2 uint16
	data3 uint16
	data4 [8]byte
}

type propertyKey struct {
	fmtid guid
	pid   uint32
}

// PROPVARIANT has an eight-byte tag area followed by a sixteen-byte union on
// both Windows amd64 and arm64. Friendly-name values use VT_LPWSTR, whose
// pointer begins at the union offset.
type propVariant struct {
	vt       uint16
	reserved [3]uint16
	value    [2]uintptr
}

// WAVEFORMATEX is read only through its fixed header fields. The returned
// format is owned by COM and freed with CoTaskMemFree.
type waveFormatEx struct {
	formatTag      uint16
	channels       uint16
	samplesPerSec  uint32
	avgBytesPerSec uint32
	blockAlign     uint16
	bitsPerSample  uint16
	cbSize         uint16
}

type host struct{}

var _ driver.Driver = (*host)(nil)

// New returns a side-effect-free WASAPI driver. COM is initialized only for
// the duration of an enumeration call.
func New() driver.Driver { return &host{} }

func (*host) Name() string { return "wasapi" }

func (h *host) Devices() ([]driver.Info, error) {
	return withSTA(func() ([]driver.Info, error) {
		enumerator, err := createEnumerator()
		if err != nil {
			return nil, err
		}
		defer release(enumerator)

		defaults := make(map[uint8]string, 2)
		for _, dir := range []uint8{1, 2} {
			id, err := defaultEndpointID(enumerator, dir)
			if err != nil && !isNotFound(err) {
				return nil, err
			}
			defaults[dir] = id
		}

		var devices []driver.Info
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

func (h *host) Default(dir uint8) (driver.Info, error) {
	if dir != 1 && dir != 2 {
		return driver.Info{}, fmt.Errorf("tymbal wasapi: invalid direction %d", dir)
	}
	return withSTA(func() (driver.Info, error) {
		enumerator, err := createEnumerator()
		if err != nil {
			return driver.Info{}, err
		}
		defer release(enumerator)

		device, err := getDefaultEndpoint(enumerator, dir)
		if err != nil {
			return driver.Info{}, err
		}
		defer release(device)
		info, err := inspectEndpoint(device, dir)
		if err != nil {
			return driver.Info{}, err
		}
		info.Default = dir
		return info, nil
	})
}

// Watch is a no-op until this backend registers an IMMNotificationClient.
func (h *host) Watch(_ func(driver.Event)) func() { return func() {} }

func (h *host) Open(req driver.Request) (driver.Stream, error) { return openRenderStream(req) }

func withSTA[T any](fn func() (T, error)) (T, error) {
	type result struct {
		value T
		err   error
	}
	completed := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if err := initializeSTA(); err != nil {
			var zero T
			completed <- result{value: zero, err: err}
			return
		}
		value, err := fn()
		uninitializeCOM()
		completed <- result{value: value, err: err}
	}()
	out := <-completed
	return out.value, out.err
}

// createEnumerator requires COM to be initialized on the current OS thread.
func createEnumerator() (uintptr, error) {
	var enumerator uintptr
	hr, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)),
		0,
		clsctxAll,
		uintptr(unsafe.Pointer(&iidMMDeviceEnumerator)),
		uintptr(unsafe.Pointer(&enumerator)),
	)
	runtime.KeepAlive(&clsidMMDeviceEnumerator)
	runtime.KeepAlive(&iidMMDeviceEnumerator)
	runtime.KeepAlive(&enumerator)
	if err := checkHRESULT("CoCreateInstance(MMDeviceEnumerator)", hr); err != nil {
		release(enumerator)
		return 0, err
	}
	if enumerator == 0 {
		return 0, errors.New("tymbal wasapi: CoCreateInstance returned a nil enumerator")
	}
	return enumerator, nil
}

func enumerateFlow(enumerator uintptr, flow int, direction uint8, defaultID string) ([]driver.Info, error) {
	var collection uintptr
	hr := comCall3(enumerator, immDeviceEnumeratorEnumAudioEndpoints,
		uintptr(flow), deviceStateActive, uintptr(unsafe.Pointer(&collection)))
	runtime.KeepAlive(&collection)
	if err := checkHRESULT("IMMDeviceEnumerator.EnumAudioEndpoints", hr); err != nil {
		release(collection)
		return nil, err
	}
	if collection == 0 {
		return nil, errors.New("tymbal wasapi: endpoint enumeration returned a nil collection")
	}
	defer release(collection)

	var count uint32
	hr = comCall1(collection, immDeviceCollectionGetCount, uintptr(unsafe.Pointer(&count)))
	runtime.KeepAlive(&count)
	if err := checkHRESULT("IMMDeviceCollection.GetCount", hr); err != nil {
		return nil, err
	}

	devices := make([]driver.Info, 0, int(count))
	for i := uint32(0); i < count; i++ {
		var device uintptr
		hr = comCall2(collection, immDeviceCollectionItem, uintptr(i), uintptr(unsafe.Pointer(&device)))
		runtime.KeepAlive(&device)
		if err := checkHRESULT("IMMDeviceCollection.Item", hr); err != nil {
			release(device)
			return nil, err
		}
		if device == 0 {
			return nil, errors.New("tymbal wasapi: endpoint collection returned a nil device")
		}
		info, inspectErr := inspectEndpoint(device, direction)
		release(device)
		if inspectErr != nil {
			return nil, inspectErr
		}
		if info.ID == defaultID {
			info.Default = direction
		}
		devices = append(devices, info)
	}
	return devices, nil
}

// getDefaultEndpoint requires enumerator's owning COM thread.
func getDefaultEndpoint(enumerator uintptr, direction uint8) (uintptr, error) {
	flow := flowRender
	if direction == 2 {
		flow = flowCapture
	}
	var endpoint uintptr
	hr := comCall3(enumerator, immDeviceEnumeratorGetDefaultAudioEndpoint,
		uintptr(flow), roleConsole, uintptr(unsafe.Pointer(&endpoint)))
	runtime.KeepAlive(&endpoint)
	if err := checkHRESULT("IMMDeviceEnumerator.GetDefaultAudioEndpoint", hr); err != nil {
		release(endpoint)
		return 0, err
	}
	if endpoint == 0 {
		return 0, errors.New("tymbal wasapi: default endpoint lookup returned a nil device")
	}
	return endpoint, nil
}

func defaultEndpointID(enumerator uintptr, direction uint8) (string, error) {
	endpoint, err := getDefaultEndpoint(enumerator, direction)
	if err != nil {
		return "", err
	}
	defer release(endpoint)
	return endpointID(endpoint)
}

// inspectEndpoint requires endpoint's owning COM thread.
func inspectEndpoint(endpoint uintptr, direction uint8) (driver.Info, error) {
	id, err := endpointID(endpoint)
	if err != nil {
		return driver.Info{}, err
	}
	name := endpointFriendlyName(endpoint)
	if name == "" {
		name = id
	}

	var audioClient uintptr
	hr := comCall4(endpoint, immDeviceActivate,
		uintptr(unsafe.Pointer(&iidAudioClient)),
		clsctxAll,
		0,
		uintptr(unsafe.Pointer(&audioClient)))
	runtime.KeepAlive(&iidAudioClient)
	runtime.KeepAlive(&audioClient)
	if err := checkHRESULT("IMMDevice.Activate(IAudioClient)", hr); err != nil {
		release(audioClient)
		return driver.Info{}, err
	}
	if audioClient == 0 {
		return driver.Info{}, errors.New("tymbal wasapi: endpoint activation returned a nil audio client")
	}
	defer release(audioClient)

	var mixFormat uintptr
	hr = comCall1(audioClient, audioClientGetMixFormat, uintptr(unsafe.Pointer(&mixFormat)))
	runtime.KeepAlive(&mixFormat)
	if err := checkHRESULT("IAudioClient.GetMixFormat", hr); err != nil {
		if mixFormat != 0 {
			procCoTaskMemFree.Call(mixFormat)
		}
		return driver.Info{}, err
	}
	if mixFormat == 0 {
		return driver.Info{}, errors.New("tymbal wasapi: mix format lookup returned nil")
	}
	defer procCoTaskMemFree.Call(mixFormat)
	wave := (*waveFormatEx)(unsafe.Pointer(mixFormat))
	channels := int(wave.channels)
	rate := int(wave.samplesPerSec)
	if channels <= 0 || rate <= 0 {
		return driver.Info{}, fmt.Errorf("tymbal wasapi: invalid endpoint mix format: %d channels at %d Hz", channels, rate)
	}

	info := driver.Info{ID: id, Name: name, SampleRates: []int{rate}}
	if direction == 1 {
		info.Outputs = channels
	} else {
		info.Inputs = channels
	}
	return info, nil
}

func endpointID(endpoint uintptr) (string, error) {
	var idPtr uintptr
	hr := comCall1(endpoint, immDeviceGetID, uintptr(unsafe.Pointer(&idPtr)))
	runtime.KeepAlive(&idPtr)
	if err := checkHRESULT("IMMDevice.GetId", hr); err != nil {
		if idPtr != 0 {
			procCoTaskMemFree.Call(idPtr)
		}
		return "", err
	}
	if idPtr == 0 {
		return "", errors.New("tymbal wasapi: IMMDevice.GetId returned nil")
	}
	defer procCoTaskMemFree.Call(idPtr)
	return utf16PointerToString((*uint16)(unsafe.Pointer(idPtr))), nil
}

func endpointFriendlyName(endpoint uintptr) string {
	var store uintptr
	hr := comCall2(endpoint, immDeviceOpenPropertyStore, stgmRead, uintptr(unsafe.Pointer(&store)))
	runtime.KeepAlive(&store)
	if int32(hr) < 0 || store == 0 {
		release(store)
		return ""
	}
	defer release(store)

	value := propVariant{}
	defer procPropVariantClear.Call(uintptr(unsafe.Pointer(&value)))
	hr = comCall2(store, propertyStoreGetValue,
		uintptr(unsafe.Pointer(&pkeyDeviceFriendlyName)),
		uintptr(unsafe.Pointer(&value)))
	runtime.KeepAlive(&pkeyDeviceFriendlyName)
	runtime.KeepAlive(&value)
	if int32(hr) < 0 {
		return ""
	}
	if value.vt != vtLPWStr {
		return ""
	}
	ptr := value.value[0]
	return utf16PointerToString((*uint16)(unsafe.Pointer(ptr)))
}

func utf16PointerToString(ptr *uint16) string {
	if ptr == nil {
		return ""
	}
	for n := 0; n < 32768; n++ {
		if *(*uint16)(unsafe.Pointer(uintptr(unsafe.Pointer(ptr)) + uintptr(n)*2)) == 0 {
			return syscall.UTF16ToString(unsafe.Slice(ptr, n))
		}
	}
	return ""
}

// release must run on the COM object's owning thread.
func release(object uintptr) {
	if object != 0 {
		_ = comCall0(object, 2)
	}
}

// initializeSTA and uninitializeCOM must be paired on the same locked OS
// thread. IAudioClient's first device access must occur on an STA thread on
// Windows 8, so stream setup uses these on Tymbal's locked stream thread.
func initializeSTA() error {
	hr, _, _ := procCoInitializeEx.Call(0, coinitApartmentThreaded)
	return checkHRESULT("CoInitializeEx", hr)
}

func uninitializeCOM() { procCoUninitialize.Call() }

// comMethod and the fixed-arity call helpers avoid building an argument slice.
// They are suitable for the real-time stream path. Callers must keep Go memory
// passed by address alive through each call.
func comMethod(object uintptr, slot int) uintptr {
	vtable := *(*uintptr)(unsafe.Pointer(object))
	return *(*uintptr)(unsafe.Pointer(vtable + uintptr(slot)*unsafe.Sizeof(uintptr(0))))
}

func comCall0(object uintptr, slot int) uintptr {
	hr, _, _ := syscall.SyscallN(comMethod(object, slot), object)
	runtime.KeepAlive(object)
	return hr
}

func comCall1(object uintptr, slot int, a0 uintptr) uintptr {
	hr, _, _ := syscall.SyscallN(comMethod(object, slot), object, a0)
	runtime.KeepAlive(object)
	return hr
}

func comCall2(object uintptr, slot int, a0, a1 uintptr) uintptr {
	hr, _, _ := syscall.SyscallN(comMethod(object, slot), object, a0, a1)
	runtime.KeepAlive(object)
	return hr
}

func comCall3(object uintptr, slot int, a0, a1, a2 uintptr) uintptr {
	hr, _, _ := syscall.SyscallN(comMethod(object, slot), object, a0, a1, a2)
	runtime.KeepAlive(object)
	return hr
}

func comCall4(object uintptr, slot int, a0, a1, a2, a3 uintptr) uintptr {
	hr, _, _ := syscall.SyscallN(comMethod(object, slot), object, a0, a1, a2, a3)
	runtime.KeepAlive(object)
	return hr
}

func comCall5(object uintptr, slot int, a0, a1, a2, a3, a4 uintptr) uintptr {
	hr, _, _ := syscall.SyscallN(comMethod(object, slot), object, a0, a1, a2, a3, a4)
	runtime.KeepAlive(object)
	return hr
}

func comCall6(object uintptr, slot int, a0, a1, a2, a3, a4, a5 uintptr) uintptr {
	hr, _, _ := syscall.SyscallN(comMethod(object, slot), object, a0, a1, a2, a3, a4, a5)
	runtime.KeepAlive(object)
	return hr
}

type hresultError struct {
	op    string
	code  uint32
	cause error
}

func (e *hresultError) Error() string {
	return fmt.Sprintf("tymbal wasapi: %s failed with HRESULT 0x%08X", e.op, e.code)
}

func (e *hresultError) Unwrap() error { return e.cause }

func checkHRESULT(op string, value uintptr) error {
	code := uint32(value)
	if int32(code) >= 0 {
		return nil
	}
	err := &hresultError{op: op, code: code}
	if code == audclntErrDeviceInvalidated {
		err.cause = driver.ErrLost
	}
	return err
}

func isNotFound(err error) bool {
	var hrErr *hresultError
	return errors.As(err, &hrErr) && hrErr.code == 0x80070490 // HRESULT_FROM_WIN32(ERROR_NOT_FOUND)
}
