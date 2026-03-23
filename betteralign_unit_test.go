package betteralign

// Unit tests for unexported functions: align, gcSizes (Alignof/Sizeof/ptrdata),
// optimalOrder, hasSuffixes, hasGeneratedComment, and hasIgnoreComment.
// Each test is labelled with the BUG-xx it is designed to catch.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"github.com/sirkon/dst"
)

// testSizes64 is a gcSizes configured for a 64-bit platform (WordSize=8, MaxAlign=8).
// All size/alignment expectations in this file assume a 64-bit target.
var testSizes64 = &gcSizes{WordSize: 8, MaxAlign: 8}

// ─── Layer 1: align() (BUG-01) ────────────────────────────────────────────────

// TestAlign verifies the round-up-to-alignment helper.
// BUG-01: using x+a instead of x+a-1 causes already-aligned values to
// overshoot to the next boundary (e.g. align(8,8)==16 instead of 8).
func TestAlign(t *testing.T) {
	tests := []struct {
		name       string
		x, a, want int64
	}{
		// Already-aligned inputs must not move (BUG-01 would overshoot).
		{"already aligned 8/8", 8, 8, 8},
		{"already aligned 4/4", 4, 4, 4},
		{"already aligned 0/8", 0, 8, 0},
		{"already aligned 16/8", 16, 8, 16},
		// Normal round-up cases.
		{"round up 7 to 8", 7, 8, 8},
		{"round up 1 to 4", 1, 4, 4},
		{"round up 9 to 16", 9, 8, 16},
		// Alignment-1 is always already aligned.
		{"align-1 identity", 5, 1, 5},
		{"align-1 zero", 0, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := align(tc.x, tc.a)
			if got != tc.want {
				t.Errorf("align(%d, %d) = %d, want %d", tc.x, tc.a, got, tc.want)
			}
		})
	}
}

// ─── Layer 2: gcSizes.Alignof (BUG-02, BUG-03) ────────────────────────────────

// TestGcSizesAlignofArray verifies that an array's alignment equals its
// element's alignment, not 1.
// BUG-02: returning 1 unconditionally for arrays.
func TestGcSizesAlignofArray(t *testing.T) {
	// [4]uint64 must have alignment 8 (element alignment), not 1.
	arrU64 := types.NewArray(types.Typ[types.Uint64], 4)
	if got := testSizes64.Alignof(arrU64); got != 8 {
		t.Errorf("Alignof([4]uint64) = %d, want 8 (BUG-02 would return 1)", got)
	}

	// [100]bool must have alignment 1 (element alignment).
	arrBool := types.NewArray(types.Typ[types.Bool], 100)
	if got := testSizes64.Alignof(arrBool); got != 1 {
		t.Errorf("Alignof([100]bool) = %d, want 1", got)
	}
}

// TestGcSizesAlignofStruct verifies that a struct's alignment is the maximum
// field alignment, not the minimum.
// BUG-03: using < instead of > in the running-max comparison.
func TestGcSizesAlignofStruct(t *testing.T) {
	// struct { bool; uint64 }: alignment = max(1, 8) = 8.
	// BUG-03 would compute min(1, 8) = 1.
	fields := []*types.Var{
		types.NewVar(token.NoPos, nil, "x", types.Typ[types.Bool]),
		types.NewVar(token.NoPos, nil, "y", types.Typ[types.Uint64]),
	}
	mixedStruct := types.NewStruct(fields, nil)
	if got := testSizes64.Alignof(mixedStruct); got != 8 {
		t.Errorf("Alignof(struct{bool;uint64}) = %d, want 8 (BUG-03 would return 1)", got)
	}

	// Empty struct must have alignment >= 1 (spec).
	emptyStruct := types.NewStruct(nil, nil)
	if got := testSizes64.Alignof(emptyStruct); got < 1 {
		t.Errorf("Alignof(struct{}) = %d, want >= 1", got)
	}
}

// ─── Layer 3: gcSizes.Sizeof (BUG-04 … BUG-08) ────────────────────────────────

