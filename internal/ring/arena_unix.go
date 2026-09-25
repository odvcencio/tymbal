//go:build linux || darwin

package ring

import "syscall"

func mapArena(size int) ([]byte, error) {
	return syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
}

func lockArena(mem []byte) error {
	return syscall.Mlock(mem)
}

func unlockArena(mem []byte) error {
	return syscall.Munlock(mem)
}

func unmapArena(mem []byte) error {
	return syscall.Munmap(mem)
}
