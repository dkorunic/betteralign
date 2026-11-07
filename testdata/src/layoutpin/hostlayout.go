package layoutpin

import "structs"

// structs.HostLayout fixes the C-ABI layout: neither reported nor rewritten.
type Host struct {
	_ structs.HostLayout
	a bool
	b int32
	c bool
}

// An embedded (non-blank) marker counts the same.
type HostEmbedded struct {
	structs.HostLayout
	a bool
	b int32
	c bool
}

type hostAlias = structs.HostLayout

// The marker reached through an alias still counts.
type HostViaAlias struct {
	_ hostAlias
	a bool
	b int32
	c bool
}

// The marker applies only to the struct directly containing it; an outer
// struct holding a HostLayout struct by value is analyzed as usual.
type HostOuter struct { // want `struct of size 20 could be 16`
	a bool
	h Host
	c bool
}

// A local type that merely shares the name is not the marker.
type HostLayout struct{}

type FakeHost struct { // want `struct of size 12 could be 8$`
	_ HostLayout
	a bool
	b int32
	c bool
}
