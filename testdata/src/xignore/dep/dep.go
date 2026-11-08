package dep

type Braced struct { // betteralign:ignore keep struct{} layout
	A byte
	P *int
	B byte
}

type Block struct { // betteralign:ignore
	/* A comes first. */
	A byte
	P *int
	B byte
}
