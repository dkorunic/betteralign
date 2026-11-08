package dep

// N's fields share a line, a shape dstmin refuses: reported, never rewritten.
type N struct { // want `bytes saved: struct of size \d+ could be \d+$`
	A byte; P *int
	B byte
}
