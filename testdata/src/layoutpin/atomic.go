package layoutpin

import "sync/atomic"

// The function API needs a 64-bit word 8-byte aligned on 386/arm/mips, which
// only the first word of an allocation guarantees; reordering moves it.
type atomicFirst struct { // want `reorder skipped: 64-bit atomic access at .*atomic.go:27:`
	atomicN    int64
	atomicName *string
	atomicOK   bool
}

// Promoted through an embedded struct: both layouts place the word.
type atomicInner struct { // want `reorder skipped: 64-bit atomic access at .*atomic.go:28:`
	atomicU    uint64
	atomicP    *string
	atomicFlag bool
}

type atomicOuter struct { // want `reorder skipped: 64-bit atomic access at .*atomic.go:28:`
	atomicInner
	atomicQ *string
	atomicB bool
}

func atomics(a *atomicFirst, o *atomicOuter) {
	atomic.AddInt64(&a.atomicN, 1)
	_ = atomic.LoadUint64(&(o.atomicU))
}

// An array element is placed by the array's offset, so it pins too.
type atomicArray struct { // want `reorder skipped: 64-bit atomic access at .*atomic.go:46:`
	atomicCounts [2]uint64
	atomicS      *string
	atomicZ      bool
}

// A slice's elements live in their own allocation: no pin.
type atomicSlice struct { // want `struct with 16 pointer bytes could be 8$`
	atomicHead  int64
	atomicItems []uint64
	atomicY     bool
}

func atomicIndexed(a *atomicArray, s *atomicSlice, i int) {
	atomic.AddUint64(&a.atomicCounts[i], 1)
	atomic.AddUint64(&s.atomicItems[i], 1)
}

// 32-bit operations and the typed atomic.Int64 (aligned by the compiler)
// need nothing: both structs still reorder.
type atomicTyped struct { // want `struct of size 24 could be 16$`
	atomicT  bool
	atomicV  atomic.Int64
	atomicT2 bool
	atomicI  int32
}

type atomic32 struct { // want `struct of size 24 could be 16$`
	atomicC  bool
	atomicW  *int32
	atomicC2 bool
	atomicX  int32
}

func atomicsFine(t *atomicTyped, s *atomic32) {
	t.atomicV.Add(1)
	atomic.AddInt32(&s.atomicX, 1)
}
