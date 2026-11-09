package user

import "xatomic/dep"

// Each of these holds by value a dep struct whose word dep accesses with a
// 64-bit atomic: moving it behind the pointer puts the word at offset 4 on
// 386, arm and 32-bit mips, which panics at run time.
type Stats struct { // want `reorder skipped: 64-bit atomic access at .*dep.go:11:.* needs its 8-byte alignment`
	Hits dep.Counter
	Name *string
}

type ViaWrapped struct { // want `reorder skipped: 64-bit atomic access at .*dep.go:11:`
	W    dep.Wrapped
	Name *string
}

type ViaArray struct { // want `reorder skipped: 64-bit atomic access at .*dep.go:11:`
	Hits [2]dep.Counter
	Name *string
}

type ViaRenamed struct { // want `reorder skipped: 64-bit atomic access at .*renamed.go:10:`
	G    dep.Gauge
	Name *string
}

// Through an inline struct.
type Inline struct { // want `reorder skipped: 64-bit atomic access at .*dep.go:11:`
	In struct {
		C dep.Counter
	}
	Name *string
}

// Holding a local holder by value: the fixed point.
type Outer struct { // want `reorder skipped: 64-bit atomic access at .*dep.go:11:`
	L    local
	Name *string
}

type local struct {
	c dep.Counter
}

// dep.Plain is never accessed atomically: reordered as usual.
type Control struct { // want `struct with 16 pointer bytes could be 8$`
	P    dep.Plain
	Name *string
}

// Through a local pointer into dep.Dedup's array field.
type ViaElemPtr struct { // want `reorder skipped: 64-bit atomic access at .*dep.go:32:`
	D    dep.Dedup
	Name *string
}
