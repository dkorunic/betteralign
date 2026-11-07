package layoutpin

// Cache-line pad against false sharing: the waste is deliberate, so it is
// neither reported nor rewritten.
type CachePadded struct {
	a bool
	_ [56]byte
	b int64
	c bool
}

// A scalar blank pad counts just the same.
type ScalarPad struct {
	a bool
	_ uint32
	b int32
	c bool
}

// A zero-sized blank field before a real pad doesn't hide it.
type TwoPads struct {
	a bool
	_ [0]int64
	b int32
	_ [3]byte
	c bool
	_ [3]byte
}

// Zero-sized blank markers carry no padding and don't count (the trailing
// [0]func() still costs a tail byte, hence 24).
type NoCompare struct { // want `struct of size 24 could be 8$`
	a bool
	b int32
	c bool
	_ [0]func()
}

type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

type NoCopyGuard struct { // want `struct of size 12 could be 8$`
	a bool
	_ noCopy
	b int32
	c bool
}

// A named (non-blank) field is ordinary data, not padding.
type NamedPad struct { // want `struct of size 12 could be 8$`
	a   bool
	pad [2]byte
	b   int32
	c   bool
}
