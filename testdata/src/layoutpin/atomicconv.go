package layoutpin

import (
	"sync/atomic"
	"unsafe"
)

type atomicConvNamed int64

// The word may arrive through a pointer conversion.
type atomicConvA struct { // want `reorder skipped: 64-bit atomic access at .*atomicconv.go:29:`
	convN atomicConvNamed
	convP *string
}

type atomicConvB struct { // want `reorder skipped: 64-bit atomic access at .*atomicconv.go:30:`
	convU uint64
	convQ *string
}

// A slice element is its own allocation, converted or not.
type atomicConvC struct { // want `struct with 16 pointer bytes could be 8$`
	convH     int64
	convItems []atomicConvNamed
	convY     bool
}

func atomicConversions(a *atomicConvA, b *atomicConvB, c *atomicConvC, i int) {
	atomic.AddInt64((*int64)(&a.convN), 1)
	_ = atomic.LoadInt64((*int64)(unsafe.Pointer(&b.convU)))
	atomic.AddInt64((*int64)(&c.convItems[i]), 1)
}

// Not a conversion: what a function returns is out of sight.
type atomicConvD struct { // want `struct with 16 pointer bytes could be 8$`
	convI  int64
	convPP *string
	convZ  bool
}

func convPtr(p *int64) *int64 { return p }

// A function that only shares the naming of sync/atomic's pins nothing.
type atomicLookalike struct { // want `struct with 16 pointer bytes could be 8$`
	lookN int64
	lookP *string
	lookZ bool
}

func fakeAddInt64(p *int64, d int64) {}

func atomicNotAtomic(d *atomicConvD, l *atomicLookalike) {
	atomic.AddInt64(convPtr(&d.convI), 1)
	fakeAddInt64(&l.lookN, 1)
}
