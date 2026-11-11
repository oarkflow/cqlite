package parser

import "unsafe"

// arena is a monotonic bump allocator.
// It pre-allocates a large slab and hands out slices from it.
// This eliminates per-node heap allocations and the associated GC overhead.
//
// When the current slab is exhausted, a new (larger) slab is allocated.
// All slabs are released together on reset().
//
// For a typical SQL statement the primary slab (64 KiB) is sufficient,
// meaning the entire parse produces zero net allocations after warm-up.
type arena struct {
	slabs [][]byte
	cur   []byte
	off   int
}

const (
	initialSlabSize = 8 * 1024 // 8 KiB
	growFactor      = 2
)

func (a *arena) alloc(n int) []byte {
	// round up to 8-byte alignment
	n = (n + 7) &^ 7
	if a.off+n > len(a.cur) {
		size := len(a.cur) * growFactor
		if size < n+8 {
			size = n + initialSlabSize
		}
		slab := make([]byte, size)
		a.slabs = append(a.slabs, slab)
		a.cur = slab
		a.off = 0
	}
	out := a.cur[a.off : a.off+n]
	a.off += n
	return out[:n]
}

// reset releases all slabs and reinitialises the arena.
// The first slab is retained to avoid re-allocation on the next parse.
func (a *arena) reset() {
	if len(a.slabs) > 0 {
		best := a.slabs[0]
		for _, slab := range a.slabs[1:] {
			if len(slab) > len(best) {
				best = slab
			}
		}
		a.slabs[0] = best
		a.slabs = a.slabs[:1]
		a.cur = best
		a.off = 0
	}
}

// ensure the first slab exists
func (a *arena) init() {
	if a.cur == nil {
		slab := make([]byte, initialSlabSize)
		a.slabs = append(a.slabs, slab)
		a.cur = slab
	}
}

// allocPtr returns a pointer into the arena for a single value of size n.
func (a *arena) allocPtr(n uintptr) unsafe.Pointer {
	b := a.alloc(int(n))
	return unsafe.Pointer(&b[0])
}

func arenaMakeSlice[T any](a *arena, n, capn int) []T {
	if capn < n {
		capn = n
	}
	if capn == 0 {
		return nil
	}
	var zero T
	elemSize := unsafe.Sizeof(zero)
	mem := a.alloc(int(elemSize * uintptr(capn)))
	base := (*T)(unsafe.Pointer(&mem[0]))
	out := unsafe.Slice(base, capn)
	return out[:n]
}

func arenaAppend[T any](a *arena, s []T, v T) []T {
	if len(s) < cap(s) {
		n := len(s)
		s = s[:n+1]
		s[n] = v
		return s
	}
	newCap := 4
	if c := cap(s); c > 0 {
		newCap = c * 2
	}
	ns := arenaMakeSlice[T](a, len(s)+1, newCap)
	copy(ns, s)
	ns[len(s)] = v
	return ns
}
