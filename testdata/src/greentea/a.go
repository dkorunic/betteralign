//go:build go1.26 && (amd64 || arm64)

package greentea

// The //go:build go1.26 constraint sets ast.File.GoVersion, driving the Green
// Tea scan-work framing of the pointer-bytes diagnostic (see greenTeaGC).
type PointerBad struct { // want "struct with 8008 pointer bytes could be 8 \\(lowers GC scan-work estimate\\)"
	buf [1000]uintptr
	P   *int
}
