//go:build betteralign_never_set

package otherbuild

var (
	direct  = Direct{true, 1, false}
	elided  = []Elided{{true, 2, false}}
	pointer = []*Pointer{{true, 3, false}}
	keyed   = Keyed{J: true, K: 4}
)

var (
	slices = Slices{{true, 6, false}}
	nested = Nested{{{true, 7, false}}}
	keys   = Keys{{true, 8, false}: 1}
)
