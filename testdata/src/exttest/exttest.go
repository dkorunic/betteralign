package exttest

// T is built positionally only by the external test package, which loads as
// another package: reordering it stops exttest_test from compiling.
type T struct { // want `reorder skipped: positional composite literal at .*exttest_test.go:12:3 would break`
	A byte
	B *int
	C byte
}

// E is built positionally as an elided slice element.
type E struct { // want `reorder skipped: positional composite literal at .*exttest_test.go:13:11 would break`
	A byte
	B *int
	C byte
}

// K is built keyed: reordered as usual.
type K struct { // want `struct of size 24 could be 16$`
	A byte
	B *int
	C byte
}
