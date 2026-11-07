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
