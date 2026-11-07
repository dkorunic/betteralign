package otherbuild

// Each struct below is built positionally only in a file this platform
// doesn't compile (a_windows.go, a_linux.go when analysed elsewhere, or
// tagged.go behind a build tag): invisible to type checking, yet a reorder
// would break that build or silently re-map its values.
type Direct struct { // want `reorder skipped: positional composite literal at .*tagged.go`
	A bool
	B int64
	C bool
}

type Elided struct { // want `reorder skipped: positional composite literal at .*tagged.go`
	D bool
	E int64
	F bool
}

type Pointer struct { // want `reorder skipped: positional composite literal at .*tagged.go`
	G bool
	H int64
	I bool
}

// Keyed literals elsewhere don't pin.
type Keyed struct { // want `struct of size 24 could be 16$`
	J bool
	K int64
	L bool
}

// Built positionally only by gen.go, a file of another package (main): not
// this package's literal, so no pin.
type OnlyGen struct { // want `struct of size 24 could be 16$`
	M bool
	N int64
	O bool
}

// Built positionally only in never.go, which //go:build ignore keeps out of
// every build: nothing can break, so it still reorders.
type Never struct { // want `struct of size 24 could be 16$`
	P bool
	Q int64
	R bool
}

// Elided under named containers: the element type is only known by name in
// tagged.go, so it is resolved against this package's types.
type InSlice struct { // want `reorder skipped: positional composite literal at .*tagged.go`
	S bool
	T int64
	U bool
}

type InMapKey struct { // want `reorder skipped: positional composite literal at .*tagged.go`
	V bool
	W int64
	X bool
}

type (
	Slices []InSlice
	Nested [][]InSlice
	Keys   map[*InMapKey]int
)

// Built positionally only in negated.go, behind a tag's negation: a run that
// sets the tag excludes it (TestOtherBuildPositionalNegatedTag).
type Negated struct { // want `reorder skipped: positional composite literal at .*negated.go`
	Y bool
	Z int64
	Q bool
}
