package nested

// four is already optimal: 32 bytes, 8 of them pointer-free at the end.
type four struct {
	p, q, r *int
	x       int64
}

// inner reorders to p, q, a, ending in 8 pointer-free bytes instead of none.
type inner struct { // want `struct with 24 pointer bytes could be 16`
	p *int
	a bool
	q *int
}

// Measured against inner as declared, outer would move in to the front (no
// trailing pointer-free bytes) and the next run would move it back behind the
// larger f. Measured as this run leaves inner, outer is already in place.
type outer struct {
	f  four
	in inner
}

// The same through a type alias, whose struct literal is rewritten too.
type aliasInner = struct { // want `struct with 24 pointer bytes could be 16`
	ap *int
	aa bool
	aq *int
}

type aliasOuter struct {
	f  four
	in aliasInner
}

// An inner struct no run rewrites is measured as declared, so its outer is
// reordered once and stays put: ignored ...
type ignoredInner struct { // betteralign:ignore
	p *int
	a bool
	q *int
}

type ignoredOuter struct { // want `struct with 56 pointer bytes could be 48`
	f  four
	in ignoredInner
}

// ... single-line (dstmin can't rewrite it; see oneline.go) ...
type oneLineOuter struct { // want `struct with 56 pointer bytes could be 48`
	f  four
	in oneLine
}

// ... or declared in a generated file (see gen.go).
type genOuter struct { // want `struct with 56 pointer bytes could be 48`
	f  four
	in genInner
}

// A generic struct laid out by its type parameter is never rewritten, so an
// instance is measured as declared ...
type byValue[T any] struct {
	p *int
	a T
	q *int
}

type byValueOuter struct { // want `struct with 56 pointer bytes could be 48`
	f  four
	in byValue[bool]
}

// ... while one whose layout doesn't depend on it is, like any other.
type byPointer[T any] struct { // want `struct with 24 pointer bytes could be 16`
	p *T
	a bool
	q *int
}

type byPointerOuter struct {
	f  four
	in byPointer[int]
}

// An alias of a defined type is measured like the type itself.
type innerAlias = inner

type innerAliasOuter struct {
	f  four
	in innerAlias
}

// An inner struct the same run really reorders (time.Time's shape) must not
// stop its outer: measured as declared, the order below looks worse (40
// pointer bytes), so a check against the declared layout refused outer in
// the first run and accepted it in the second, once Time had moved.
type Time struct { // want `struct with 24 pointer bytes could be 8`
	wall uint64
	ext  int64
	loc  *int
}

type Outer struct { // want `struct with 32 pointer bytes could be 24`
	T Time
	S string
}
