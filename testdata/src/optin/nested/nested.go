package nested

type four struct {
	p, q, r *int
	x       int64
}

// inner is misaligned but not opted in, so -opt_in never rewrites it ...
type inner struct {
	p *int
	a bool
	q *int
}

// ... and outer, which is, measures it as declared: in moves to the front.
//
// betteralign:check
type outer struct { // want `struct with 56 pointer bytes could be 48`
	f  four
	in inner
}
