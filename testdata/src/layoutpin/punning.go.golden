package layoutpin

import "unsafe"

// Punning one indirection further in: *(*[]T)(p) reads a slice header whose
// elements are T (runtime/metrics.Value seen as the runtime's metricValue),
// and **T a pointer to T, so T's layout is what the memory holds. Both
// directions, as for *T.
type punElem struct { // want `reorder skipped: unsafe.Pointer conversion at .*punning.go:15:`
	ea bool
	eb int64
	ec bool
}

func punSlice(p unsafe.Pointer) []punElem { return *(*[]punElem)(p) }

type punPtr struct { // want `reorder skipped: unsafe.Pointer conversion at .*punning.go:23:`
	pa bool
	pb int64
	pc bool
}

func punPtrPtr(p unsafe.Pointer) *punPtr { return *(**punPtr)(p) }

type punOut struct { // want `reorder skipped: unsafe.Pointer conversion at .*punning.go:31:`
	oa bool
	ob int64
	oc bool
}

func punFrom(s []punOut) unsafe.Pointer { return unsafe.Pointer(&s) }
