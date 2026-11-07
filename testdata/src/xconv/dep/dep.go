package dep

// U is reordered by this package's own pass, which cannot see the importer
// that converts to it.
type U struct {
	A bool
	B int64
	C bool
}
