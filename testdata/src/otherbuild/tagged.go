//go:build betteralign_never_set

package otherbuild

var (
	direct  = Direct{true, 1, false}
	elided  = []Elided{{true, 2, false}}
	pointer = []*Pointer{{true, 3, false}}
	keyed   = Keyed{J: true, K: 4}
)
