package asmcall

import "unsafe"

// The assembly (asmcall.s) fills an argument block at fixed offsets and
// calls back into Go (github.com/ebitengine/purego's callbackArgs): the
// callback's operand structs are pinned, as for a body-less stub.
type cbArgs struct { // want `reorder skipped: assembly reference at .*asmcall.s:9:7 accesses fields by offset`
	index  uintptr
	args   unsafe.Pointer
	result uintptr
}

func callbackWrap(a *cbArgs) { _ = a }

// Called through a func-typed variable the assembly loads.
type viaVarArgs struct { // want `reorder skipped: assembly reference at .*asmcall.s:8:7 `
	vi uintptr
	vp unsafe.Pointer
	vr uintptr
}

func viaVar(a *viaVarArgs) { _ = a }

var viaVarPC = viaVar

// A variable the assembly reads at an offset.
type asmGlobal struct { // want `reorder skipped: assembly reference at .*asmcall.s:10:7 `
	ga bool
	gb int64
	gc bool
}

var gState asmGlobal

// Named only as another package's symbol (runtime·sameName), in a comment,
// or by assembly under //go:build ignore: still reordered.
type foreignArgs struct { // want `struct with 16 pointer bytes could be 8$`
	fi uintptr
	fp unsafe.Pointer
}

func sameName(a *foreignArgs) { _ = a }

type commentArgs struct { // want `struct with 16 pointer bytes could be 8$`
	ci uintptr
	cp unsafe.Pointer
}

func commented(a *commentArgs) { _ = a }

type ignoredArgs struct { // want `struct with 16 pointer bytes could be 8$`
	ii uintptr
	ip unsafe.Pointer
}

func ignoredCallback(a *ignoredArgs) { _ = a }

// Called only from assembly this build excludes (asmcall_never.s): pinned,
// since that build reads the same layout.
type neverArgs struct { // want `reorder skipped: assembly reference at .*asmcall_never.s:6:7 `
	ni uintptr
	np unsafe.Pointer
}

func neverCallback(a *neverArgs) { _ = a }

// The callback itself is declared only in an excluded file (tagged.go):
// found there by name, its operand resolved here.
type taggedArgs struct { // want `reorder skipped: assembly reference at .*asmcall_never.s:7:7 `
	ti uintptr
	tp unsafe.Pointer
}

// The assembly's own entry point: a body-less stub with no struct operand.
func trampoline(index uintptr)
