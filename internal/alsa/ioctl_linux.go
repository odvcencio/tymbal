//go:build linux && (amd64 || arm64)

package alsa

import (
	"runtime"
	"syscall"
	"unsafe"
)

const (
	iocNone  uintptr = 0
	iocWrite uintptr = 1
	iocRead  uintptr = 2
)

// ioc implements the asm-generic ioctl encoding used by Linux amd64 and
// arm64: direction in bits 30-31, size in bits 16-29, type in bits 8-15,
// and request number in bits 0-7.
func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | typ<<8 | nr
}

var (
	ioctlCtlCardInfo      = ioc(iocRead, 'U', 0x01, unsafe.Sizeof(sndCtlCardInfo{}))
	ioctlCtlPCMNextDevice = ioc(iocRead, 'U', 0x30, unsafe.Sizeof(int32(0)))
	ioctlCtlPCMInfo       = ioc(iocRead|iocWrite, 'U', 0x31, unsafe.Sizeof(sndPCMInfo{}))

	ioctlPCMPVersion     = ioc(iocRead, 'A', 0x00, unsafe.Sizeof(int32(0)))
	ioctlPCMTTStamp      = ioc(iocWrite, 'A', 0x03, unsafe.Sizeof(int32(0)))
	ioctlPCMUserPVersion = ioc(iocWrite, 'A', 0x04, unsafe.Sizeof(int32(0)))
	ioctlPCMHWRefine     = ioc(iocRead|iocWrite, 'A', 0x10, unsafe.Sizeof(hwParams{}))
	ioctlPCMHWParams     = ioc(iocRead|iocWrite, 'A', 0x11, unsafe.Sizeof(hwParams{}))
	ioctlPCMSWParams     = ioc(iocRead|iocWrite, 'A', 0x13, unsafe.Sizeof(swParams{}))
	ioctlPCMDelay        = ioc(iocRead, 'A', 0x21, unsafe.Sizeof(int64(0)))
	ioctlPCMPrepare      = ioc(iocNone, 'A', 0x40, 0)
	ioctlPCMStart        = ioc(iocNone, 'A', 0x42, 0)
	ioctlPCMDrop         = ioc(iocNone, 'A', 0x43, 0)
	ioctlPCMResume       = ioc(iocNone, 'A', 0x47, 0)
	ioctlPCMWriteI       = ioc(iocWrite, 'A', 0x50, unsafe.Sizeof(xferi{}))
	ioctlPCMReadI        = ioc(iocRead, 'A', 0x51, unsafe.Sizeof(xferi{}))
	ioctlPCMLink         = ioc(iocWrite, 'A', 0x60, unsafe.Sizeof(int32(0)))
)

//tymbal:rt
func ioctl(fd int, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), request, uintptr(arg))
	runtime.KeepAlive(arg)
	if errno != 0 {
		return errno
	}
	return nil
}
