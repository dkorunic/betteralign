package user

import "xline/dep"

// dep.N is ignored; the //line directive above it must not hide that.
type U struct { // want `reorder skipped: conversion at .*user.go:12:`
	A byte
	P *int
	B byte
}

func conv(u U) dep.N { return dep.N(u) }
