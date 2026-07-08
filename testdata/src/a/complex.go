package a

// ComplexGood is already optimal: complex64 aligns to 4 (like [2]float32),
// so leading with it packs the two bytes into its tail padding.
type ComplexGood struct {
	b complex64
	a byte
	c byte
}

type ComplexBad struct { // want "struct of size 16 could be 12"
	a byte
	b complex64
	c byte
}