// TestGcSizesSizeofString verifies that string is measured as two words.
// BUG-04: returning WordSize (one word) instead of WordSize*2.
func TestGcSizesSizeofString(t *testing.T) {
	got := testSizes64.Sizeof(types.Typ[types.String])
	if got != 16 {
		t.Errorf("Sizeof(string) = %d, want 16 (BUG-04 would return 8)", got)
	}
}

// TestGcSizesSizeofSlice verifies that a slice header is three words.
// BUG-05: returning WordSize*2 (two words) instead of WordSize*3.
func TestGcSizesSizeofSlice(t *testing.T) {
	sliceInt := types.NewSlice(types.Typ[types.Int])
	got := testSizes64.Sizeof(sliceInt)
	if got != 24 {
		t.Errorf("Sizeof([]int) = %d, want 24 (BUG-05 would return 16)", got)
	}
}

// TestGcSizesSizeofArray verifies that array size is element-count × element-size.
// BUG-06: returning element count alone (forgetting to multiply by element size).
func TestGcSizesSizeofArray(t *testing.T) {
	// [8]uint64 = 8 × 8 = 64 bytes; BUG-06 would return 8.
	arr := types.NewArray(types.Typ[types.Uint64], 8)
	if got := testSizes64.Sizeof(arr); got != 64 {
		t.Errorf("Sizeof([8]uint64) = %d, want 64 (BUG-06 would return 8)", got)
	}

	// [3]uint32 = 3 × 4 = 12 bytes.
	arr2 := types.NewArray(types.Typ[types.Uint32], 3)
	if got := testSizes64.Sizeof(arr2); got != 12 {
		t.Errorf("Sizeof([3]uint32) = %d, want 12", got)
	}
}

// TestGcSizesSizeofEmptyStruct verifies that struct{} has size 0.
// BUG-07: removing the o != 0 guard causes struct{} to be assigned size 1.
func TestGcSizesSizeofEmptyStruct(t *testing.T) {
	emptyStruct := types.NewStruct(nil, nil)
	if got := testSizes64.Sizeof(emptyStruct); got != 0 {
		t.Errorf("Sizeof(struct{}) = %d, want 0 (BUG-07 would return 1)", got)
	}
}

// TestGcSizesSizeofTrailingPadding verifies that struct size is rounded up to
// the struct's maximum field alignment (trailing padding is added).
// BUG-08: returning the raw byte offset without the final align() call.
func TestGcSizesSizeofTrailingPadding(t *testing.T) {
	// struct { uint64; bool } = 8 + 1 + 7 padding = 16.
	// BUG-08 would return 9 (no trailing padding).
	fields := []*types.Var{
		types.NewVar(token.NoPos, nil, "x", types.Typ[types.Uint64]),
		types.NewVar(token.NoPos, nil, "y", types.Typ[types.Bool]),
	}
	strType := types.NewStruct(fields, nil)
	if got := testSizes64.Sizeof(strType); got != 16 {
		t.Errorf("Sizeof(struct{uint64;bool}) = %d, want 16 (BUG-08 would return 9)", got)
	}

	// struct { uint32; bool } = 4 + 1 + 3 padding = 8.
	fields2 := []*types.Var{
		types.NewVar(token.NoPos, nil, "x", types.Typ[types.Uint32]),
		types.NewVar(token.NoPos, nil, "y", types.Typ[types.Bool]),
	}
	strType2 := types.NewStruct(fields2, nil)
	if got := testSizes64.Sizeof(strType2); got != 8 {
		t.Errorf("Sizeof(struct{uint32;bool}) = %d, want 8", got)
	}
}

// ─── Layer 4: gcSizes.ptrdata (BUG-09 … BUG-12) ──────────────────────────────

// TestGcSizesPtrdataString verifies that string contributes one pointer word.
// BUG-09: treating string as non-pointer-bearing (returning 0).
func TestGcSizesPtrdataString(t *testing.T) {
	got := testSizes64.ptrdata(types.Typ[types.String])
	if got != 8 {
		t.Errorf("ptrdata(string) = %d, want 8 (BUG-09 would return 0)", got)
	}
}

// TestGcSizesPtrdataInterface verifies that an interface contributes two pointer words.
// BUG-10: returning WordSize (one word) instead of 2×WordSize.
func TestGcSizesPtrdataInterface(t *testing.T) {
	iface := types.NewInterfaceType(nil, nil)
	got := testSizes64.ptrdata(iface)
	if got != 16 {
		t.Errorf("ptrdata(interface{}) = %d, want 16 (BUG-10 would return 8)", got)
	}
}

