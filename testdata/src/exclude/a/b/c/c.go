package c

type C struct { // want "8 bytes saved: struct with 16 pointer bytes could be 8"
	c int
	s string
}
