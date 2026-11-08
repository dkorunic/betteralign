package user

import "optin/xconv/dep"

// betteralign:check
type ToMarked struct { // want `8 bytes saved: struct of size 24 could be 16$`
	A byte
	P *int
	B byte
}

// betteralign:check
type ToPlain struct { // want `reorder skipped: conversion at .*user.go:20:`
	A byte
	P *int
	B byte
}

func marked(t ToMarked) dep.Marked { return dep.Marked(t) }
func plain(t ToPlain) dep.Plain    { return dep.Plain(t) }
