package ring

import (
	"errors"
	"sync/atomic"
	"unsafe"
)

var (
	ErrFrameRingConfig = errors.New("ring: slots must be a power of two and slot size must be positive")
	ErrFrameRingSize   = errors.New("ring: frame ring size overflows int")
	ErrArenaExhausted  = errors.New("ring: arena has insufficient space for frame ring")
)

// frameRingControl is the fixed control block shared with the trampoline ABI.
// The two counters occupy separate cache lines; keep their order and padding
// stable when changing this type.
type frameRingControl struct {
	head  uint64
	_     [56]byte
	tail  uint64
	_     [56]byte
	slots uint64
	slot  uint64
	data  unsafe.Pointer
}

// FrameRing is a single-producer, single-consumer ring of fixed-size frames.
// The control block and frame storage live in arena; keep the arena alive and
// do not close it until both sides have stopped.
type FrameRing struct {
	control   *frameRingControl
	arena     *Arena
	slots     uint64
	slotBytes int
}

// FrameRingStorageSize reports the arena bytes needed by NewFrameRing when the
// arena has not been used for other allocations.
func FrameRingStorageSize(slots, slotBytes int) (int, error) {
	if slots <= 0 || slots&(slots-1) != 0 || slotBytes <= 0 {
		return 0, ErrFrameRingConfig
	}
	maxInt := int(^uint(0) >> 1)
	if slotBytes > maxInt/slots {
		return 0, ErrFrameRingSize
	}
	controlSize := int(unsafe.Sizeof(frameRingControl{}))
	align := int(unsafe.Alignof(frameRingControl{}))
	dataAlign := int(unsafe.Alignof(uint64(0)))
	dataBytes := slots * slotBytes
	if controlSize > maxInt-(align-1) || controlSize+align-1 > maxInt-(dataAlign-1) {
		return 0, ErrFrameRingSize
	}
	base := controlSize + align - 1 + dataAlign - 1
	if dataBytes > maxInt-base {
		return 0, ErrFrameRingSize
	}
	return base + dataBytes, nil
}

// NewFrameRing places the ring's control block and slots in a. slots must be a
// power of two. Buffers passed to Write and Read must each be exactly slotBytes
// long. False means full/empty or an invalid buffer length.
func NewFrameRing(a *Arena, slots, slotBytes int) (*FrameRing, error) {
	if a == nil || a.closed {
		return nil, ErrArenaClosed
	}
	if slots <= 0 || slots&(slots-1) != 0 || slotBytes <= 0 {
		return nil, ErrFrameRingConfig
	}
	maxInt := int(^uint(0) >> 1)
	if slotBytes > maxInt/slots {
		return nil, ErrFrameRingSize
	}
	dataBytes := slots * slotBytes
	controlAlign := int(unsafe.Alignof(frameRingControl{}))
	dataAlign := int(unsafe.Alignof(uint64(0)))
	controlOffset, ok := alignedOffset(a.off, controlAlign)
	if !ok {
		return nil, ErrFrameRingSize
	}
	controlSize := int(unsafe.Sizeof(frameRingControl{}))
	if controlOffset > len(a.mem) || controlSize > len(a.mem)-controlOffset {
		return nil, ErrArenaExhausted
	}
	dataOffset, ok := alignedOffset(controlOffset+controlSize, dataAlign)
	if !ok || dataOffset > len(a.mem) || dataBytes > len(a.mem)-dataOffset {
		return nil, ErrArenaExhausted
	}
	ctrlBytes := a.Take(controlSize, controlAlign)
	dataBytesView := a.Take(dataBytes, dataAlign)
	ctrl := (*frameRingControl)(unsafe.Pointer(&ctrlBytes[0]))
	ctrl.slots = uint64(slots)
	ctrl.slot = uint64(slotBytes)
	ctrl.data = unsafe.Pointer(&dataBytesView[0])
	return &FrameRing{control: ctrl, arena: a, slots: uint64(slots), slotBytes: slotBytes}, nil
}

// Write copies one frame into the next free slot. Only the producer may call
// Write; it returns false if the ring is full or src has the wrong length.
func (r *FrameRing) Write(src []byte) bool {
	if r == nil || r.control == nil || len(src) != r.slotBytes {
		return false
	}
	h := atomic.LoadUint64(&r.control.head)
	t := atomic.LoadUint64(&r.control.tail)
	if h-t >= r.slots {
		return false
	}
	index := h & (r.slots - 1)
	dst := unsafe.Slice((*byte)(unsafe.Add(r.control.data, uintptr(index*r.control.slot))), r.slotBytes)
	copy(dst, src)
	atomic.StoreUint64(&r.control.head, h+1)
	return true
}

// Read copies the oldest available frame into dst. Only the consumer may call
// Read; it returns false if the ring is empty or dst has the wrong length.
func (r *FrameRing) Read(dst []byte) bool {
	if r == nil || r.control == nil || len(dst) != r.slotBytes {
		return false
	}
	t := atomic.LoadUint64(&r.control.tail)
	h := atomic.LoadUint64(&r.control.head)
	if t == h {
		return false
	}
	index := t & (r.slots - 1)
	src := unsafe.Slice((*byte)(unsafe.Add(r.control.data, uintptr(index*r.control.slot))), r.slotBytes)
	copy(dst, src)
	atomic.StoreUint64(&r.control.tail, t+1)
	return true
}

// Len returns the number of committed frames currently in the ring. A
// concurrent producer or consumer may change the result immediately.
func (r *FrameRing) Len() int {
	if r == nil || r.control == nil {
		return 0
	}
	h := atomic.LoadUint64(&r.control.head)
	t := atomic.LoadUint64(&r.control.tail)
	used := h - t
	if used > r.slots {
		used = r.slots
	}
	return int(used)
}

// Capacity returns the number of fixed-size slots.
func (r *FrameRing) Capacity() int {
	if r == nil {
		return 0
	}
	return int(r.slots)
}

func alignedOffset(off, align int) (int, bool) {
	mask := align - 1
	if off > int(^uint(0)>>1)-mask {
		return 0, false
	}
	return (off + mask) &^ mask, true
}
