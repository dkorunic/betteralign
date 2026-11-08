package dep

// Marked is rewritten by its own pass, under -opt_in too.
//
// betteralign:check
type Marked struct { // want `8 bytes saved: struct of size 24 could be 16$`
	A byte
	P *int
	B byte
}

// Plain lacks the mark, so -opt_in leaves it alone.
type Plain struct {
	A byte
	P *int
	B byte
}