// TestGcSizesPtrdataArray verifies the array ptrdata formula: (n-1)*stride + elem_ptrdata.
// BUG-11: using n instead of n-1, overestimating by one element's stride.
func TestGcSizesPtrdataArray(t *testing.T) {
	// [3]*int: (3-1)*8 + 8 = 24.
	// BUG-11 would compute 3*8 + 8 = 32.
	ptrInt := types.NewPointer(types.Typ[types.Int])
	arr := types.NewArray(ptrInt, 3)
	if got := testSizes64.ptrdata(arr); got != 24 {
		t.Errorf("ptrdata([3]*int) = %d, want 24 (BUG-11 would return 32)", got)
	}

	// [1]*int: (1-1)*8 + 8 = 8.
	arr1 := types.NewArray(ptrInt, 1)
	if got := testSizes64.ptrdata(arr1); got != 8 {
		t.Errorf("ptrdata([1]*int) = %d, want 8", got)
	}

	// [4]int (no pointers): ptrdata = 0.
	arrInt := types.NewArray(types.Typ[types.Int], 4)
	if got := testSizes64.ptrdata(arrInt); got != 0 {
		t.Errorf("ptrdata([4]int) = %d, want 0", got)
	}
}

// TestGcSizesPtrdataStructOffset verifies that struct ptrdata records the pointer
// extent using the field's offset before advancing, not after.
// BUG-12: advancing o += sz before recording p = o + fp, shifting all pointer
// extents by one field's size.
func TestGcSizesPtrdataStructOffset(t *testing.T) {
	// struct { *int; int }:
	//   field 0 (*int) at offset 0: fp=8 → p = 0 + 8 = 8, then o = 8
	//   field 1 (int)  at offset 8: fp=0 → p unchanged = 8
	// ptrdata = 8.
	// BUG-12 would report p = (0+8) + 8 = 16.
	ptrInt := types.NewPointer(types.Typ[types.Int])
	fields := []*types.Var{
		types.NewVar(token.NoPos, nil, "ptr", ptrInt),
		types.NewVar(token.NoPos, nil, "val", types.Typ[types.Int]),
	}
	strType := types.NewStruct(fields, nil)
	if got := testSizes64.ptrdata(strType); got != 8 {
		t.Errorf("ptrdata(struct{*int;int}) = %d, want 8 (BUG-12 would return 16)", got)
	}
}

// ─── Layer 5: optimalOrder sort comparator (BUG-13 … BUG-17) ─────────────────

// TestOptimalOrderZeroSizedFirst verifies that zero-sized fields sort before
// non-zero-sized fields.
// BUG-13: returning zeroj instead of zeroi places zero-sized fields last.
func TestOptimalOrderZeroSizedFirst(t *testing.T) {
	// struct { int32; struct{} }: struct{} (zero-sized) must sort first.
	emptyStruct := types.NewStruct(nil, nil)
	fields := []*types.Var{
		types.NewVar(token.NoPos, nil, "x", types.Typ[types.Int32]),
		types.NewVar(token.NoPos, nil, "z", emptyStruct),
	}
	strType := types.NewStruct(fields, nil)
	_, indexes := optimalOrder(strType, testSizes64)

	if len(indexes) != 2 {
		t.Fatalf("expected 2 indexes, got %d", len(indexes))
	}
	// indexes[0] == 1 means struct{} (original index 1) is placed first.
	if indexes[0] != 1 {
		t.Errorf("zero-sized field should be first: indexes[0] = %d, want 1 (BUG-13 places it last)", indexes[0])
	}
}

