package layoutpin

// A body-less declaration is implemented in assembly, which reads its
// arguments' fields at offsets fixed by today's layout: every struct its
// parameters and results reach (through pointers, slices and arrays) is
// pinned, with the structs it holds by value. Methods count by receiver.
type AsmCtx struct { // want `reorder skipped: assembly implementation at .*asm.go:20:`
	a  bool
	in AsmInner
	b  bool
}

// Held by value inside AsmCtx: part of the bytes the assembly reads.
type AsmInner struct { // want `reorder skipped: assembly implementation at .*asm.go:20:`
	ix bool
	iy int64
	iz bool
}

func asmGet(c *AsmCtx) int

type AsmRecv struct { // want `reorder skipped: assembly implementation at .*asm.go:28:`
	qa bool
	qb int64
	qc bool
}

func (r *AsmRecv) asmLoad() int

// Passed only to a function with a body: still reordered.
type AsmControl struct { // want `struct of size 24 could be 16$`
	ca bool
	cb int64
	cc bool
}

func asmControl(c *AsmControl) int { return int(c.cb) }

// A //go:linkname pull from another package, or a one-argument push, binds
// the declaration to code elsewhere that keeps its own copy of the layout
// (runtime.initTask, the runtime's riscvHWProbePairs) or to assembly: pinned
// all the same, whether the directive is the doc comment or detached.
type AsmLinked struct { // want `reorder skipped: assembly implementation at .*asm.go:50:`
	ka bool
	kb int64
	kc bool
}

//go:linkname asmLinked runtime.asmLinked
func asmLinked(l *AsmLinked) int

type AsmDetached struct { // want `reorder skipped: assembly implementation at .*asm.go:60:`
	da bool
	db int64
	dc bool
}

//go:linkname asmDetached

func asmDetached(d *AsmDetached) int

// A linkname cannot name a method: a body-less method sharing a linknamed
// function's name is still assembly. (A pull from this same package, Go code
// against the real layout, is exempt: fixture linkself, since the copies
// copyTestPackage makes of this package change its path.)
type AsmShadow struct { // want `reorder skipped: assembly implementation at .*asm.go:75:`
	ha bool
	hb int64
	hc bool
}

//go:linkname asmShadow layoutpin.asmShadow
func asmShadow() int

func (h *AsmShadow) asmShadow() int
