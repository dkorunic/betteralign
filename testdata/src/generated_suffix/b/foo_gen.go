// With -generated_files=true the suffix skip is bypassed; the struct is reported.

package generatedsuffix

type SuffixGenBad struct { // want "struct of size 12 could be 8"
	x byte
	y int32
	z byte
}
