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

// Built positionally through a container declared in an excluded file.
type LocalDecl struct { // want `reorder skipped: positional composite literal at .*decl.go`
	LA bool
	LB int64
	LC bool
}

// Built positionally as the element of a generic container's instance.
type GenElem struct { // want `reorder skipped: positional composite literal at .*decl.go`
	GA bool
	GB int64
	GC bool
}

type GenList[E any] []E

// Built positionally through a generic alias's instance.
type AliasElem struct { // want `reorder skipped: positional composite literal at .*decl.go`
	AA bool
	AB int64
	AC bool
}

type GenAlias[T any] = []T

// Built positionally in paths.go through every way an elided literal's type
// is spelled: a generic instance whose argument is declared there, an array,
// a slice of pointers, a map value, a map key, nested slices and a generic
// map instance.
type CvA struct { // want `reorder skipped: positional composite literal at .*paths.go`
	CvA1 bool
	CvA2 int64
	CvA3 bool
}

type CvB struct { // want `reorder skipped: positional composite literal at .*paths.go`
	CvB1 bool
	CvB2 int64
	CvB3 bool
}

type CvC struct { // want `reorder skipped: positional composite literal at .*paths.go`
	CvC1 bool
	CvC2 int64
	CvC3 bool
}

type CvD struct { // want `reorder skipped: positional composite literal at .*paths.go`
	CvD1 bool
	CvD2 int64
	CvD3 bool
}

type CvE struct { // want `reorder skipped: positional composite literal at .*paths.go`
	CvE1 bool
	CvE2 int64
	CvE3 bool
}

type CvF struct { // want `reorder skipped: positional composite literal at .*paths.go`
	CvF1 bool
	CvF2 int64
	CvF3 bool
}

type CvG struct { // want `reorder skipped: positional composite literal at .*paths.go`
	CvG1 bool
	CvG2 int64
	CvG3 bool
}

type (
	CvList[E any]              []E
	CvArr                      [2]CvB
	CvMap[K comparable, V any] map[K]V
)

// Built positionally only in asm_windows.s, which is not Go: no pin.
type AsmOnly struct { // want `struct of size 24 could be 16$`
	AO1 bool
	AO2 int64
	AO3 bool
}

// Taken by a body-less declaration only in asmstub_never.go: its assembly
// reads the fields by offset.
type AsmState struct { // want `reorder skipped: assembly implementation at .*asmstub_never.go`
	AS1 bool
	AS2 int64
	AS3 bool
}

// The receiver of a body-less method there, holding AsmState by value.
type AsmHolder struct { // want `reorder skipped: assembly implementation at .*asmstub_never.go`
	AH1 bool
	AH2 AsmState
	AH3 bool
}

// Passed only to a function with a body in the excluded file: still reordered.
type AsmPlain struct { // want `struct of size 24 could be 16$`
	AP1 bool
	AP2 int64
	AP3 bool
}

// Taken only by a declaration in the excluded file that //go:linkname pulls
// from another package, which keeps its own copy of the layout: pinned.
type AsmLinkedOther struct { // want `reorder skipped: assembly implementation at .*asmstub_never.go`
	AL1 bool
	AL2 int64
	AL3 bool
}

// Taken only by a declaration pulled from this same package: Go code against
// the real layout, so still reordered.
type AsmSelfOther struct { // want `struct of size 24 could be 16$`
	AF1 bool
	AF2 int64
	AF3 bool
}

// The receiver of a body-less method sharing a linknamed function's name: a
// linkname cannot name a method, so pinned.
type AsmShadowOther struct { // want `reorder skipped: assembly implementation at .*asmstub_never.go`
	AW1 bool
	AW2 int64
	AW3 bool
}

// Elements of a named slice taken by an arm64 assembly stub.
type HS []HElem

type HElem struct { // want `reorder skipped: assembly implementation at .*asmstub_arm64.go`
	HE1 bool
	HE2 int64
	HE3 bool
}

// Elements of a named array taken by pointer there.
type Blk [4]ArrElem

type ArrElem struct { // want `reorder skipped: assembly implementation at .*asmstub_arm64.go`
	AE1 bool
	AE2 int64
	AE3 bool
}
