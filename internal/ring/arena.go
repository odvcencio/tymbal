// Package ring provides real-time-friendly memory arenas and single-producer,
// single-consumer frame rings.
package ring

import (
	"errors"
	"os"
)

var (
	ErrArenaSize   = errors.New("ring: arena size must be positive")
	ErrArenaClosed = errors.New("ring: arena is closed")
)

// Arena is a page-backed region carved up at stream setup and retained until
// Close. Take panics if a request is invalid or exceeds the remaining region.
type Arena struct {
	mem    []byte
	off    int
	locked bool
	closed bool
}

// NewArena maps size bytes and touches every page before returning. The backing
// pages are outside the Go heap so an OS API may retain their address.
func NewArena(size int) (*Arena, error) {
	if size <= 0 {
		return nil, ErrArenaSize
	}
	mem, err := mapArena(size)
	if err != nil {
		return nil, err
	}
	pageSize := os.Getpagesize()
	for i := 0; i < len(mem); {
		mem[i] = 0
		if len(mem)-i <= pageSize {
			break
		}
		i += pageSize
	}
	mem[len(mem)-1] = 0
	return &Arena{mem: mem}, nil
}

// Take returns n bytes with the requested power-of-two alignment.
func (a *Arena) Take(n, align int) []byte {
	if a == nil || a.closed {
		panic(ErrArenaClosed)
	}
	if n < 0 || align <= 0 || align&(align-1) != 0 {
		panic("ring: invalid arena allocation")
	}
	mask := align - 1
	if a.off > int(^uint(0)>>1)-mask {
		panic("ring: arena offset overflow")
	}
	start := (a.off + mask) &^ mask
	if start > len(a.mem) || n > len(a.mem)-start {
		panic("ring: arena exhausted")
	}
	a.off = start + n
	return a.mem[start:a.off]
}

// Lock asks the operating system to keep the mapped pages resident. Failure is
// returned to the caller; it does not prevent using the arena.
func (a *Arena) Lock() error {
	if a == nil || a.closed {
		return ErrArenaClosed
	}
	if a.locked {
		return nil
	}
	if err := lockArena(a.mem); err != nil {
		return err
	}
	a.locked = true
	return nil
}

// Close unmaps the region. It is safe to call more than once.
func (a *Arena) Close() error {
	if a == nil || a.closed {
		return nil
	}
	var unlockErr error
	if a.locked {
		unlockErr = unlockArena(a.mem)
	}
	unmapErr := unmapArena(a.mem)
	if unmapErr == nil {
		a.mem = nil
		a.off = 0
		a.locked = false
		a.closed = true
	}
	if unlockErr != nil {
		return unlockErr
	}
	return unmapErr
}

func (a *Arena) remaining() int {
	if a == nil || a.closed {
		return 0
	}
	return len(a.mem) - a.off
}
