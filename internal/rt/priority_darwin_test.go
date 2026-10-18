//go:build darwin && (amd64 || arm64)

package rt

import (
	"testing"
	"time"
	"unsafe"
)

func TestDarwinPriorityGrantRestoreAndDenial(t *testing.T) {
	for _, denied := range []bool{false, true} {
		releases, sets := 0, 0
		c := darwinPriorityCalls{thread: func() uint32 { return 42 }, timebase: func() (uint32, uint32) { return 125, 3 },
			get: func(thread, flavor uint32, p unsafe.Pointer, n, d *uint32) int32 {
				*d = 1
				if flavor == 1 {
					*(*uint32)(p) = 1
				}
				return 0
			},
			set: func(thread, flavor uint32, p unsafe.Pointer, n uint32) int32 {
				sets++
				if denied {
					return 5
				}
				if sets == 1 {
					v := *(*timeConstraintPolicy)(p)
					if flavor != 2 || n != 4 || v.period != 64000 || v.computation != 32000 || v.constraint != 64000 {
						t.Errorf("invalid time policy%+v", v)
					}
				} else if flavor != 1 || n != 1 {
					t.Error("previous policy was not restored")
				}
				return 0
			},
			release: func(uint32) { releases++ }}
		g := raiseDarwinPriority(time.Second*128/48000, c)
		if denied {
			if g.Kind != "normal" || releases != 1 {
				t.Fatal("denied priority was not cleanly released")
			}
		} else {
			if g.Kind != "coreaudio time-constraint" {
				t.Fatal(g.Kind)
			}
			Lower(g)
			if releases != 1 || sets != 2 {
				t.Fatal("priority/port not restored")
			}
		}
	}
}
