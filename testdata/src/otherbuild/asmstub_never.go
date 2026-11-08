//go:build betteralign_never_set

package otherbuild

// The usual shape of an arch-gated assembly stub (f_arm64.go beside
// f_arm64.s, with a //go:build !arm64 Go fallback): the declarations exist
// only in a file this build excludes, yet the assembly reads their operands'
// fields at fixed offsets.
func asmStep(s *AsmState) int64

func (h *AsmHolder) asmRun() int

// A function with a body pins nothing.
func asmPlain(p *AsmPlain) int64 { return p.AP2 }

// Bound by //go:linkname to a Go implementation: no offset contract.
//
//go:linkname asmLinked runtime.asmLinked
func asmLinked(l *AsmLinkedOther) int
