//go:build windows

package rt

import (
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var procGetSystemDirectoryW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemDirectoryW")

type mmcssAPI interface {
	load() error
	setCharacteristics(taskName *uint16, taskIndex *uint32) uintptr
	revert(thread uintptr) bool
}

type avrtProcedures struct {
	setCharacteristics *syscall.Proc
	revert             *syscall.Proc
}

var (
	avrtLoadOnce sync.Once
	avrt         *avrtProcedures
	avrtLoadErr  error
)

type windowsMMCSSAPI struct{}

func (windowsMMCSSAPI) load() error {
	avrtLoadOnce.Do(func() {
		avrt, avrtLoadErr = loadAVRTFromSystemDirectory()
	})
	return avrtLoadErr
}

func (windowsMMCSSAPI) setCharacteristics(taskName *uint16, taskIndex *uint32) uintptr {
	handle, _, _ := avrt.setCharacteristics.Call(
		uintptr(unsafe.Pointer(taskName)),
		uintptr(unsafe.Pointer(taskIndex)),
	)
	runtime.KeepAlive(taskName)
	runtime.KeepAlive(taskIndex)
	return handle
}

func (windowsMMCSSAPI) revert(thread uintptr) bool {
	result, _, _ := avrt.revert.Call(thread)
	return result != 0
}

func loadAVRTFromSystemDirectory() (*avrtProcedures, error) {
	if err := procGetSystemDirectoryW.Find(); err != nil {
		return nil, err
	}
	var systemDirectory [260]uint16
	length, _, callErr := procGetSystemDirectoryW.Call(
		uintptr(unsafe.Pointer(&systemDirectory[0])),
		uintptr(len(systemDirectory)),
	)
	runtime.KeepAlive(&systemDirectory)
	if length == 0 {
		if callErr != nil {
			return nil, callErr
		}
		return nil, syscall.EINVAL
	}
	if length >= uintptr(len(systemDirectory)) {
		return nil, syscall.ENAMETOOLONG
	}
	dllPath := syscall.UTF16ToString(systemDirectory[:length]) + `\avrt.dll`
	dll, err := syscall.LoadDLL(dllPath)
	if err != nil {
		return nil, err
	}
	setCharacteristics, err := dll.FindProc("AvSetMmThreadCharacteristicsW")
	if err != nil {
		return nil, err
	}
	revert, err := dll.FindProc("AvRevertMmThreadCharacteristics")
	if err != nil {
		return nil, err
	}
	return &avrtProcedures{setCharacteristics: setCharacteristics, revert: revert}, nil
}

func raisePriority(period time.Duration) Grant {
	return raisePriorityWithAPI(period, windowsMMCSSAPI{})
}

func raisePriorityWithAPI(_ time.Duration, api mmcssAPI) Grant {
	if err := api.load(); err != nil {
		return Grant{Kind: "normal"}
	}
	taskName, err := syscall.UTF16PtrFromString("Pro Audio")
	if err != nil {
		return Grant{Kind: "normal"}
	}
	var taskIndex uint32
	thread := api.setCharacteristics(taskName, &taskIndex)
	runtime.KeepAlive(taskName)
	runtime.KeepAlive(&taskIndex)
	if thread == 0 {
		return Grant{Kind: "normal"}
	}
	return Grant{
		Kind:    "MMCSS Pro Audio",
		restore: func() { api.revert(thread) },
	}
}
