package user

import "cgofile"

// cgofile.L sits in a cgo file, which no run rewrites, so U must stay put.
type U struct { // want `reorder skipped: conversion at .*user.go:12:`
	A byte
	P *int
	B byte
}

func conv(u U) cgofile.L { return cgofile.L(u) }
