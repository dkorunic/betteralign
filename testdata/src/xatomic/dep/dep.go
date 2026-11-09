package dep

import "sync/atomic"

// Counter's word is accessed atomically by its own method, which an
// importer holding a Counter by value cannot see (go.uber.org/atomic.Int64).
type Counter struct {
	v int64
}

func (c *Counter) Inc() { atomic.AddInt64(&c.v, 1) }

// Wrapped holds a Counter by value: an importer holding a Wrapped is placing
// the word too.
type Wrapped struct {
	C Counter
}

// Plain is never accessed atomically.
type Plain struct {
	V int64
}

// Dedup's words are reached through a local pointer to an element of its
// array field (internal/bisect.dedup): recent's offset places them.
type Dedup struct {
	recent [4][2]uint64
}

func (d *Dedup) Seen(h uint64) bool {
	cache := &d.recent[h%4]
	return atomic.LoadUint64(&cache[0]) == h
}
