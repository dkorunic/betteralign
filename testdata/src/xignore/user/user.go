package user

import "xignore/dep"

// Both partners are ignored, so neither side may move.
type U1 struct { // want `reorder skipped: conversion at .*user.go:19:`
	A byte
	P *int
	B byte
}

type U2 struct { // want `reorder skipped: conversion at .*user.go:20:`
	A byte
	P *int
	B byte
}

func conv(u1 U1, u2 U2) (dep.Braced, dep.Block) {
	return dep.Braced(u1),
		dep.Block(u2)
}
