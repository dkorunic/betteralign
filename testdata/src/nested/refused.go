package nested

// multiComment is a shape dstmin refuses (a multi-comment group on its own
// line), so it stays as declared: p last, no trailing pointer-free bytes.
type multiComment struct { // want `struct with 24 pointer bytes could be 8`
	a int64
	/* one */ /* two */
	b         int64
	p         *int
}

// holdsMultiComment is already in place for multiComment as it stays.
// Measured as if multiComment moved (16 trailing pointer-free bytes), s went
// in front and the pointer bytes rose from 32 to 40.
type holdsMultiComment struct {
	f multiComment
	s string
}

// crossBrace is refused too: a block comment crosses its opening brace.
type crossBrace struct { /* crossing // want `struct with 24 pointer bytes could be 8`
	 */a int64
	b    int64
	p    *int
}

type holdsCrossBrace struct {
	f crossBrace
	s string
}
