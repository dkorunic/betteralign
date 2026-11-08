package dep

import "strings"

var prefix = strings.ToUpper("x")

func Prefix() string { return prefix }

// Marked is rewritten by its own pass, under -opt_in too.
//
// betteralign:check
type Marked struct { // want `bytes saved: struct of size \d+ could be \d+$`
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

// Ignored keeps its order whatever the mark says.
//
// betteralign:check
type Ignored struct { // betteralign:ignore
	A byte
	P *int
	B byte
}
