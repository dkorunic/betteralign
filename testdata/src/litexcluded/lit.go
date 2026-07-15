package litexcluded

// T MUST be reordered: its only positional literal is in a build-excluded test
// file that never compiles, so it can't break. The deferral must key on loaded
// files, not a disk glob that ignores build constraints.
type T struct { // want `pointer bytes could be`
	a bool
	b *int
	c int64
}
