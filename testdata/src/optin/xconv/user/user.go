package user

import "optin/xconv/dep"

// betteralign:check
type ToMarked struct { // want `bytes saved: struct of size \d+ could be \d+$`
	A byte
	P *int
	B byte
}

// betteralign:check
type ToPlain struct { // want `reorder skipped: conversion at .*user.go:27:`
	A byte
	P *int
	B byte
}

// betteralign:check
type ToIgnored struct { // want `reorder skipped: conversion at .*user.go:28:`
	A byte
	P *int
	B byte
}

func marked(t ToMarked) dep.Marked    { return dep.Marked(t) }
func plain(t ToPlain) dep.Plain       { return dep.Plain(t) }
func ignored(t ToIgnored) dep.Ignored { return dep.Ignored(t) }