// TestOptimalOrderHighAlignmentFirst verifies that higher-alignment fields sort
// before lower-alignment fields.
// BUG-14: using < instead of > in the alignment comparison.
func TestOptimalOrderHighAlignmentFirst(t *testing.T) {
	// struct { bool; uint64 }: uint64 (align=8) must precede bool (align=1).
	fields := []*types.Var{
		types.NewVar(token.NoPos, nil, "b", types.Typ[types.Bool]),
		types.NewVar(token.NoPos, nil, "u", types.Typ[types.Uint64]),
	}
	strType := types.NewStruct(fields, nil)
	_, indexes := optimalOrder(strType, testSizes64)

	if len(indexes) != 2 {
		t.Fatalf("expected 2 indexes, got %d", len(indexes))
	}
	// indexes[0] == 1 means uint64 (original index 1) is placed first.
	if indexes[0] != 1 {
		t.Errorf("uint64 (align=8) should be first: indexes[0] = %d, want 1 (BUG-14 inverts order)", indexes[0])
	}
}

// TestOptimalOrderPointerBearingFirst verifies that pointer-bearing fields sort
// before pointer-free fields of the same alignment.
// BUG-15: returning noptrsi instead of noptrsj swaps the placement.
func TestOptimalOrderPointerBearingFirst(t *testing.T) {
	// struct { uint64; *int }: both have alignment 8.
	// *int  (ptrdata=8) must precede uint64 (ptrdata=0).
	ptrInt := types.NewPointer(types.Typ[types.Int])
	fields := []*types.Var{
		types.NewVar(token.NoPos, nil, "u", types.Typ[types.Uint64]),
		types.NewVar(token.NoPos, nil, "p", ptrInt),
	}
	strType := types.NewStruct(fields, nil)
	_, indexes := optimalOrder(strType, testSizes64)

	if len(indexes) != 2 {
		t.Fatalf("expected 2 indexes, got %d", len(indexes))
	}
	// indexes[0] == 1 means *int (original index 1) is placed first.
	if indexes[0] != 1 {
		t.Errorf("*int (pointer-bearing) should be first: indexes[0] = %d, want 1 (BUG-15 inverts)", indexes[0])
	}
}

// TestOptimalOrderFewerTrailingFirst verifies that among pointer-bearing fields,
// the one with fewer trailing non-pointer bytes sorts first.
// BUG-16: using > instead of < inverts the trailing-bytes comparison.
func TestOptimalOrderFewerTrailingFirst(t *testing.T) {
	// *int:   sizeof=8,  ptrdata=8,  trailing = 8-8 = 0
	// string: sizeof=16, ptrdata=8,  trailing = 16-8 = 8
	// *int (trailing=0) must precede string (trailing=8).
	ptrInt := types.NewPointer(types.Typ[types.Int])
	fields := []*types.Var{
		types.NewVar(token.NoPos, nil, "s", types.Typ[types.String]),
		types.NewVar(token.NoPos, nil, "p", ptrInt),
	}
	strType := types.NewStruct(fields, nil)
	_, indexes := optimalOrder(strType, testSizes64)

	if len(indexes) != 2 {
		t.Fatalf("expected 2 indexes, got %d", len(indexes))
	}
	// indexes[0] == 1 means *int (original index 1) is placed first.
	if indexes[0] != 1 {
		t.Errorf("*int (trailing=0) should be first: indexes[0] = %d, want 1 (BUG-16 inverts)", indexes[0])
	}
}

// TestOptimalOrderLargerSizeFirst verifies that, as a final tiebreaker, larger
// fields sort before smaller fields when all other criteria are equal.
// BUG-17: using < instead of > places smaller fields first.
func TestOptimalOrderLargerSizeFirst(t *testing.T) {
	// uint32:    sizeof=4, align=4, ptrdata=0
	// [2]uint32: sizeof=8, align=4, ptrdata=0
	// [2]uint32 (larger) must precede uint32 (smaller).
	arr2u32 := types.NewArray(types.Typ[types.Uint32], 2)
	fields := []*types.Var{
		types.NewVar(token.NoPos, nil, "u", types.Typ[types.Uint32]),
		types.NewVar(token.NoPos, nil, "a", arr2u32),
	}
	strType := types.NewStruct(fields, nil)
	_, indexes := optimalOrder(strType, testSizes64)

	if len(indexes) != 2 {
		t.Fatalf("expected 2 indexes, got %d", len(indexes))
	}
	// indexes[0] == 1 means [2]uint32 (original index 1) is placed first.
	if indexes[0] != 1 {
		t.Errorf("[2]uint32 (larger) should be first: indexes[0] = %d, want 1 (BUG-17 inverts)", indexes[0])
	}
}

