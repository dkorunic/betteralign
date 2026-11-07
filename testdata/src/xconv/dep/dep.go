package dep

// U is reordered by this package's own pass, which cannot see the importer
// that converts to it.
type U struct {
	A bool
	B int64
	C bool
}

// These type their values with an unnamed struct: an importer's named struct
// is assignable to them only while its fields stay in this order.
func Param(x struct {
	D bool
	E int64
	F bool
},
) {
}

var Var struct {
	G bool
	H int64
	I bool
}

func Result() (r struct {
	J bool
	K int64
	L bool
},
) {
	return r
}

type Holder struct {
	Field struct {
		M bool
		N int64
		O bool
	}
}

// Named types whose fields or elements are unnamed structs: a positional or
// element literal assigns to them with no field reference to follow.
type Wrapped struct {
	W struct {
		P1 bool
		P2 int64
		P3 bool
	}
}

type List []struct {
	Q1 bool
	Q2 int64
	Q3 bool
}

// Deep is reachable only through Shallow's field: an importer that never
// names Deep can't assign to its unnamed field.
type Shallow struct {
	In Deep
}

type Deep struct {
	F struct {
		Z1 bool
		Z2 int64
		Z3 bool
	}
}

// Four is optimal; Inner is reordered by this package's pass, ending in 8
// pointer-free bytes. NU holds both; measured as the run leaves Inner, NU is
// already in place — and an importer's identical struct converted to NU must
// reach the same verdict, or the conversion breaks.
type Four struct {
	P, Q, R *int
	X       int64
}

type Inner struct {
	P *int
	A bool
	Q *int
}

type NU struct {
	F  Four
	In Inner
}

// Ignored is never reordered by this package's pass, which an importer can
// see in the source: a struct converted to it must stay put.
type Ignored struct { // betteralign:ignore
	I1 bool
	I2 int64
	I3 bool
}

// Unnamed struct elements of an array variable and a channel: assigning an
// element or sending links an importer's struct to them.
var Arr [2]struct {
	R1 bool
	R2 int64
	R3 bool
}

var Ch chan struct {
	S1 bool
	S2 int64
	S3 bool
}
