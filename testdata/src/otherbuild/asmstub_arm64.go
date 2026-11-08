//go:build arm64

package otherbuild

// Assembly operands of a named slice or array type: the assembly reads the
// element structs by offset.
func asmHS(x HS) int

func asmBlk(b *Blk) int
