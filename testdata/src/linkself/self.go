package linkself

// A body-less declaration //go:linkname pulls from this same package is Go
// code compiled against the real layout (the badlinkname self-references in
// reflect, net/http, go/types), not assembly: still reordered.
type Self struct { // want `struct of size 24 could be 16$`
	sa bool
	sb int64
	sc bool
}

//go:linkname linkSelf linkself.linkSelf
func linkSelf(s *Self) int

// Pulled from another package (one whose path merely starts with this one's),
// which keeps its own copy of the layout: pinned.
type Other struct { // want `reorder skipped: assembly implementation at .*self.go:24:`
	oa bool
	ob int64
	oc bool
}

//go:linkname linkOther linkselfx.linkOther
func linkOther(o *Other) int