// ─── Layer 7: hasSuffixes (BUG-21, BUG-22) ────────────────────────────────────

// TestHasSuffixesCacheConsistency verifies that repeated calls with the same
// filename return the same result, i.e. the cache is not inverted.
// BUG-21: returning !t from the cache hit branch inverts results after the first call.
func TestHasSuffixesCacheConsistency(t *testing.T) {
	suffixes := []string{"_test.go"}

	t.Run("matching file stays true on repeated calls", func(t *testing.T) {
		fset := make(map[string]bool)
		if !hasSuffixes(fset, "foo_test.go", suffixes) {
			t.Error("first call: expected true for _test.go file")
		}
		// BUG-21 would return false on the second call.
		if !hasSuffixes(fset, "foo_test.go", suffixes) {
			t.Error("second call: cache inversion detected (BUG-21)")
		}
		// A third call should also be consistent.
		if !hasSuffixes(fset, "foo_test.go", suffixes) {
			t.Error("third call: cache inversion detected (BUG-21)")
		}
	})

	t.Run("non-matching file stays false on repeated calls", func(t *testing.T) {
		fset := make(map[string]bool)
		if hasSuffixes(fset, "foo.go", suffixes) {
			t.Error("first call: expected false for regular .go file")
		}
		// BUG-21 would return true on the second call.
		if hasSuffixes(fset, "foo.go", suffixes) {
			t.Error("second call: cache inversion detected (BUG-21)")
		}
	})
}

// TestHasSuffixesNonMatchingCachedFalse verifies that non-matching files are
// cached as false, not as true.
// BUG-22: unconditionally writing fset[fn] = true before the loop caches every
// file as matching, making subsequent calls wrongly return true.
func TestHasSuffixesNonMatchingCachedFalse(t *testing.T) {
	suffixes := []string{"_test.go"}
	fset := make(map[string]bool)

	// First call: non-matching file must return false.
	if hasSuffixes(fset, "regular.go", suffixes) {
		t.Error("expected false for non-matching file")
	}

	// The cache entry for the file must be false (BUG-22 caches it as true).
	cached, ok := fset["regular.go"]
	if !ok {
		t.Error("file should be cached after the first call")
	}
	if cached {
		t.Error("non-matching file was cached as true (BUG-22)")
	}

	// Second call using the cache must also return false.
	if hasSuffixes(fset, "regular.go", suffixes) {
		t.Error("second call: cached non-matching file returned true (BUG-22)")
	}
}

// ─── Layer 7: hasGeneratedComment (BUG-23) ────────────────────────────────────

// parseTestFile is a helper that parses src as Go source and returns the *ast.File.
func parseTestFile(t *testing.T, src string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	return f
}

// TestHasGeneratedComment verifies that the generated-file marker is detected
// only when it appears before the package keyword.
// BUG-23: using < instead of > causes the function to stop at comments that
// precede the package keyword, never inspecting them.
func TestHasGeneratedComment(t *testing.T) {
	t.Run("generated comment before package is detected", func(t *testing.T) {
		// The canonical "DO NOT EDIT" header must be recognised.
		// BUG-23 would return false because the guard would fire immediately.
		f := parseTestFile(t, "// Code generated by foo. DO NOT EDIT.\npackage foo\n")
		if !hasGeneratedComment(make(map[string]bool), "test.go", f) {
			t.Error("generated comment before package keyword not detected (BUG-23)")
		}
	})

	t.Run("no generated comment returns false", func(t *testing.T) {
		f := parseTestFile(t, "// Regular comment.\npackage foo\n")
		if hasGeneratedComment(make(map[string]bool), "test.go", f) {
			t.Error("non-generated comment should not be detected")
		}
	})

	t.Run("generated comment after package keyword is not detected", func(t *testing.T) {
		// Comments that appear after the package keyword are not headers and
		// must be ignored.
		f := parseTestFile(t, "package foo\n// Code generated by foo. DO NOT EDIT.\n")
		if hasGeneratedComment(make(map[string]bool), "test.go", f) {
			t.Error("generated comment after package keyword should not be detected")
		}
	})

	t.Run("positive detection populates the cache map", func(t *testing.T) {
		f := parseTestFile(t, "// Code generated by foo. DO NOT EDIT.\npackage foo\n")
		fset := make(map[string]bool)
		hasGeneratedComment(fset, "test.go", f)
		if !fset["test.go"] {
			t.Error("generated file should be cached in fset after detection")
		}
	})
}

