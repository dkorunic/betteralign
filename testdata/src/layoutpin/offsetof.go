package layoutpin

import "unsafe"

// Offsetof on a value pins the struct.
type ByValue struct { // want `struct of size 12 could be 8; reorder skipped: unsafe.Offsetof at .*offsetof.go:12:`
	a bool
	b int32
	c bool
}

var _ = unsafe.Offsetof(ByValue{}.b)

// Through a pointer receiver and a parenthesised argument.
type ByPointer struct { // want `struct of size 12 could be 8; reorder skipped: unsafe.Offsetof at`
	a bool
	b int32
	c bool
}

func offPtr(p *ByPointer) uintptr { return unsafe.Offsetof((p.c)) }

// A compile-time offset assertion: exactly what a reorder would break.
type Asserted struct { // want `struct of size 12 could be 8; reorder skipped: unsafe.Offsetof at`
	a bool
	b int32
	c bool
}

var _ [unsafe.Offsetof(Asserted{}.b) - 4]byte

// A promoted field pins the outer struct and every embedded struct on the path.
type Inner struct { // want `struct of size 12 could be 8; reorder skipped: unsafe.Offsetof at .*offsetof.go:51:`
	x bool
	y int32
	z bool
}

type Middle struct { // want `struct of size 20 could be 16; reorder skipped: unsafe.Offsetof at .*offsetof.go:51:`
	m bool
	Inner
	n bool
}

type Outer struct { // want `struct of size 28 could be 24; reorder skipped: unsafe.Offsetof at .*offsetof.go:51:`
	o bool
	Middle
	p bool
}

var _ = unsafe.Offsetof(Outer{}.y)

// The field's own type is not on the path: Leaf is only the type of the
// selected field, so its layout doesn't affect the offset.
type Leaf struct { // want `struct of size 12 could be 8$`
	a bool
	b int32
	c bool
}

type HasLeaf struct {
	l Leaf
	k int32
}

var _ = unsafe.Offsetof(HasLeaf{}.l)

// Sizeof and Alignof don't depend on field order.
type SizedOnly struct { // want `struct of size 12 could be 8$`
	a bool
	b int32
	c bool
}

var (
	_ = unsafe.Sizeof(SizedOnly{})
	_ = unsafe.Alignof(SizedOnly{}.b)
)

// A user function named Offsetof is not the builtin.
type Shadowed struct { // want `struct of size 12 could be 8$`
	a bool
	b int32
	c bool
}

type fake struct{}

func (fake) Offsetof(v any) uintptr { return 0 }

var _ = fake{}.Offsetof(Shadowed{}.b)

// Method values are selections too, but not field selections.
type WithMethod struct { // want `struct of size 12 could be 8$`
	a bool
	b int32
	c bool
}

func (WithMethod) M() {}

var _ = WithMethod{}.M

// Positional literal and Offsetof on one struct: the first in source wins.
type Both struct { // want `reorder skipped: positional composite literal at .*offsetof.go:112:`
	a bool
	b int32
	c bool
}

var (
	_ = Both{true, 1, false}
	_ = unsafe.Offsetof(Both{}.c)
)
