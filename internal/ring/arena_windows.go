//go:build windows

package ring

import (
	"syscall"
	"unsafe"
)

const (
	memCommit     = 0x1000
	memReserve    = 0x2000
	memRelease    = 0x8000
	pageReadWrite = 0x04
)

var (
	kernel32      = syscall.NewLazyDLL("kernel32.dll")
	virtualAlloc  = kernel32.NewProc("VirtualAlloc")
	virtualFree   = kernel32.NewProc("VirtualFree")
	virtualLock   = kernel32.NewProc("VirtualLock")
	virtualUnlock = kernel32.NewProc("VirtualUnlock")
)

func mapArena(size int) ([]byte, error) {
	addr, _, callErr := virtualAlloc.Call(0, uintptr(size), memReserve|memCommit, pageReadWrite)
	if addr == 0 {
		return nil, syscallError(callErr)
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(addr)), size), nil
}

func lockArena(mem []byte) error {
	addr, size := arenaAddress(mem)
	r1, _, callErr := virtualLock.Call(addr, size)
	if r1 == 0 {
		return syscallError(callErr)
	}
	return nil
}

func unlockArena(mem []byte) error {
	addr, size := arenaAddress(mem)
	r1, _, callErr := virtualUnlock.Call(addr, size)
	if r1 == 0 {
		return syscallError(callErr)
	}
	return nil
}

func unmapArena(mem []byte) error {
	addr, _ := arenaAddress(mem)
	r1, _, callErr := virtualFree.Call(addr, 0, memRelease)
	if r1 == 0 {
		return syscallError(callErr)
	}
	return nil
}

func arenaAddress(mem []byte) (uintptr, uintptr) {
	if len(mem) == 0 {
		return 0, 0
	}
	return uintptr(unsafe.Pointer(&mem[0])), uintptr(len(mem))
}

func syscallError(err error) error {
	if errno, ok := err.(syscall.Errno); ok && errno != 0 {
		return errno
	}
	return syscall.EINVAL
}
