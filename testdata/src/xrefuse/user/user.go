package user

import "xrefuse/dep"

// dep.N stays as declared, so U must too.
type U struct { // want `reorder skipped: conversion at .*user.go:12:`
	A byte
	P *int
	B byte
}

func conv(u U) dep.N { return dep.N(u) }
