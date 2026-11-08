package cgofile

// #include <stdlib.h>
import "C"

type L struct { // want `bytes saved: struct of size \d+ could be \d+$`
	A byte
	P *int
	B byte
}

func Use(l L) int { return int(C.abs(1)) + int(l.A) }
