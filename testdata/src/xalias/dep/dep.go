package dep

// An alias of a struct literal is rewritten like a defined type.
type N = struct { // want `bytes saved: struct of size \d+ could be \d+$`
	A byte
	P *int
	B byte
}
