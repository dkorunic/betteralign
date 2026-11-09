package exttest_test

import (
	"testing"

	ext "exttest"
)

// Renamed import: the qualifier is ext, not exttest.
func TestLiterals(t *testing.T) {
	_ = []any{
		ext.T{1, nil, 2},
		[]ext.E{{1, nil, 2}},
		ext.K{A: 1, C: 2},
	}
}
