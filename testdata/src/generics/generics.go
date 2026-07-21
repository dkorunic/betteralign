package generics

// Pair's layout depends on T: betteralign sizes T from the constraint
// underlying (interface{}), so any size figure is a per-instantiation guess.
// It must not be reported, so this struct deliberately carries no expectation
// comment.
type Pair[T any] struct {
	A bool
	B T
	C bool
}

// ArrayOfParam is instantiation-dependent through a by-value array element, so
// it is suppressed for the same reason and also carries no expectation comment.
type ArrayOfParam[T any] struct {
	A bool
	B [4]T
}

// Plain is a non-generic misaligned struct: it must still be reported, proving
// the suppression targets type-param layouts rather than generic types wholesale.
type Plain struct { // want `struct of size 24 could be 16`
	a bool
	b int64
	c bool
}

// PointerParam's fields have a fixed layout regardless of T (a pointer is one
// word), so its misalignment is real for every instantiation and must be
// reported.
type PointerParam[T any] struct { // want `struct of size 24 could be 16`
	a bool
	b *T
	c bool
}
