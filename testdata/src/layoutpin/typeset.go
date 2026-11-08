package layoutpin

import (
	"encoding/binary"
	"io"
	"sync/atomic"
)

// A codec reached through a type parameter encodes every struct of its type
// set.
type tsCodec struct { // want `reorder skipped: encoding/binary call at .*typeset.go:17:`
	tsCodecA bool
	tsCodecB int64
	tsCodecC bool
}

func tsWrite[P interface{ tsCodec }](w io.Writer, v P) { _ = binary.Write(w, binary.LittleEndian, v) }

// An atomically accessed struct held inside an inline struct field still
// places its word by the holder's layout.
type tsCounter struct {
	tsN int64
}

func (c *tsCounter) inc() { atomic.AddInt64(&c.tsN, 1) }

type tsInline struct { // want `reorder skipped: 64-bit atomic access at .*typeset.go:25:`
	tsIn struct{ c tsCounter }
	tsP  *int
}

// A conversion to a type parameter converts to every term of a union.
type tsFrom struct { // want `reorder skipped: conversion at .*typeset.go:51:`
	tsA bool
	tsB int64
	tsC bool
}

type tsU1 struct { // betteralign:ignore
	tsA bool
	tsB int64
	tsC bool
}

type tsU2 struct { // betteralign:ignore
	tsA bool
	tsB int64
	tsC bool
}

func tsUnion[P interface{ tsU1 | tsU2 }](s tsFrom) P { return P(s) }

// The type set reached through an embedded constraint, and through two that
// share one (a diamond), holds the struct a literal of P builds.
type tsEmb struct { // want `reorder skipped: positional composite literal at .*typeset.go:63:`
	tsEA bool
	tsEB int64
	tsEC bool
}

type tsEmbCons interface{ tsEmb }

func tsMakeEmb[P interface{ tsEmbCons }]() P { return P{true, 1, false} }

type tsDia struct { // want `reorder skipped: positional composite literal at .*typeset.go:81:`
	tsDA bool
	tsDB int64
	tsDC bool
}

type (
	tsDiaBase  interface{ tsDia }
	tsDiaLeft  interface{ tsDiaBase }
	tsDiaRight interface{ tsDiaBase }
)

func tsMakeDia[P interface {
	tsDiaLeft
	tsDiaRight
}]() P {
	return P{true, 1, false}
}
