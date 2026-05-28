package testfiles

type TestBad struct { // want "struct of size 12 could be 8"
	x byte
	y int32
	z byte
}
