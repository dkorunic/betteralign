//go:build ignore

// A generator for another package: its literals are not this package's.
package main

type Direct struct{ X, Y, Z int }

var _ = Direct{1, 2, 3}

var _ = OnlyGen{true, 1, false}

type OnlyGen struct{ M, N, O int }
