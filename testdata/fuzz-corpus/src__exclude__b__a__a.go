package a

type A struct { // want "8 bytes saved: struct with 16 pointer bytes could be 8"
	a int
	s string
}
