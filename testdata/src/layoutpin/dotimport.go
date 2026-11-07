package layoutpin

import . "unsafe"

// Offsetof reached through a dot-import is still the builtin.
type DotImported struct { // want `struct of size 12 could be 8; reorder skipped: unsafe.Offsetof at .*dotimport.go:12:`
	a bool
	b int32
	c bool
}

var _ = Offsetof(DotImported{}.b)
