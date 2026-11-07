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
