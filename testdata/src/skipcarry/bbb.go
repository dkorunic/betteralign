// Reached after aaa.go; the per-file skip reset must re-enable analysis here.

package skipcarry

type SkipCarryReal struct { // want "struct of size 12 could be 8"
	x byte
	y int32
	z byte
}
