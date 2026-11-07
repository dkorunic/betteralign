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
