package ring

import (
	"runtime"
	"sync"
	"testing"
	"unsafe"
)

func TestArenaAlignmentAndClose(t *testing.T) {
	if _, err := NewArena(0); err != ErrArenaSize {
		t.Fatalf("NewArena(0) error = %v, want ErrArenaSize", err)
	}
	a, err := NewArena(256)
	if err != nil {
		t.Fatal(err)
	}
	a.Take(3, 1)
	aligned := a.Take(16, 8)
	if uintptr(unsafe.Pointer(&aligned[0]))%8 != 0 {
		t.Fatalf("Take did not align memory to 8 bytes")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := a.Lock(); err != ErrArenaClosed {
		t.Fatalf("Lock after Close = %v, want ErrArenaClosed", err)
	}
}

func TestFrameRingLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("fixed trampoline layout is for 64-bit targets")
	}
	var c frameRingControl
	if unsafe.Offsetof(c.head) != 0 || unsafe.Offsetof(c.tail) != 64 || unsafe.Offsetof(c.slots) != 128 || unsafe.Offsetof(c.slot) != 136 || unsafe.Offsetof(c.data) != 144 || unsafe.Sizeof(c) != 152 {
		t.Fatalf("unexpected control layout: head=%d tail=%d slots=%d slot=%d data=%d size=%d",
			unsafe.Offsetof(c.head), unsafe.Offsetof(c.tail), unsafe.Offsetof(c.slots), unsafe.Offsetof(c.slot), unsafe.Offsetof(c.data), unsafe.Sizeof(c))
	}
}

func TestFrameRingFullEmptyAndWrap(t *testing.T) {
	const slots, slotBytes = 4, 3
	size, err := FrameRingStorageSize(slots, slotBytes)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewArena(size)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	r, err := NewFrameRing(a, slots, slotBytes)
	if err != nil {
		t.Fatal(err)
	}
	if r.Capacity() != slots || r.Len() != 0 {
		t.Fatalf("initial capacity/length = %d/%d, want %d/0", r.Capacity(), r.Len(), slots)
	}
	if r.Read(make([]byte, slotBytes)) {
		t.Fatal("Read succeeded on an empty ring")
	}
	if r.Write([]byte{1, 2}) {
		t.Fatal("Write accepted a buffer with the wrong length")
	}
	for i := byte(0); i < slots; i++ {
		if !r.Write([]byte{i, i + 10, i + 20}) {
			t.Fatalf("Write %d failed before ring filled", i)
		}
	}
	if r.Len() != slots {
		t.Fatalf("full ring Len = %d, want %d", r.Len(), slots)
	}
	if r.Write([]byte{9, 9, 9}) {
		t.Fatal("Write succeeded on a full ring")
	}
	for i := byte(0); i < slots; i++ {
		got := make([]byte, slotBytes)
		if !r.Read(got) {
			t.Fatalf("Read %d failed before ring emptied", i)
		}
		want := []byte{i, i + 10, i + 20}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("Read %d = %v, want %v", i, got, want)
			}
		}
	}
	if r.Len() != 0 || r.Read(make([]byte, slotBytes)) {
		t.Fatal("ring was not empty after draining")
	}
	if !r.Write([]byte{7, 8, 9}) {
		t.Fatal("Write after wrap failed")
	}
	got := make([]byte, slotBytes)
	if !r.Read(got) || got[0] != 7 || got[1] != 8 || got[2] != 9 {
		t.Fatalf("wrapped Read = %v", got)
	}
}

func TestFrameRingConcurrentSPSC(t *testing.T) {
	const slots, slotBytes, frames = 64, 8, 100000
	size, err := FrameRingStorageSize(slots, slotBytes)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewArena(size)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	r, err := NewFrameRing(a, slots, slotBytes)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var failed bool
	wg.Add(2)
	go func() {
		defer wg.Done()
		var frame [slotBytes]byte
		for n := uint64(0); n < frames; n++ {
			for {
				putUint64(frame[:], n)
				if r.Write(frame[:]) {
					break
				}
				runtime.Gosched()
			}
		}
	}()
	go func() {
		defer wg.Done()
		var frame [slotBytes]byte
		for n := uint64(0); n < frames; n++ {
			for {
				if r.Read(frame[:]) {
					if got := getUint64(frame[:]); got != n {
						failed = true
					}
					break
				}
				runtime.Gosched()
			}
		}
	}()
	wg.Wait()
	if failed {
		t.Fatal("SPSC ring reordered or corrupted frames")
	}
}

func TestFrameRingNoAlloc(t *testing.T) {
	size, err := FrameRingStorageSize(2, 8)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewArena(size)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	r, err := NewFrameRing(a, 2, 8)
	if err != nil {
		t.Fatal(err)
	}
	var src, dst [8]byte
	allocs := testing.AllocsPerRun(100, func() {
		if !r.Write(src[:]) || !r.Read(dst[:]) {
			t.Fatal("ring operation failed")
		}
	})
	if allocs != 0 {
		t.Fatalf("Write/Read allocated %g times per frame", allocs)
	}
}

func TestFrameRingConfigAndCapacity(t *testing.T) {
	if _, err := FrameRingStorageSize(3, 8); err != ErrFrameRingConfig {
		t.Fatalf("non-power-of-two size error = %v, want ErrFrameRingConfig", err)
	}
	if _, err := FrameRingStorageSize(4, 0); err != ErrFrameRingConfig {
		t.Fatalf("zero slot size error = %v, want ErrFrameRingConfig", err)
	}
	if _, err := NewFrameRing(nil, 2, 8); err != ErrArenaClosed {
		t.Fatalf("nil arena error = %v, want ErrArenaClosed", err)
	}
	a, err := NewArena(1)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := NewFrameRing(a, 2, 8); err != ErrArenaExhausted {
		t.Fatalf("small arena error = %v, want ErrArenaExhausted", err)
	}
}

func TestArenaLockIsBestEffort(t *testing.T) {
	a, err := NewArena(4096)
	if err != nil {
		t.Fatal(err)
	}
	lockErr := a.Lock() // Host memlock limits may reject this request.
	closeErr := a.Close()
	if lockErr == nil && closeErr != nil {
		t.Fatalf("Close after successful Lock: %v", closeErr)
	}
}

func putUint64(dst []byte, v uint64) {
	for i := range 8 {
		dst[i] = byte(v >> (8 * i))
	}
}

func getUint64(src []byte) uint64 {
	var v uint64
	for i := range 8 {
		v |= uint64(src[i]) << (8 * i)
	}
	return v
}
