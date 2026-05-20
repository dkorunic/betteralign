package anonymous

// Every anonymous struct below has a misaligned field order; none must be reported.

// var declaration with a misaligned anonymous struct type.
var badVar = struct {
	x byte
	y int32
	z byte
}{}

// Function parameter with a misaligned anonymous struct type.
func badParam(p struct {
	x byte
	y int32
	z byte
}) {
	_ = p
}

// Function return value of misaligned anonymous struct type.
func badReturn() (r struct {
	x byte
	y int32
	z byte
}) {
	return r
}

// Slice element of misaligned anonymous struct type.
var badSlice = []struct {
	x byte
	y int32
	z byte
}{}

// Map value of misaligned anonymous struct type.
var badMap = map[string]struct {
	x byte
	y int32
	z byte
}{}

// Container's inner anonymous struct must not be reported separately.
type Container struct {
	inner struct {
		x byte
		y int32
		z byte
	}
}
