package b

// betteralign:check
type B struct { // want "8 bytes saved: struct with 16 pointer bytes could be 8"
	b int
	s string
}
