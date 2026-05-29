package positional

// Positional literal pins layout: reorder reported but suppressed.
type Bad struct { // want `struct of size 12 could be 8; reorder skipped: positional composite literal`
	x byte
	y int32
	z byte
}

var _ = Bad{1, 2, 3}

// Keyed-only construction: reorder is safe.
type Good struct { // want `struct of size 12 could be 8`
	a byte
	b int32
	c byte
}

var _ = Good{a: 1, b: 2, c: 3}

// Empty literal T{} has no elements to re-map, so reorder is safe.
type EmptyLit struct { // want `struct of size 12 could be 8`
	p byte
	q int32
	r byte
}

var _ = EmptyLit{}

// Elided positional literal inside a slice: type comes from types.Info.
type NestedSlice struct { // want `struct of size 12 could be 8; reorder skipped: positional composite literal`
	x byte
	y int32
	z byte
}

var _ = []NestedSlice{{1, 2, 3}, {4, 5, 6}}

// &T{...} wraps the literal in UnaryExpr; inner node is still detected.
type AddressOf struct { // want `struct of size 12 could be 8; reorder skipped: positional composite literal`
	x byte
	y int32
	z byte
}

var _ = &AddressOf{1, 2, 3}

// Type-definition chain shares the layout; AliasChained's literal pins it.
type Chained struct { // want `struct of size 12 could be 8; reorder skipped: positional composite literal`
	x byte
	y int32
	z byte
}

type AliasChained Chained

var _ = AliasChained{1, 2, 3}

// Elided positional inside slice-of-pointer: canonicalStructType must unwrap *T.
type NestedSliceOfPtr struct { // want `struct of size 12 could be 8; reorder skipped: positional composite literal`
	x byte
	y int32
	z byte
}

var _ = []*NestedSliceOfPtr{{1, 2, 3}}

// Alias must collapse to T's underlying so the positional literal pins T.
type Aliased struct { // want `struct of size 12 could be 8; reorder skipped: positional composite literal`
	x byte
	y int32
	z byte
}

type AliasName = Aliased

var _ = AliasName{1, 2, 3}

// Generic instantiation pins the origin: canonicalStructType must collapse
// Box[int] onto the generic origin's *types.Struct via Origin().Underlying().
type Box[T any] struct { // want `struct of size 12 could be 8; reorder skipped: positional composite literal`
	x byte
	y int32
	z byte
}

var _ = Box[int]{1, 2, 3}

// Elided positional inside a slice of pointer to a generic instantiation:
// exercises the *types.Pointer unwrap together with Origin().
type GenPtr[T any] struct { // want `struct of size 12 could be 8; reorder skipped: positional composite literal`
	x byte
	y int32
	z byte
}

var _ = []*GenPtr[int]{{1, 2, 3}}
