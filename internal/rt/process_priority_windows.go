//go:build windows

package rt

import (
	"fmt"
	"sync"
	"syscall"
)

const (
	normalPriorityClass   = 0x00000020
	highPriorityClass     = 0x00000080
	realtimePriorityClass = 0x00000100
)

var (
	procGetCurrentProcess = syscall.NewLazyDLL("kernel32.dll").NewProc("GetCurrentProcess")
	procGetPriorityClass  = syscall.NewLazyDLL("kernel32.dll").NewProc("GetPriorityClass")
	procSetPriorityClass  = syscall.NewLazyDLL("kernel32.dll").NewProc("SetPriorityClass")
)

type processPriorityAPI interface {
	currentClass() (uint32, error)
	setClass(class uint32) error
}

type windowsProcessPriorityAPI struct{}

func (windowsProcessPriorityAPI) currentClass() (uint32, error) {
	process, _, _ := procGetCurrentProcess.Call()
	if process == 0 {
		return 0, syscall.EINVAL
	}
	class, _, callErr := procGetPriorityClass.Call(process)
	if class == 0 {
		return 0, windowsCallError(callErr)
	}
	return uint32(class), nil
}

func (windowsProcessPriorityAPI) setClass(class uint32) error {
	process, _, _ := procGetCurrentProcess.Call()
	if process == 0 {
		return syscall.EINVAL
	}
	result, _, callErr := procSetPriorityClass.Call(process, uintptr(class))
	if result == 0 {
		return windowsCallError(callErr)
	}
	return nil
}

func windowsCallError(err error) error {
	if err != nil && err != syscall.Errno(0) {
		return err
	}
	return syscall.EINVAL
}

// RaiseProcessPriority raises this process to HIGH_PRIORITY_CLASS and returns
// a function that restores its previous class. The load worker must be started
// before this call so it keeps its normal class. Use only for a bounded audio
// engine run: Windows applies the class to every thread in this process.
func RaiseProcessPriority() (restore func() error, priority string, err error) {
	return raiseProcessPriority(windowsProcessPriorityAPI{})
}

func raiseProcessPriority(api processPriorityAPI) (restore func() error, priority string, err error) {
	previous, err := api.currentClass()
	if err != nil {
		return func() error { return nil }, "", fmt.Errorf("rt: get process priority class: %w", err)
	}
	if previous == realtimePriorityClass {
		return func() error { return nil }, "REALTIME_PRIORITY_CLASS (unchanged)", nil
	}
	if previous == highPriorityClass {
		return func() error { return nil }, "HIGH_PRIORITY_CLASS (already set)", nil
	}
	if err := api.setClass(highPriorityClass); err != nil {
		return func() error { return nil }, "", fmt.Errorf("rt: set HIGH_PRIORITY_CLASS: %w", err)
	}
	var once sync.Once
	var restoreErr error
	return func() error {
		once.Do(func() { restoreErr = api.setClass(previous) })
		if restoreErr != nil {
			return fmt.Errorf("rt: restore process priority class: %w", restoreErr)
		}
		return nil
	}, "HIGH_PRIORITY_CLASS", nil
}
