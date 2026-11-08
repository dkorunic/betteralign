//go:build betteralign_never_set

package otherbuild

// Containers declared only in this excluded file, mixed with the package's
// own, so an elided literal resolves through both.
type (
	cvSs   []CvA
	cvTbl  []*CvC
	cvM    map[string]CvD
	cvRows []cvRow
	cvRow  []CvF
)

var (
	cvGenOfDecl = CvList[cvSs]{{{true, 1, false}}}
	cvArr       = CvArr{{true, 2, false}, {false, 3, true}}
	cvPtrs      = cvTbl{{true, 4, false}}
	cvMap       = cvM{"k": {true, 5, false}}
	cvKeys      = map[CvE]int{{true, 6, false}: 1}
	cvNested    = cvRows{{{true, 7, false}}}
	cvGenMap    = CvMap[string, CvG]{"k": {true, 8, false}}
)
