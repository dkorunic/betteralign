package user

import "xconv/dep"

// T converts to dep.U, which the same run reorders in its own package. Both
// sort to the same order, so T must be reordered too — pinning T alone is
// what would break the conversion (net/http's HTTP2Config and
// net/http/internal/http2.Config).
type T struct { // want `struct of size 24 could be 16$`
	A bool
	B int64
	C bool
}

func toDep(t T) dep.U { return dep.U(t) }

// Two local types converted to each other are still pinned.
type L1 struct { // want `reorder skipped: conversion at`
	A bool
	B int64
	C bool
}

type L2 struct { // want `reorder skipped: conversion at`
	A bool
	B int64
	C bool
}

var _ = L2(L1{})

// Assignable to unnamed struct types declared in dep, so pinned: reordering
// these would break the calls and assignments below.
type P struct { // want `reorder skipped: identical unnamed struct type at .*dep.go`
	D bool
	E int64
	F bool
}

type V struct { // want `reorder skipped: identical unnamed struct type at .*dep.go`
	G bool
	H int64
	I bool
}

type R struct { // want `reorder skipped: identical unnamed struct type at .*dep.go`
	J bool
	K int64
	L bool
}

type H struct { // want `reorder skipped: identical unnamed struct type at .*dep.go`
	M bool
	N int64
	O bool
}

func assign(p P, v V, h H) R {
	dep.Param(p)
	dep.Var = v
	var x dep.Holder
	x.Field = h
	return dep.Result()
}