// ─── Layer 7/8: hasIgnoreComment (BUG-24, BUG-25) ────────────────────────────

// TestHasIgnoreCommentOpening verifies that the betteralign:ignore directive is
// read from the Opening decoration of the field list, not from other positions.
// BUG-24: checking End (closing-brace area) instead of Opening means the
// annotation is never found.
func TestHasIgnoreCommentOpening(t *testing.T) {
	t.Run("ignore comment in Opening is detected", func(t *testing.T) {
		fl := &dst.FieldList{}
		fl.Decs.Opening = dst.Decorations{"// betteralign:ignore"}
		if !hasIgnoreComment(fl) {
			t.Error("betteralign:ignore in Opening not detected (BUG-24)")
		}
	})

	t.Run("ignore comment in End (node tail) is NOT detected", func(t *testing.T) {
		// BUG-24 moves the check to a different decoration position;
		// comments placed there should not trigger ignore.
		fl := &dst.FieldList{}
		fl.Decs.End = dst.Decorations{"// betteralign:ignore"}
		if hasIgnoreComment(fl) {
			t.Error("betteralign:ignore in End should not trigger ignore (BUG-24 would wrongly trigger)")
		}
	})

	t.Run("ignore comment in Start (node head) is NOT detected", func(t *testing.T) {
		fl := &dst.FieldList{}
		fl.Decs.Start = dst.Decorations{"// betteralign:ignore"}
		if hasIgnoreComment(fl) {
			t.Error("betteralign:ignore in Start should not trigger ignore")
		}
	})

	t.Run("no decorations returns false", func(t *testing.T) {
		fl := &dst.FieldList{}
		if hasIgnoreComment(fl) {
			t.Error("empty field list should return false")
		}
	})

	t.Run("unrelated comment in Opening does not trigger", func(t *testing.T) {
		fl := &dst.FieldList{}
		fl.Decs.Opening = dst.Decorations{"// some other comment"}
		if hasIgnoreComment(fl) {
			t.Error("unrelated Opening comment should not trigger ignore")
		}
	})
}

// TestHasIgnoreCommentPrefixGuard verifies that only line comments (// prefix)
// can carry the betteralign:ignore directive, not block comments or bare strings.
// BUG-25: removing the HasPrefix("//") guard allows block comments and other
// strings that merely contain the directive substring to trigger ignore.
func TestHasIgnoreCommentPrefixGuard(t *testing.T) {
	t.Run("line comment triggers ignore", func(t *testing.T) {
		fl := &dst.FieldList{}
		fl.Decs.Opening = dst.Decorations{"// betteralign:ignore"}
		if !hasIgnoreComment(fl) {
			t.Error("line comment with // prefix should trigger ignore")
		}
	})

	t.Run("block comment does NOT trigger ignore", func(t *testing.T) {
		// BUG-25 removes HasPrefix("//") so block comments would also match.
		fl := &dst.FieldList{}
		fl.Decs.Opening = dst.Decorations{"/* betteralign:ignore */"}
		if hasIgnoreComment(fl) {
			t.Error("block comment should NOT trigger ignore (BUG-25 would wrongly trigger)")
		}
	})

	t.Run("bare string containing directive does NOT trigger ignore", func(t *testing.T) {
		// Without the // prefix guard any decoration string containing the
		// magic substring would match — BUG-25 risk.
		fl := &dst.FieldList{}
		fl.Decs.Opening = dst.Decorations{"betteralign:ignore"}
		if hasIgnoreComment(fl) {
			t.Error("bare string without // prefix should NOT trigger ignore (BUG-25 would wrongly trigger)")
		}
	})

	t.Run("partial match without directive does NOT trigger", func(t *testing.T) {
		fl := &dst.FieldList{}
		fl.Decs.Opening = dst.Decorations{"// betteralign:checked"}
		if hasIgnoreComment(fl) {
			t.Error("partial directive match should not trigger ignore")
		}
	})
}
