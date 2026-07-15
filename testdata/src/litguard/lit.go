package litguard

// T must NOT be rewritten: its only positional literal is in the in-package
// test file, invisible to the base pass (issue #36 sibling). The test variant
// appends "reorder skipped: ..." to the diagnostic; this substring matches both.
type T struct { // want `pointer bytes could be`
	a bool
	b *int
	c int64
}
