// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package betteralign

// Unit tests for unexported helpers. BUG-xx cases pin specific historical mutants.

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dst "github.com/dkorunic/betteralign/internal/dstmin"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ast/inspector"
)

// testSizes64 is the 64-bit gcSizes used by every size/alignment expectation here.
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

	// Cases where element-size ≠ element-alignment, to distinguish Alignof from Sizeof.
	arrInt8 := types.NewArray(types.Typ[types.Int8], 8)
	if got := testSizes64.Alignof(arrInt8); got != 1 {
		t.Errorf("Alignof([8]int8) = %d, want 1 (Sizeof-mutation would return 8)", got)
	}

	inner := types.NewArray(types.Typ[types.Int8], 8)
	outer := types.NewArray(inner, 4)
	if got := testSizes64.Alignof(outer); got != 1 {
		t.Errorf("Alignof([4][8]int8) = %d, want 1", got)
	}
}

// TestGcSizesAlignofStruct verifies that a struct's alignment is the maximum
// field alignment, not the minimum.
// BUG-03: using < instead of > in the running-max comparison.
func TestGcSizesAlignofStruct(t *testing.T) {
	// struct{bool;uint64}: max(1,8) = 8 (BUG-03 would yield min = 1).
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
	// struct{uint64;bool} = 16 with trailing padding (BUG-08 would return 9).
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
	// [3]*int = (3-1)*8 + 8 = 24 (BUG-11 uses n instead of n-1 → 32).
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
	// struct{*int;int}: ptrdata = 8 (BUG-12 would report 16 by advancing o before recording p).
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

// ─── Layer 4.5: gcSizes cycle safety (BUG-29) ────────────────────────────────

// TestGcSizesCycleSafety pins the sentinel-pre-population in Alignof / Sizeof
// / ptrdata. The trigger is a self-embedding struct whose composite-literal
// use forces go/types to materialise the struct type instead of replacing
// it with Invalid. Without the literal, the type-checker's cycle detector
// rewrites e to *types.Basic[Invalid] and the bug stays dormant.
//
// BUG-29: cache filled only after recursion returns, so the in-progress call
// re-enters itself, recurses without bound, and exhausts the goroutine
// stack. Found by FuzzOptimalOrder; trigger pinned as fuzz seed
// testdata/fuzz/FuzzOptimalOrder/bug29_recursive_struct.
//
// The watchdog guards against a regression hanging the whole test binary:
// the goroutine running optimalOrder still leaks until the process exits,
// but the test result is deterministic.
func TestGcSizesCycleSafety(t *testing.T) {
	const src = `package p
type e struct {
	e
}
var _ = e{}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "in.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	conf := types.Config{Error: func(error) {}, Importer: nil}
	pkg, _ := conf.Check("p", fset, []*ast.File{file}, nil)
	if pkg == nil {
		t.Fatal("nil package from type-check")
	}
	tn, ok := pkg.Scope().Lookup("e").(*types.TypeName)
	if !ok {
		t.Fatal("type e missing from scope")
	}
	named, ok := tn.Type().(*types.Named)
	if !ok {
		t.Fatalf("e.Type() = %T, want *types.Named", tn.Type())
	}
	st, ok := named.Origin().Underlying().(*types.Struct)
	if !ok {
		t.Skipf("type-checker no longer admits self-embedding here (Underlying = %T); BUG-29 trigger neutralised upstream", named.Underlying())
	}
	if st.NumFields() == 0 || st.Field(0).Type() != named {
		t.Skip("type-checker no longer makes field[0] self-referential; BUG-29 trigger neutralised upstream")
	}

	sizes := newGCSizes(8, 8)
	type result struct {
		indexes             []int
		optSize, optPtrdata int64
	}
	resCh := make(chan result, 1)
	go func() {
		indexes, optSize, optPtrdata := optimalOrder(st, sizes)
		resCh <- result{indexes, optSize, optPtrdata}
	}()
	var res result
	select {
	case res = <-resCh:
	case <-time.After(5 * time.Second):
		t.Fatal("BUG-29: optimalOrder did not terminate within 5s on a self-embedding struct (cache sentinel missing)")
	}

	if len(res.indexes) != st.NumFields() {
		t.Errorf("len(indexes) = %d, want %d", len(res.indexes), st.NumFields())
	}
	seen := make([]bool, st.NumFields())
	for _, i := range res.indexes {
		if i < 0 || i >= st.NumFields() {
			t.Errorf("index %d out of range [0,%d)", i, st.NumFields())
			continue
		}
		if seen[i] {
			t.Errorf("index %d appears twice", i)
		}
		seen[i] = true
	}
	if origSize := sizes.Sizeof(st); res.optSize > origSize {
		t.Errorf("optSize=%d > origSize=%d", res.optSize, origSize)
	}
	if origPtrdata := sizes.ptrdata(st); res.optPtrdata > origPtrdata {
		t.Errorf("optPtrdata=%d > origPtrdata=%d", res.optPtrdata, origPtrdata)
	}
}

// ─── Layer 4.6: gcSizes overflow safety (BUG-30) ─────────────────────────────

// TestGcSizesArrayOverflowSaturates pins saturation in Sizeof / ptrdata array paths.
// BUG-30: raw int64 multiply on huge arrays wrapped Sizeof to MinInt64.
// Fuzz seed testdata/fuzz/FuzzGCSizes/d713d410fe8c6747 is the corpus-level regression.
func TestGcSizesArrayOverflowSaturates(t *testing.T) {
	sizes := newGCSizes(8, 8)

	huge := types.NewArray(types.Typ[types.Uint64], 1<<60)
	if got := sizes.Sizeof(huge); got < 0 {
		t.Errorf("Sizeof saturating: got %d (negative), want MaxInt64", got)
	}

	hugePtrs := types.NewArray(types.NewPointer(types.Typ[types.Int]), 1<<60)
	sz := sizes.Sizeof(hugePtrs)
	pd := sizes.ptrdata(hugePtrs)
	if pd < 0 || sz < 0 {
		t.Errorf("array of pointers overflow: Sizeof=%d ptrdata=%d, want both non-negative", sz, pd)
	}
	if pd > sz {
		t.Errorf("invariant violation: ptrdata=%d > Sizeof=%d", pd, sz)
	}
}

// TestAlignSaturationBoundary pins the `(a-1)` off-by-one in align()'s overflow guard.
func TestAlignSaturationBoundary(t *testing.T) {
	cases := []struct {
		name string
		x, a int64
		want int64
	}{
		// MaxInt64-3 with a=4: result is MaxInt64-3 (no saturation); off-by-one mutation saturates.
		{"boundary x=MaxInt64-3 a=4", math.MaxInt64 - 3, 4, math.MaxInt64 - 3},
		{"saturates x=MaxInt64-2 a=4", math.MaxInt64 - 2, 4, math.MaxInt64},
		{"already aligned MaxInt64-7 a=8", math.MaxInt64 - 7, 8, math.MaxInt64 - 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := align(tc.x, tc.a); got != tc.want {
				t.Errorf("align(%d, %d) = %d, want %d", tc.x, tc.a, got, tc.want)
			}
		})
	}
}

// TestAddSizeArgOrder pins the operand pairing in addSize's `a > MaxInt64-b` guard.
// An asymmetric (a, b) distinguishes the baseline from the `MaxInt64-a` typo.
func TestAddSizeArgOrder(t *testing.T) {
	const a int64 = math.MaxInt64 - 20
	const b int64 = 10
	const want = math.MaxInt64 - 10

	if got := addSize(a, b); got != want {
		t.Errorf("addSize(%d, %d) = %d, want %d (typo `a > MaxInt64-a` would saturate)", a, b, got, want)
	}
}

// TestMulSizeSaturates pins the saturating-multiply helper directly.
func TestMulSizeSaturates(t *testing.T) {
	cases := []struct {
		name    string
		n, size int64
		want    int64
	}{
		{"normal", 10, 8, 80},
		{"zero size", 100, 0, 0},
		{"zero n", 0, 100, 0},
		{"negative n", -1, 100, 0},
		{"overflow saturates", 1 << 60, 1 << 10, math.MaxInt64},
		{"exact MaxInt64", math.MaxInt64, 1, math.MaxInt64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mulSize(tc.n, tc.size); got != tc.want {
				t.Errorf("mulSize(%d, %d) = %d, want %d", tc.n, tc.size, got, tc.want)
			}
		})
	}
}

// TestAddSizeSaturates pins the saturating-add helper directly.
func TestAddSizeSaturates(t *testing.T) {
	cases := []struct {
		name string
		a, b int64
		want int64
	}{
		{"normal", 10, 20, 30},
		{"overflow saturates", math.MaxInt64 - 5, 10, math.MaxInt64},
		{"max + zero", math.MaxInt64, 0, math.MaxInt64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := addSize(tc.a, tc.b); got != tc.want {
				t.Errorf("addSize(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
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
	indexes, _, _ := optimalOrder(strType, testSizes64)

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
	indexes, _, _ := optimalOrder(strType, testSizes64)

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
	// Same alignment 8; *int (pointer-bearing) must precede uint64.
	ptrInt := types.NewPointer(types.Typ[types.Int])
	fields := []*types.Var{
		types.NewVar(token.NoPos, nil, "u", types.Typ[types.Uint64]),
		types.NewVar(token.NoPos, nil, "p", ptrInt),
	}
	strType := types.NewStruct(fields, nil)
	indexes, _, _ := optimalOrder(strType, testSizes64)

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
	// *int (trailing=0) must precede string (trailing=8) under the fewer-trailing rule.
	ptrInt := types.NewPointer(types.Typ[types.Int])
	fields := []*types.Var{
		types.NewVar(token.NoPos, nil, "s", types.Typ[types.String]),
		types.NewVar(token.NoPos, nil, "p", ptrInt),
	}
	strType := types.NewStruct(fields, nil)
	indexes, _, _ := optimalOrder(strType, testSizes64)

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
	// Same align/ptrdata; [2]uint32 (larger) must precede uint32 as final tiebreak.
	arr2u32 := types.NewArray(types.Typ[types.Uint32], 2)
	fields := []*types.Var{
		types.NewVar(token.NoPos, nil, "u", types.Typ[types.Uint32]),
		types.NewVar(token.NoPos, nil, "a", arr2u32),
	}
	strType := types.NewStruct(fields, nil)
	indexes, _, _ := optimalOrder(strType, testSizes64)

	if len(indexes) != 2 {
		t.Fatalf("expected 2 indexes, got %d", len(indexes))
	}
	// indexes[0] == 1 means [2]uint32 (original index 1) is placed first.
	if indexes[0] != 1 {
		t.Errorf("[2]uint32 (larger) should be first: indexes[0] = %d, want 1 (BUG-17 inverts)", indexes[0])
	}
}

// ─── Layer 6: hasSuffix ──────────────────────────────────────────────────────

// TestHasSuffix verifies the suffix matcher used to skip test or generated
// files by filename. The previous cached variant (hasSuffixes) was retired in
// favour of a single per-file call from the visitor.
func TestHasSuffix(t *testing.T) {
	suffixes := []string{"_test.go", "_generated.go", ".pb.go"}
	tests := []struct {
		name string
		fn   string
		want bool
	}{
		{"matches _test.go", "foo_test.go", true},
		{"matches .pb.go", "rpc.pb.go", true},
		{"matches _generated.go", "schema_generated.go", true},
		{"no match plain .go", "foo.go", false},
		{"no match different suffix", "foo_tests.go", false},
		{"empty filename", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasSuffix(tc.fn, suffixes); got != tc.want {
				t.Errorf("hasSuffix(%q) = %v, want %v", tc.fn, got, tc.want)
			}
		})
	}
}

// ─── Layer 7: hasGeneratedComment (BUG-23) ───────────────────────────────────

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
		// Canonical "DO NOT EDIT" header (BUG-23 would short-circuit the guard).
		f := parseTestFile(t, "// Code generated by foo. DO NOT EDIT.\npackage foo\n")
		if !hasGeneratedComment(f) {
			t.Error("generated comment before package keyword not detected (BUG-23)")
		}
	})

	t.Run("no generated comment returns false", func(t *testing.T) {
		f := parseTestFile(t, "// Regular comment.\npackage foo\n")
		if hasGeneratedComment(f) {
			t.Error("non-generated comment should not be detected")
		}
	})

	t.Run("generated comment after package keyword is not detected", func(t *testing.T) {
		// Post-package comments are not headers and must be ignored.
		f := parseTestFile(t, "package foo\n// Code generated by foo. DO NOT EDIT.\n")
		if hasGeneratedComment(f) {
			t.Error("generated comment after package keyword should not be detected")
		}
	})
}

// ─── Layer 8: hasIgnoreComment (BUG-24, BUG-25) ──────────────────────────────

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
		// BUG-24 would also fire on End-decorations; this asserts it doesn't.
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
		// Without the // prefix guard a bare substring match would fire (BUG-25).
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

// ─── Layer 8b: hasIgnoreCommentAST (DST-independent ignore) ──────────────────

// firstStructType parses src (with comments) and returns its fileset, file,
// and the first *ast.StructType in preorder.
func firstStructType(t *testing.T, src string) (*token.FileSet, *ast.File, *ast.StructType) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var st *ast.StructType
	ast.Inspect(f, func(n ast.Node) bool {
		if s, ok := n.(*ast.StructType); ok && st == nil {
			st = s
			return false
		}
		return true
	})
	if st == nil {
		t.Fatal("no struct type in fixture")
	}
	return fset, f, st
}

// TestHasIgnoreCommentAST pins the DST-independent ignore check: it honors the
// directive only on the opening-brace line (matching hasIgnoreComment's
// Opening routing), so it agrees with the DST path on decoratable structs
// while still working for shapes dstmin cannot decorate.
func TestHasIgnoreCommentAST(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"brace-line directive", "package p\ntype S struct { // betteralign:ignore\n\ta byte\n\tb int64\n}\n", true},
		{"directive on first-field line (lead-doc, not Opening)", "package p\ntype S struct {\n\t// betteralign:ignore\n\ta byte\n\tb int64\n}\n", false},
		{"unrelated brace-line comment", "package p\ntype S struct { // hello\n\ta byte\n\tb int64\n}\n", false},
		{"no comment", "package p\ntype S struct {\n\ta byte\n\tb int64\n}\n", false},
		{"block comment does not trigger", "package p\ntype S struct { /* betteralign:ignore */\n\ta byte\n\tb int64\n}\n", false},
		// Trailing comment is after the brace, outside the body: not honored.
		{"single-line trailing directive not honored", "package p\ntype S struct { a byte; b int64 } // betteralign:ignore\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset, f, st := firstStructType(t, tc.src)
			if got := hasIgnoreCommentAST(fset, f, st); got != tc.want {
				t.Errorf("hasIgnoreCommentAST = %v, want %v", got, tc.want)
			}
		})
	}
}

// ─── Layer 9: optimalOrder direct size/ptrdata (P5) ──────────────────────────

// TestOptimalOrderSizeAndPtrdata verifies that the size and ptrdata returned
// directly by optimalOrder match gcSizes.Sizeof / gcSizes.ptrdata computed on
// the same fields re-built in optimal order. The direct computation must
// produce identical numbers to the indirect path for every input.
func TestOptimalOrderSizeAndPtrdata(t *testing.T) {
	ptrInt := types.NewPointer(types.Typ[types.Int])
	arr2u32 := types.NewArray(types.Typ[types.Uint32], 2)
	emptyStruct := types.NewStruct(nil, nil)
	cases := []struct {
		name   string
		fields []*types.Var
	}{
		{"bool_then_uint64", []*types.Var{
			types.NewVar(token.NoPos, nil, "b", types.Typ[types.Bool]),
			types.NewVar(token.NoPos, nil, "u", types.Typ[types.Uint64]),
		}},
		{"three_mixed", []*types.Var{
			types.NewVar(token.NoPos, nil, "x", types.Typ[types.Bool]),
			types.NewVar(token.NoPos, nil, "y", types.Typ[types.Int32]),
			types.NewVar(token.NoPos, nil, "z", types.Typ[types.Uint64]),
		}},
		{"string_then_pointer", []*types.Var{
			types.NewVar(token.NoPos, nil, "s", types.Typ[types.String]),
			types.NewVar(token.NoPos, nil, "p", ptrInt),
		}},
		{"zero_sized_field_present", []*types.Var{
			types.NewVar(token.NoPos, nil, "x", types.Typ[types.Int32]),
			types.NewVar(token.NoPos, nil, "z", emptyStruct),
		}},
		{"equal_align_size_tiebreak", []*types.Var{
			types.NewVar(token.NoPos, nil, "u", types.Typ[types.Uint32]),
			types.NewVar(token.NoPos, nil, "a", arr2u32),
		}},
		{"single_field", []*types.Var{
			types.NewVar(token.NoPos, nil, "u", types.Typ[types.Uint64]),
		}},
		{"empty_struct", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			str := types.NewStruct(tc.fields, nil)
			indexes, optSize, optPtrdata := optimalOrder(str, testSizes64)
			if len(indexes) != len(tc.fields) {
				t.Fatalf("indexes len = %d, want %d", len(indexes), len(tc.fields))
			}
			ordered := make([]*types.Var, len(indexes))
			for i, idx := range indexes {
				ordered[i] = str.Field(idx)
			}
			optStruct := types.NewStruct(ordered, nil)
			if got := testSizes64.Sizeof(optStruct); got != optSize {
				t.Errorf("size mismatch: optimalOrder=%d, Sizeof(optimalStruct)=%d", optSize, got)
			}
			if got := testSizes64.ptrdata(optStruct); got != optPtrdata {
				t.Errorf("ptrdata mismatch: optimalOrder=%d, ptrdata(optimalStruct)=%d", optPtrdata, got)
			}
		})
	}
}

// ─── Layer 10: applyToFile (BUG-28) ──────────────────────────────────────────

// TestApplyToFileSuccess writes content to a pre-existing file and verifies
// the file contents and mode are preserved as expected. The error paths are
// covered by TestApplyToFileSentinelsAreWrapped below.
func TestApplyToFileSuccess(t *testing.T) {
	dir := t.TempDir()
	fn := filepath.Join(dir, "target.go")
	const initialContent = "package x\nvar X = 1\n"
	const newContent = "package x\nvar X = 2\n"
	// Use a non-default mode so we can verify it survives the write.
	const wantMode os.FileMode = 0o640

	if err := os.WriteFile(fn, []byte(initialContent), wantMode); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := applyToFile(fn, []byte(newContent)); err != nil {
		t.Fatalf("applyToFile: %v", err)
	}

	got, err := os.ReadFile(fn)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != newContent {
		t.Errorf("file content mismatch:\n got=%q\nwant=%q", got, newContent)
	}

	info, err := os.Stat(fn)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != wantMode.Perm() {
		t.Errorf("file mode after write = %o, want %o", info.Mode().Perm(), wantMode.Perm())
	}
}

// TestApplyToFileSentinelsAreWrapped verifies that errors returned by
// applyToFile chain to their sentinel via errors.Is.
// BUG-28: formatting sentinels with %v instead of %w makes errors.Is checks
// silently return false, breaking the public error API.
func TestApplyToFileSentinelsAreWrapped(t *testing.T) {
	t.Run("non-existent file wraps ErrStatFile", func(t *testing.T) {
		err := applyToFile(filepath.Join(t.TempDir(), "does-not-exist.go"), []byte("package x\n"))
		if err == nil {
			t.Fatal("expected error for missing file, got nil")
		}
		if !errors.Is(err, ErrStatFile) {
			t.Errorf("errors.Is(err, ErrStatFile) = false; err = %v (BUG-28)", err)
		}
	})

	t.Run("directory wraps ErrNotRegularFile", func(t *testing.T) {
		dir := t.TempDir()
		err := applyToFile(dir, []byte("package x\n"))
		if err == nil {
			t.Fatal("expected error for directory path, got nil")
		}
		if !errors.Is(err, ErrNotRegularFile) {
			t.Errorf("errors.Is(err, ErrNotRegularFile) = false; err = %v (BUG-28)", err)
		}
	})

	t.Run("symlink to regular file wraps ErrNotRegularFile", func(t *testing.T) {
		// Lstat must observe the symlink, not its target.
		dir := t.TempDir()
		target := filepath.Join(dir, "target.go")
		if err := os.WriteFile(target, []byte("package x\n"), 0o644); err != nil {
			t.Fatalf("seed target: %v", err)
		}
		link := filepath.Join(dir, "link.go")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		err := applyToFile(link, []byte("package y\n"))
		if err == nil {
			t.Fatal("expected error for symlink path, got nil")
		}
		if !errors.Is(err, ErrNotRegularFile) {
			t.Errorf("errors.Is(err, ErrNotRegularFile) = false; err = %v", err)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read target after refused write: %v", err)
		}
		if string(got) != "package x\n" {
			t.Errorf("symlink target was modified: got %q, want %q", got, "package x\n")
		}
	})
}

// ─── Layer 11: StringArrayFlag.Set (BUG-26) ──────────────────────────────────

// TestStringArrayFlagSetEmptyValues verifies that StringArrayFlag.Set never
// appends empty strings.
// BUG-26: strings.Split(value, ",") yields a single empty entry for "" and
// adjacent empty entries for "a,", ",a", and "a,,b". An empty entry in
// excludeDirs makes filepath.Rel(".", dir) succeed for every file, silently
// excluding the entire tree from analysis.
func TestStringArrayFlagSetEmptyValues(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"empty string yields no entries", "", nil},
		{"single non-empty value", "a", []string{"a"}},
		{"trailing comma drops empty tail", "a,", []string{"a"}},
		{"leading comma drops empty head", ",a", []string{"a"}},
		{"adjacent commas drop empty middle", "a,,b", []string{"a", "b"}},
		{"only commas yields no entries", ",,", nil},
		{"surrounding whitespace trimmed", " a , b ", []string{"a", "b"}},
		{"tab whitespace trimmed", "\ta\t,\tb\t", []string{"a", "b"}},
		{"whitespace-only entries dropped", " , a , ", []string{"a"}},
		{"internal whitespace preserved", "my path,other path", []string{"my path", "other path"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var f StringArrayFlag
			if err := f.Set(tc.input); err != nil {
				t.Fatalf("Set(%q) returned error: %v", tc.input, err)
			}
			if len(f) != len(tc.want) {
				t.Fatalf("Set(%q): got %d entries %v, want %d entries %v",
					tc.input, len(f), []string(f), len(tc.want), tc.want)
			}
			for i := range f {
				if f[i] != tc.want[i] {
					t.Errorf("Set(%q)[%d] = %q, want %q", tc.input, i, f[i], tc.want[i])
				}
			}
		})
	}
}

// ─── Layer 12: commentGroupHasOptIn / isExcluded / commentHasDirective ───────

// makeCommentGroup builds an *ast.CommentGroup from comment text strings.
// Each string must include its // or /* */ markers.
func makeCommentGroup(texts ...string) *ast.CommentGroup {
	cg := &ast.CommentGroup{}
	for _, t := range texts {
		cg.List = append(cg.List, &ast.Comment{Text: t})
	}
	return cg
}

// TestCommentGroupHasOptInPrefix verifies that only line comments with the
// betteralign:check directive as a separate token (word boundary) trigger
// opt-in. Block comments, substring matches, and missing // prefix must not.
// BUG-27: substring matching (strings.Contains) without a // prefix guard and
// without a word-boundary check accepts block comments and partial directives,
// inconsistent with hasIgnoreComment.
func TestCommentGroupHasOptInPrefix(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"nil group", "", false},
		{"line comment with directive", "// betteralign:check", true},
		{"line comment with directive and trailing text", "// betteralign:check explanation", true},
		{"line comment with directive and tab separator", "// betteralign:check\texplanation", true},
		{"line comment with extra spaces before directive", "//   betteralign:check", true},
		{"block comment is rejected", "/* betteralign:check */", false},
		{"bare string without // prefix is rejected", "betteralign:check", false},
		{"substring suffix is rejected (checked)", "// betteralign:checked", false},
		{"substring prefix is rejected", "// xbetteralign:check", false},
		{"unrelated comment", "// some other comment", false},
		{"empty line comment", "//", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var cg *ast.CommentGroup
			if tc.text != "" || tc.name == "empty line comment" {
				cg = makeCommentGroup(tc.text)
			}
			got := commentGroupHasOptIn(cg)
			if got != tc.want {
				t.Errorf("commentGroupHasOptIn(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// TestHasIgnoreCommentPrefix verifies that hasIgnoreComment also enforces the
// word-boundary rule. The pre-fix code matched "// betteralign:ignored" as a
// substring of betteralign:ignore.
func TestHasIgnoreCommentPrefix(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"directive matches", "// betteralign:ignore", true},
		{"directive with trailing text matches", "// betteralign:ignore reason", true},
		{"directive with trailing tab matches", "// betteralign:ignore\treason", true},
		{"longer-token suffix is rejected", "// betteralign:ignored", false},
		{"longer-token prefix is rejected", "// xbetteralign:ignore", false},
		{"block comment rejected", "/* betteralign:ignore */", false},
		{"bare string rejected", "betteralign:ignore", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fl := &dst.FieldList{}
			fl.Decs.Opening = dst.Decorations{tc.text}
			got := hasIgnoreComment(fl)
			if got != tc.want {
				t.Errorf("hasIgnoreComment(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// TestIsExcluded directly exercises the exclude_dirs / exclude_files matcher
// without spinning up analysistest. Integration tests cover the wiring; these
// pin down per-pattern semantics.
func TestIsExcluded(t *testing.T) {
	wd := filepath.Join(string(filepath.Separator), "proj")
	tests := []struct {
		name  string
		fn    string
		dirs  []string
		files []string
		want  bool
	}{
		{
			name: "no patterns is not excluded",
			fn:   filepath.Join(wd, "foo.go"),
			want: false,
		},
		{
			name: "file in excluded dir is excluded",
			fn:   filepath.Join(wd, "sub", "foo.go"),
			dirs: []string{"sub"},
			want: true,
		},
		{
			name: "file outside excluded dir is not excluded",
			fn:   filepath.Join(wd, "other", "foo.go"),
			dirs: []string{"sub"},
			want: false,
		},
		{
			name: "trailing slash on excludeDir still matches",
			fn:   filepath.Join(wd, "sub", "foo.go"),
			dirs: []string{"sub" + string(filepath.Separator)},
			want: true,
		},
		{
			name: "nested file under excluded dir is excluded",
			fn:   filepath.Join(wd, "sub", "deep", "foo.go"),
			dirs: []string{"sub"},
			want: true,
		},
		{
			name:  "file matching glob pattern is excluded",
			fn:    filepath.Join(wd, "foo.go"),
			files: []string{"*.go"},
			want:  true,
		},
		{
			name:  "file not matching glob is not excluded",
			fn:    filepath.Join(wd, "foo.go"),
			files: []string{"*.txt"},
			want:  false,
		},
		{
			name:  "qualified path glob matches",
			fn:    filepath.Join(wd, "sub", "foo.go"),
			files: []string{filepath.Join("sub", "*.go")},
			want:  true,
		},
		{
			name:  "any matching pattern wins (dir match)",
			fn:    filepath.Join(wd, "sub", "foo.go"),
			dirs:  []string{"other", "sub"},
			files: []string{"*.txt"},
			want:  true,
		},
		{
			name:  "any matching pattern wins (file match)",
			fn:    filepath.Join(wd, "foo.go"),
			dirs:  []string{"sub"},
			files: []string{"*.txt", "*.go"},
			want:  true,
		},
		{
			name:  "bad glob pattern is treated as excluded",
			fn:    filepath.Join(wd, "foo.go"),
			files: []string{"[unclosed"},
			want:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isExcluded(wd, tc.fn, tc.dirs, tc.files)
			if got != tc.want {
				t.Errorf("isExcluded(%q, %q) with dirs=%v files=%v = %v, want %v",
					wd, tc.fn, tc.dirs, tc.files, got, tc.want)
			}
		})
	}
}

// TestCommentHasDirective exercises the shared //-comment / word-boundary
// matcher directly. hasIgnoreComment and commentGroupHasOptIn both delegate to
// it; this table makes the contract explicit rather than relying on indirect
// coverage through the two callers.
func TestCommentHasDirective(t *testing.T) {
	const directive = "betteralign:check"
	tests := []struct {
		name      string
		comment   string
		directive string
		want      bool
	}{
		{"bare directive", "// betteralign:check", directive, true},
		{"directive then space then text", "// betteralign:check reason", directive, true},
		{"directive then tab then text", "// betteralign:check\treason", directive, true},
		{"leading spaces preserved", "//   betteralign:check", directive, true},
		{"no space after //", "//betteralign:check", directive, true},
		{"block comment rejected", "/* betteralign:check */", directive, false},
		{"missing // prefix", "betteralign:check", directive, false},
		{"directive followed by colon (no boundary)", "// betteralign:check:extra", directive, false},
		{"directive as substring suffix (checked)", "// betteralign:checked", directive, false},
		{"directive as substring prefix", "// xbetteralign:check", directive, false},
		{"unrelated comment", "// hello world", directive, false},
		{"empty body", "//", directive, false},
		{"empty input", "", directive, false},
		{"different directive does not match", "// betteralign:ignore", directive, false},
		{"matches ignore directive", "// betteralign:ignore", "betteralign:ignore", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := commentHasDirective(tc.comment, tc.directive)
			if got != tc.want {
				t.Errorf("commentHasDirective(%q, %q) = %v, want %v", tc.comment, tc.directive, got, tc.want)
			}
		})
	}
}

// TestStringArrayFlagSetAccumulates verifies that successive Set calls append.
// Both forms (repeated flag use and comma-separated values) must accumulate
// without inserting empty entries.
func TestStringArrayFlagSetAccumulates(t *testing.T) {
	var f StringArrayFlag
	if err := f.Set("a,b"); err != nil {
		t.Fatalf("first Set returned error: %v", err)
	}
	if err := f.Set("c"); err != nil {
		t.Fatalf("second Set returned error: %v", err)
	}
	if err := f.Set(""); err != nil {
		t.Fatalf("third Set returned error: %v", err)
	}
	want := []string{"a", "b", "c"}
	if len(f) != len(want) {
		t.Fatalf("got %v, want %v", []string(f), want)
	}
	for i := range f {
		if f[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, f[i], want[i])
		}
	}
}

// ─── Layer 13: gcSizes.Alignof additional branches ───────────────────────────

// TestGcSizesAlignofPointer verifies pointer alignment is one word.
func TestGcSizesAlignofPointer(t *testing.T) {
	if got := testSizes64.Alignof(types.NewPointer(types.Typ[types.Int])); got != 8 {
		t.Errorf("Alignof(*int) = %d, want 8", got)
	}
}

// TestGcSizesAlignofSlice verifies a slice header aligns at MaxAlign (the
// 24-byte sizeof is capped down).
func TestGcSizesAlignofSlice(t *testing.T) {
	if got := testSizes64.Alignof(types.NewSlice(types.Typ[types.Int])); got != 8 {
		t.Errorf("Alignof([]int) = %d, want 8 (capped by MaxAlign)", got)
	}
}

// TestGcSizesAlignofInterface verifies the MaxAlign cap also applies to
// interfaces (sizeof=16 > MaxAlign=8).
func TestGcSizesAlignofInterface(t *testing.T) {
	if got := testSizes64.Alignof(types.NewInterfaceType(nil, nil)); got != 8 {
		t.Errorf("Alignof(interface{}) = %d, want 8 (capped by MaxAlign)", got)
	}
}

// TestGcSizesAlignofMap verifies map alignment is one word.
func TestGcSizesAlignofMap(t *testing.T) {
	m := types.NewMap(types.Typ[types.String], types.Typ[types.Int])
	if got := testSizes64.Alignof(m); got != 8 {
		t.Errorf("Alignof(map[string]int) = %d, want 8", got)
	}
}

// TestGcSizesAlignofChan verifies channel alignment is one word.
func TestGcSizesAlignofChan(t *testing.T) {
	c := types.NewChan(types.SendRecv, types.Typ[types.Int])
	if got := testSizes64.Alignof(c); got != 8 {
		t.Errorf("Alignof(chan int) = %d, want 8", got)
	}
}

// TestGcSizesAlignofSignature verifies function-value alignment is one word.
func TestGcSizesAlignofSignature(t *testing.T) {
	sig := types.NewSignatureType(nil, nil, nil, nil, nil, false)
	if got := testSizes64.Alignof(sig); got != 8 {
		t.Errorf("Alignof(func()) = %d, want 8", got)
	}
}

// TestGcSizesAlignofMaxAlignCap verifies the cap fires when Sizeof exceeds
// MaxAlign. A deliberately tiny MaxAlign (2) reveals the cap on a uint64
// (sizeof=8): without the cap the result would be 8.
func TestGcSizesAlignofMaxAlignCap(t *testing.T) {
	sizes := newGCSizes(8, 2)
	if got := sizes.Alignof(types.Typ[types.Uint64]); got != 2 {
		t.Errorf("Alignof(uint64) with MaxAlign=2 = %d, want 2 (cap)", got)
	}
}

// TestGcSizesAlignofNestedArrayOfStruct verifies Alignof recurses into array
// element types: [3]struct{uint64} aligns to 8 because its element struct
// aligns to 8.
func TestGcSizesAlignofNestedArrayOfStruct(t *testing.T) {
	innerFields := []*types.Var{
		types.NewVar(token.NoPos, nil, "x", types.Typ[types.Uint64]),
	}
	inner := types.NewStruct(innerFields, nil)
	arr := types.NewArray(inner, 3)
	if got := testSizes64.Alignof(arr); got != 8 {
		t.Errorf("Alignof([3]struct{uint64}) = %d, want 8", got)
	}
}

// ─── Layer 14: gcSizes.Sizeof additional branches ────────────────────────────

// TestGcSizesSizeofBasicKinds covers all *types.Basic kinds, both the ones
// looked up in the basicSizes table and the ones (Int/Uint/Uintptr) that hit
// the catch-all WordSize return.
func TestGcSizesSizeofBasicKinds(t *testing.T) {
	tests := []struct {
		name string
		kind types.BasicKind
		want int64
	}{
		{"bool", types.Bool, 1},
		{"int8", types.Int8, 1},
		{"int16", types.Int16, 2},
		{"int32", types.Int32, 4},
		{"int64", types.Int64, 8},
		{"uint8", types.Uint8, 1},
		{"uint16", types.Uint16, 2},
		{"uint32", types.Uint32, 4},
		{"uint64", types.Uint64, 8},
		{"float32", types.Float32, 4},
		{"float64", types.Float64, 8},
		{"complex64", types.Complex64, 8},
		{"complex128", types.Complex128, 16},
		{"int", types.Int, 8},         // catch-all WordSize
		{"uint", types.Uint, 8},       // catch-all WordSize
		{"uintptr", types.Uintptr, 8}, // catch-all WordSize
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := testSizes64.Sizeof(types.Typ[tc.kind])
			if got != tc.want {
				t.Errorf("Sizeof(%s) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

// TestGcSizesSizeofInterface verifies the interface special case (2 words).
func TestGcSizesSizeofInterface(t *testing.T) {
	if got := testSizes64.Sizeof(types.NewInterfaceType(nil, nil)); got != 16 {
		t.Errorf("Sizeof(interface{}) = %d, want 16", got)
	}
}

// TestGcSizesSizeofPointerShapedTypes verifies that pointer, channel, map,
// and signature types fall into the catch-all WordSize arm.
func TestGcSizesSizeofPointerShapedTypes(t *testing.T) {
	tests := []struct {
		name string
		typ  types.Type
	}{
		{"*int", types.NewPointer(types.Typ[types.Int])},
		{"chan int", types.NewChan(types.SendRecv, types.Typ[types.Int])},
		{"map[string]int", types.NewMap(types.Typ[types.String], types.Typ[types.Int])},
		{"func()", types.NewSignatureType(nil, nil, nil, nil, nil, false)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := testSizes64.Sizeof(tc.typ); got != 8 {
				t.Errorf("Sizeof(%s) = %d, want 8 (WordSize)", tc.name, got)
			}
		})
	}
}

// TestGcSizesSizeofTrailingZeroSizedBumps verifies the trailing zero-sized
// field gets one byte of padding when preceded by non-zero-sized fields. The
// runtime needs this so &struct.lastField is unique per instance.
func TestGcSizesSizeofTrailingZeroSizedBumps(t *testing.T) {
	emptyStruct := types.NewStruct(nil, nil)

	bumped := types.NewStruct([]*types.Var{
		types.NewVar(token.NoPos, nil, "x", types.Typ[types.Uint32]),
		types.NewVar(token.NoPos, nil, "z", emptyStruct),
	}, nil)
	if got := testSizes64.Sizeof(bumped); got != 8 {
		t.Errorf("Sizeof(struct{uint32;struct{}}) = %d, want 8 (trailing zero-sized must bump)", got)
	}

	leading := types.NewStruct([]*types.Var{
		types.NewVar(token.NoPos, nil, "z", emptyStruct),
		types.NewVar(token.NoPos, nil, "x", types.Typ[types.Uint32]),
	}, nil)
	if got := testSizes64.Sizeof(leading); got != 4 {
		t.Errorf("Sizeof(struct{struct{};uint32}) = %d, want 4 (leading zero-sized must NOT bump)", got)
	}

	allEmpty := types.NewStruct([]*types.Var{
		types.NewVar(token.NoPos, nil, "a", emptyStruct),
		types.NewVar(token.NoPos, nil, "b", emptyStruct),
	}, nil)
	if got := testSizes64.Sizeof(allEmpty); got != 0 {
		t.Errorf("Sizeof(struct{struct{};struct{}}) = %d, want 0 (no field with offset!=0)", got)
	}
}

// TestGcSizesSizeofNestedStruct verifies a struct field that is itself a
// struct contributes its own sizeof, including trailing padding.
func TestGcSizesSizeofNestedStruct(t *testing.T) {
	inner := types.NewStruct([]*types.Var{
		types.NewVar(token.NoPos, nil, "x", types.Typ[types.Uint32]),
		types.NewVar(token.NoPos, nil, "y", types.Typ[types.Bool]),
	}, nil)
	outer := types.NewStruct([]*types.Var{
		types.NewVar(token.NoPos, nil, "i", inner),
		types.NewVar(token.NoPos, nil, "u", types.Typ[types.Uint64]),
	}, nil)
	if got := testSizes64.Sizeof(outer); got != 16 {
		t.Errorf("Sizeof(struct{inner;uint64}) = %d, want 16", got)
	}
}

// ─── Layer 15: gcSizes.ptrdata additional branches ───────────────────────────

// TestGcSizesPtrdataNonPointerBasic verifies that all non-pointer basic types
// (including uintptr, which is NOT scanned by the garbage collector) return
// zero pointer bytes.
func TestGcSizesPtrdataNonPointerBasic(t *testing.T) {
	tests := []struct {
		name string
		kind types.BasicKind
	}{
		{"bool", types.Bool},
		{"int", types.Int},
		{"int32", types.Int32},
		{"uint64", types.Uint64},
		{"float64", types.Float64},
		{"complex128", types.Complex128},
		{"uintptr", types.Uintptr},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := testSizes64.ptrdata(types.Typ[tc.kind]); got != 0 {
				t.Errorf("ptrdata(%s) = %d, want 0", tc.name, got)
			}
		})
	}
}

// TestGcSizesPtrdataUnsafePointer verifies that unsafe.Pointer (the only
// pointer-bearing *types.Basic besides string) returns one word.
func TestGcSizesPtrdataUnsafePointer(t *testing.T) {
	if got := testSizes64.ptrdata(types.Typ[types.UnsafePointer]); got != 8 {
		t.Errorf("ptrdata(unsafe.Pointer) = %d, want 8", got)
	}
}

// TestGcSizesPtrdataPointerShapedTypes verifies every pointer-shaped
// *types.Type kind in the explicit ptrdata case (Chan, Map, Pointer,
// Signature, Slice) returns one word.
func TestGcSizesPtrdataPointerShapedTypes(t *testing.T) {
	tests := []struct {
		name string
		typ  types.Type
	}{
		{"*int", types.NewPointer(types.Typ[types.Int])},
		{"chan int", types.NewChan(types.SendRecv, types.Typ[types.Int])},
		{"map[string]int", types.NewMap(types.Typ[types.String], types.Typ[types.Int])},
		{"func()", types.NewSignatureType(nil, nil, nil, nil, nil, false)},
		{"[]int", types.NewSlice(types.Typ[types.Int])},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := testSizes64.ptrdata(tc.typ); got != 8 {
				t.Errorf("ptrdata(%s) = %d, want 8", tc.name, got)
			}
		})
	}
}

// TestGcSizesPtrdataZeroLengthArray verifies a [0]*T array has zero ptrdata,
// guarding the early-exit before the (n-1)*z + a formula (which would yield
// a negative value for n==0).
func TestGcSizesPtrdataZeroLengthArray(t *testing.T) {
	arr := types.NewArray(types.NewPointer(types.Typ[types.Int]), 0)
	if got := testSizes64.ptrdata(arr); got != 0 {
		t.Errorf("ptrdata([0]*int) = %d, want 0", got)
	}
}

// TestGcSizesPtrdataArrayOfNonPointerElem verifies the elem-ptrdata==0
// early-exit: even a long array of int contributes nothing.
func TestGcSizesPtrdataArrayOfNonPointerElem(t *testing.T) {
	arr := types.NewArray(types.Typ[types.Int], 1024)
	if got := testSizes64.ptrdata(arr); got != 0 {
		t.Errorf("ptrdata([1024]int) = %d, want 0", got)
	}
}

// TestGcSizesPtrdataEmptyStruct verifies an empty struct has zero ptrdata.
func TestGcSizesPtrdataEmptyStruct(t *testing.T) {
	if got := testSizes64.ptrdata(types.NewStruct(nil, nil)); got != 0 {
		t.Errorf("ptrdata(struct{}) = %d, want 0", got)
	}
}

// TestGcSizesPtrdataStructNoPointers verifies a struct of non-pointer fields
// has zero ptrdata regardless of size.
func TestGcSizesPtrdataStructNoPointers(t *testing.T) {
	strType := types.NewStruct([]*types.Var{
		types.NewVar(token.NoPos, nil, "x", types.Typ[types.Int32]),
		types.NewVar(token.NoPos, nil, "y", types.Typ[types.Uint64]),
		types.NewVar(token.NoPos, nil, "z", types.Typ[types.Float64]),
	}, nil)
	if got := testSizes64.ptrdata(strType); got != 0 {
		t.Errorf("ptrdata(struct{int32;uint64;float64}) = %d, want 0", got)
	}
}

// TestGcSizesPtrdataMidStructPointer verifies a struct with a pointer in the
// middle reports the pointer extent up to and including that pointer, never
// past it: trailing non-pointer fields advance offset but must not shift p.
func TestGcSizesPtrdataMidStructPointer(t *testing.T) {
	strType := types.NewStruct([]*types.Var{
		types.NewVar(token.NoPos, nil, "a", types.Typ[types.Uint64]),
		types.NewVar(token.NoPos, nil, "p", types.NewPointer(types.Typ[types.Int])),
		types.NewVar(token.NoPos, nil, "b", types.Typ[types.Uint64]),
	}, nil)
	if got := testSizes64.ptrdata(strType); got != 16 {
		t.Errorf("ptrdata(struct{uint64;*int;uint64}) = %d, want 16", got)
	}
}

// unknownType is a synthetic types.Type that drives ptrdataUncached's fallback branch.
type unknownType struct{}

func (u unknownType) Underlying() types.Type { return u }
func (u unknownType) String() string         { return "unknown" }

// TestGcSizesPtrdataUnknownTypeFallback pins the WordSize catch-all in
// ptrdataUncached. BUG-B2: the prior `panic("impossible")` would crash
// the analyzer on any types.Type whose Underlying() matched none of the
// enumerated kinds — a hazard for any future Go release that adds a new
// kind, and for third-party types.Type implementations.
//
// The standalone call exercises the fallback path directly; the struct-
// embedded call confirms the same fallback fires through the recursive
// per-field walk inside the *types.Struct arm.
func TestGcSizesPtrdataUnknownTypeFallback(t *testing.T) {
	if got := testSizes64.ptrdata(unknownType{}); got != testSizes64.WordSize {
		t.Errorf("ptrdata(unknown) = %d, want %d (WordSize fallback)",
			got, testSizes64.WordSize)
	}

	strType := types.NewStruct([]*types.Var{
		types.NewVar(token.NoPos, nil, "v", unknownType{}),
	}, nil)
	if got := testSizes64.ptrdata(strType); got != testSizes64.WordSize {
		t.Errorf("ptrdata(struct{v unknown}) = %d, want %d",
			got, testSizes64.WordSize)
	}
}

// TestGcSizesPtrdataTypeParamSmoke locks the user-visible BUG-B2 guarantee:
// a struct field declared as a generic type parameter must not crash the
// analyzer. In current Go (1.26) *types.TypeParam.Underlying() returns the
// constraint's interface, so the call routes through the *types.Interface
// arm (2*WordSize) — not the fallback. The Go standard library deprecates
// that behavior, so a future release may switch Underlying() to return the
// TypeParam itself, at which point the fallback would activate. The test
// accepts either path; what it pins is that no path panics.
func TestGcSizesPtrdataTypeParamSmoke(t *testing.T) {
	tn := types.NewTypeName(token.NoPos, nil, "T", nil)
	iface := types.NewInterfaceType(nil, nil)
	iface.Complete()
	tp := types.NewTypeParam(tn, iface)

	got := testSizes64.ptrdata(tp)
	if got != testSizes64.WordSize && got != 2*testSizes64.WordSize {
		t.Errorf("ptrdata(TypeParam) = %d, want %d (fallback) or %d (Interface arm)",
			got, testSizes64.WordSize, 2*testSizes64.WordSize)
	}
}

// ─── Layer 16: gcSizes caches and newGCSizes ─────────────────────────────────

// TestNewGCSizesInitializesCaches verifies the constructor allocates all
// three memoisation maps. Callers that build a *gcSizes via newGCSizes rely
// on the maps being non-nil; the cacheless struct literal path is exercised
// by other tests in this file (testSizes64 is constructed that way).
func TestNewGCSizesInitializesCaches(t *testing.T) {
	sizes := newGCSizes(8, 8)
	if sizes.sizeCache == nil {
		t.Error("sizeCache is nil after newGCSizes")
	}
	if sizes.alignCache == nil {
		t.Error("alignCache is nil after newGCSizes")
	}
	if sizes.ptrCache == nil {
		t.Error("ptrCache is nil after newGCSizes")
	}
	if sizes.WordSize != 8 || sizes.MaxAlign != 8 {
		t.Errorf("WordSize=%d MaxAlign=%d, want 8/8", sizes.WordSize, sizes.MaxAlign)
	}
}

// TestGcSizesCachesPopulate verifies that Sizeof / Alignof / ptrdata each
// populate their respective cache after a single call, and that successive
// calls return identical values.
func TestGcSizesCachesPopulate(t *testing.T) {
	sizes := newGCSizes(8, 8)
	str := types.NewStruct([]*types.Var{
		types.NewVar(token.NoPos, nil, "p", types.NewPointer(types.Typ[types.Int])),
	}, nil)

	first := sizes.Sizeof(str)
	if _, ok := sizes.sizeCache[str]; !ok {
		t.Error("sizeCache not populated after Sizeof")
	}
	if second := sizes.Sizeof(str); second != first {
		t.Errorf("Sizeof returned %d then %d for same type (cache must be stable)", first, second)
	}

	firstAlign := sizes.Alignof(str)
	if _, ok := sizes.alignCache[str]; !ok {
		t.Error("alignCache not populated after Alignof")
	}
	if second := sizes.Alignof(str); second != firstAlign {
		t.Errorf("Alignof returned %d then %d for same type", firstAlign, second)
	}

	firstPtr := sizes.ptrdata(str)
	if _, ok := sizes.ptrCache[str]; !ok {
		t.Error("ptrCache not populated after ptrdata")
	}
	if second := sizes.ptrdata(str); second != firstPtr {
		t.Errorf("ptrdata returned %d then %d for same type", firstPtr, second)
	}
}

// TestGcSizesNilCachesAllowed verifies that a *gcSizes built with a struct
// literal (no cache maps) still computes correctly. testSizes64, used by
// every other test in this file, is constructed this way.
func TestGcSizesNilCachesAllowed(t *testing.T) {
	bare := &gcSizes{WordSize: 8, MaxAlign: 8}
	if got := bare.Sizeof(types.Typ[types.Int32]); got != 4 {
		t.Errorf("Sizeof on cacheless gcSizes = %d, want 4", got)
	}
	if got := bare.Alignof(types.Typ[types.Int32]); got != 4 {
		t.Errorf("Alignof on cacheless gcSizes = %d, want 4", got)
	}
	if got := bare.ptrdata(types.NewPointer(types.Typ[types.Int])); got != 8 {
		t.Errorf("ptrdata on cacheless gcSizes = %d, want 8", got)
	}
}

// ─── Layer 17: lookupUnsafePointer ───────────────────────────────────────────

// TestLookupUnsafePointer verifies the package-init helper finds the
// canonical unsafe.Pointer type via the unsafe package scope. A nil return
// would silently downgrade gcSizes to its 8/8 defaults at runtime (see the
// guarded fall-back in run()), masking architecture-specific sizes.
func TestLookupUnsafePointer(t *testing.T) {
	got := lookupUnsafePointer()
	if got == nil {
		t.Fatal("lookupUnsafePointer returned nil")
	}
	basic, ok := got.Underlying().(*types.Basic)
	if !ok {
		t.Fatalf("underlying type = %T, want *types.Basic", got.Underlying())
	}
	if basic.Kind() != types.UnsafePointer {
		t.Errorf("basic kind = %v, want UnsafePointer", basic.Kind())
	}
}

// TestUnsafePointerTypInitialized verifies the package-level variable is
// non-nil after init. If lookupUnsafePointer ever starts returning nil, the
// 8/8 fall-back path in run() activates and architecture autodetection
// breaks.
func TestUnsafePointerTypInitialized(t *testing.T) {
	if unsafePointerTyp == nil {
		t.Error("unsafePointerTyp is nil; run() will use 8/8 defaults instead of pass.TypesSizes")
	}
}

// ─── Layer 18: hasGeneratedComment edge cases ────────────────────────────────

// TestHasGeneratedCommentEdgeCases extends TestHasGeneratedComment with the
// rarer arrangements that the regex and position guard must still handle
// correctly.
func TestHasGeneratedCommentEdgeCases(t *testing.T) {
	t.Run("file with no comments", func(t *testing.T) {
		f := parseTestFile(t, "package foo\n")
		if hasGeneratedComment(f) {
			t.Error("file with no comments must not be detected as generated")
		}
	})

	t.Run("block comment is not recognized as generated header", func(t *testing.T) {
		f := parseTestFile(t, "/* Code generated by foo. DO NOT EDIT. */\npackage foo\n")
		if hasGeneratedComment(f) {
			t.Error("block comment must not be recognized as generated header")
		}
	})

	t.Run("matching directive in second group before package", func(t *testing.T) {
		src := "// License header line 1\n// License header line 2\n\n// Code generated by foo. DO NOT EDIT.\npackage foo\n"
		f := parseTestFile(t, src)
		if !hasGeneratedComment(f) {
			t.Error("matching directive in any group before package must be detected")
		}
	})

	t.Run("missing trailing period is rejected", func(t *testing.T) {
		f := parseTestFile(t, "// Code generated by foo. DO NOT EDIT\npackage foo\n")
		if hasGeneratedComment(f) {
			t.Error("missing trailing period must not match the generated regex")
		}
	})

	t.Run("no Code generated prefix is rejected", func(t *testing.T) {
		f := parseTestFile(t, "// Generated by foo. DO NOT EDIT.\npackage foo\n")
		if hasGeneratedComment(f) {
			t.Error("comment lacking the 'Code generated' prefix must not match")
		}
	})
}

// ─── Layer 19: StringArrayFlag.String ────────────────────────────────────────

// TestStringArrayFlagString verifies the flag.Value Stringer contract. The
// flag package relies on a non-panicking String() for usage messages, so even
// a zero-value receiver must produce a printable result.
func TestStringArrayFlagString(t *testing.T) {
	tests := []struct {
		name string
		f    StringArrayFlag
		want string
	}{
		{"nil prints as empty slice", nil, "[]"},
		{"empty prints as empty slice", StringArrayFlag{}, "[]"},
		{"single entry", StringArrayFlag{"a"}, "[a]"},
		{"multiple entries", StringArrayFlag{"a", "b", "c"}, "[a b c]"},
		{"entries containing spaces", StringArrayFlag{"my path"}, "[my path]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.f.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ─── Layer 20: declaredWithOptInComment ──────────────────────────────────────

// TestDeclaredWithOptInComment exercises the thin wrapper that reads the
// opt-in directive from a *ast.GenDecl's Doc field. The wrapper is invoked
// once per type-decl group; a nil Doc is the common case (most declarations
// have no documentation comment).
func TestDeclaredWithOptInComment(t *testing.T) {
	t.Run("nil Doc returns false", func(t *testing.T) {
		decl := &ast.GenDecl{}
		if declaredWithOptInComment(decl) {
			t.Error("decl with nil Doc must return false")
		}
	})

	t.Run("Doc with directive returns true", func(t *testing.T) {
		decl := &ast.GenDecl{Doc: makeCommentGroup("// betteralign:check")}
		if !declaredWithOptInComment(decl) {
			t.Error("decl with betteralign:check Doc must return true")
		}
	})

	t.Run("Doc with directive plus trailing text returns true", func(t *testing.T) {
		decl := &ast.GenDecl{Doc: makeCommentGroup("// betteralign:check reason here")}
		if !declaredWithOptInComment(decl) {
			t.Error("directive with trailing text must still trigger opt-in")
		}
	})

	t.Run("Doc without directive returns false", func(t *testing.T) {
		decl := &ast.GenDecl{Doc: makeCommentGroup("// unrelated comment")}
		if declaredWithOptInComment(decl) {
			t.Error("decl with unrelated Doc must return false")
		}
	})

	t.Run("Doc with block comment is rejected", func(t *testing.T) {
		decl := &ast.GenDecl{Doc: makeCommentGroup("/* betteralign:check */")}
		if declaredWithOptInComment(decl) {
			t.Error("block comment must not trigger opt-in")
		}
	})
}

// ─── Layer 21: optimalOrder stability and zero-pointer fast path ─────────────

// TestOptimalOrderStable verifies that fields with identical sort keys preserve
// their declaration order. The sort uses slices.SortStableFunc; a non-stable
// replacement could silently swap equivalent fields and produce a different
// (but still valid) fixup, surprising users who expect a deterministic rewrite.
func TestOptimalOrderStable(t *testing.T) {
	fields := []*types.Var{
		types.NewVar(token.NoPos, nil, "a", types.Typ[types.Uint32]),
		types.NewVar(token.NoPos, nil, "b", types.Typ[types.Uint32]),
		types.NewVar(token.NoPos, nil, "c", types.Typ[types.Uint32]),
	}
	strType := types.NewStruct(fields, nil)
	indexes, _, _ := optimalOrder(strType, testSizes64)
	want := []int{0, 1, 2}
	if len(indexes) != len(want) {
		t.Fatalf("got %d indexes, want %d", len(indexes), len(want))
	}
	for i, v := range want {
		if indexes[i] != v {
			t.Errorf("indexes[%d] = %d, want %d (sort must be stable)", i, indexes[i], v)
		}
	}
}

// TestOptimalOrderNoPointers verifies that optPtrdata is zero for a struct
// composed entirely of non-pointer fields, ensuring the "pointer bytes"
// diagnostic is never emitted for a struct that the GC will skip entirely.
func TestOptimalOrderNoPointers(t *testing.T) {
	fields := []*types.Var{
		types.NewVar(token.NoPos, nil, "x", types.Typ[types.Int32]),
		types.NewVar(token.NoPos, nil, "y", types.Typ[types.Uint64]),
	}
	strType := types.NewStruct(fields, nil)
	_, _, optPtrdata := optimalOrder(strType, testSizes64)
	if optPtrdata != 0 {
		t.Errorf("optPtrdata = %d, want 0 (no pointer fields)", optPtrdata)
	}
}

// TestOptimalOrderStableWithManyTiedFields forces pdqsort past the insertion-sort
// threshold and across two alignment classes, so loss of stability shuffles tied fields.
func TestOptimalOrderStableWithManyTiedFields(t *testing.T) {
	const pairs = 16 // 32 fields total: clears pdqsort's insertion-sort threshold.
	fields := make([]*types.Var, 0, pairs*2)
	for i := 0; i < pairs; i++ {
		fields = append(fields, types.NewVar(token.NoPos, nil, fmt.Sprintf("u64_%d", i), types.Typ[types.Uint64]))
		fields = append(fields, types.NewVar(token.NoPos, nil, fmt.Sprintf("u32_%d", i), types.Typ[types.Uint32]))
	}
	strType := types.NewStruct(fields, nil)
	indexes, _, _ := optimalOrder(strType, testSizes64)

	// Stable: uint64s in even-index order first, then uint32s in odd-index order.
	want := make([]int, 0, pairs*2)
	for i := 0; i < pairs; i++ {
		want = append(want, i*2) // uint64 declarations are at even indexes.
	}
	for i := 0; i < pairs; i++ {
		want = append(want, i*2+1) // uint32 declarations are at odd indexes.
	}
	for i, idx := range indexes {
		if idx != want[i] {
			t.Errorf("indexes[%d] = %d, want %d (stable sort required for tied keys; full got %v want %v)", i, idx, want[i], indexes, want)
			return
		}
	}
}

// TestOptimalOrderEmptyStructFields verifies optSize and optPtrdata for a
// struct made entirely of zero-sized fields: stable sort keeps the order
// unchanged, offset never advances, and the trailing-bump rule does not fire.
func TestOptimalOrderEmptyStructFields(t *testing.T) {
	emptyStruct := types.NewStruct(nil, nil)
	fields := []*types.Var{
		types.NewVar(token.NoPos, nil, "a", emptyStruct),
		types.NewVar(token.NoPos, nil, "b", emptyStruct),
	}
	strType := types.NewStruct(fields, nil)
	indexes, optSize, optPtrdata := optimalOrder(strType, testSizes64)
	if len(indexes) != 2 || indexes[0] != 0 || indexes[1] != 1 {
		t.Errorf("indexes = %v, want [0 1] (stable for all-equal keys)", indexes)
	}
	if optSize != 0 {
		t.Errorf("optSize = %d, want 0 (all fields zero-sized)", optSize)
	}
	if optPtrdata != 0 {
		t.Errorf("optPtrdata = %d, want 0", optPtrdata)
	}
}

// TestOptimalOrderSinglePointerField verifies the optPtrdata calculation for
// a struct containing exactly one pointer-bearing field. The pointer extent
// must equal the field's offset (0) plus its ptrdata (one word).
func TestOptimalOrderSinglePointerField(t *testing.T) {
	fields := []*types.Var{
		types.NewVar(token.NoPos, nil, "p", types.NewPointer(types.Typ[types.Int])),
	}
	strType := types.NewStruct(fields, nil)
	indexes, optSize, optPtrdata := optimalOrder(strType, testSizes64)
	if len(indexes) != 1 || indexes[0] != 0 {
		t.Errorf("indexes = %v, want [0]", indexes)
	}
	if optSize != 8 {
		t.Errorf("optSize = %d, want 8", optSize)
	}
	if optPtrdata != 8 {
		t.Errorf("optPtrdata = %d, want 8", optPtrdata)
	}
}

// ─── Layer 22: normalizeExcludePaths ─────────────────────────────────────────

// TestNormalizeExcludePaths verifies that absolute paths are rewritten to be
// wd-relative while relative paths pass through unchanged. The function backs
// the run-time conversion of -exclude_dirs/-exclude_files patterns so that
// users can supply either form interchangeably without silently disabling
// the analyzer.
func TestNormalizeExcludePaths(t *testing.T) {
	wd := filepath.Join(string(filepath.Separator), "proj")
	tests := []struct {
		name    string
		paths   []string
		want    []string
		wantErr bool
	}{
		{
			name:  "all relative passes through",
			paths: []string{"vendor", "third_party/grpc", "*.gen.go"},
			want:  []string{"vendor", "third_party/grpc", "*.gen.go"},
		},
		{
			name:  "absolute under wd rewrites to relative",
			paths: []string{filepath.Join(wd, "vendor")},
			want:  []string{"vendor"},
		},
		{
			name:  "mixed forms each handled independently",
			paths: []string{"vendor", filepath.Join(wd, "third_party")},
			want:  []string{"vendor", "third_party"},
		},
		{
			name:  "empty input yields empty output",
			paths: nil,
			want:  []string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeExcludePaths(wd, tc.paths)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeExcludePaths(%v) succeeded; want error", tc.paths)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeExcludePaths(%v) error: %v", tc.paths, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (got=%v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestNormalizeExcludePathsDoesNotMutateInput verifies that the slow path
// (input contains at least one absolute entry) leaves the caller-owned slice
// unmodified. The input slice is backed by the analyzer's flag-bound
// StringArrayFlag, so mutating it would leak run-time state across passes.
func TestNormalizeExcludePathsDoesNotMutateInput(t *testing.T) {
	wd := filepath.Join(string(filepath.Separator), "proj")
	in := []string{filepath.Join(wd, "vendor"), "third_party"}
	original := append([]string(nil), in...)
	_, err := normalizeExcludePaths(wd, in)
	if err != nil {
		t.Fatalf("normalize error: %v", err)
	}
	for i := range in {
		if in[i] != original[i] {
			t.Errorf("input mutated at [%d]: got %q, want %q", i, in[i], original[i])
		}
	}
}

// TestNormalizeExcludePathsFastPathReturnsInput verifies that when no entry
// is absolute, the function returns the caller-supplied slice unchanged
// (same backing array). The common case — relative-only patterns —
// avoids an allocation.
func TestNormalizeExcludePathsFastPathReturnsInput(t *testing.T) {
	wd := filepath.Join(string(filepath.Separator), "proj")
	in := []string{"vendor", "third_party"}
	got, err := normalizeExcludePaths(wd, in)
	if err != nil {
		t.Fatalf("normalize error: %v", err)
	}
	// Same backing array iff first-element addresses match.
	if len(in) == 0 || len(got) == 0 || &in[0] != &got[0] {
		t.Errorf("fast path allocated; want input slice returned unchanged")
	}
}

// TestNormalizeExcludePathsErrorMessage exercises the error wrapping for
// cross-volume / non-resolvable absolute paths. On POSIX, filepath.Rel only
// errors when relativisation would require runtime cwd; we synthesise that
// by passing wd="" so filepath.Rel cannot fall back to a known base.
func TestNormalizeExcludePathsErrorMessage(t *testing.T) {
	// Relative wd + absolute path forces filepath.Rel to error.
	got, err := normalizeExcludePaths("relative-wd", []string{string(filepath.Separator) + "abs"})
	if err == nil {
		t.Fatalf("expected error, got result %v", got)
	}
	msg := err.Error()
	for _, want := range []string{"exclude path", "relative-wd"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q: %s", want, msg)
		}
	}
}

// TestNormalizeExcludePathsRejectsEscapingPath: absolute paths outside wd must error, not silently no-op.
func TestNormalizeExcludePathsRejectsEscapingPath(t *testing.T) {
	wd := filepath.Join(string(filepath.Separator), "proj", "deep")
	outside := filepath.Join(string(filepath.Separator), "other", "vendor")
	got, err := normalizeExcludePaths(wd, []string{outside})
	if err == nil {
		t.Fatalf("expected error for path escaping wd, got result %v", got)
	}
	if !strings.Contains(err.Error(), outside) {
		t.Errorf("error should mention the offending path: %v", err)
	}
	if !strings.Contains(err.Error(), "escapes working directory") {
		t.Errorf("error should explain the escape: %v", err)
	}
}

// TestNormalizeExcludePathsRejectsParentLiteral pins the `rel == ".."` arm,
// which the prefix check (HasPrefix(rel, ".."+sep)) does not cover.
func TestNormalizeExcludePathsRejectsParentLiteral(t *testing.T) {
	wd := filepath.Join(string(filepath.Separator), "proj", "deep")
	parent := filepath.Join(string(filepath.Separator), "proj") // exactly one level up.

	if rel, err := filepath.Rel(wd, parent); err != nil || rel != ".." {
		t.Fatalf("test fixture mis-sized: filepath.Rel(%q, %q) = %q, %v; want \"..\"", wd, parent, rel, err)
	}

	_, err := normalizeExcludePaths(wd, []string{parent})
	if err == nil {
		t.Fatal("expected error for exclude path resolving to exactly \"..\", got nil")
	}
	if !strings.Contains(err.Error(), "escapes working directory") {
		t.Errorf("error should explain the escape: %v", err)
	}
}

// ─── Layer 23: InitAnalyzer double-registration guard ────────────────────────

// canonicalLoad type-checks src in-memory and returns its *types.Package and fileset.
func canonicalLoad(t *testing.T, src string) (*types.Package, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	conf := types.Config{}
	pkg, err := conf.Check("test", fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatalf("type-check: %v", err)
	}
	return pkg, fset
}

// TestCanonicalStructTypeGenericInstantiation pins .Origin(): a generic with T-using
// fields has a per-instantiation underlying, distinct from the origin's.
func TestCanonicalStructTypeGenericInstantiation(t *testing.T) {
	pkg, _ := canonicalLoad(t, `package test
type Box[T any] struct {
	x byte
	y T
	z byte
}
type Inst = Box[int32]
`)

	box := pkg.Scope().Lookup("Box").Type().(*types.Named)
	originStruct := box.Origin().Underlying().(*types.Struct)

	// Inst is a type alias to Box[int32]; unaliasing gives the Named instantiation.
	inst := types.Unalias(pkg.Scope().Lookup("Inst").Type()).(*types.Named)
	if inst.Origin() != box {
		t.Fatalf("Inst.Origin() = %v, want %v (fixture broken)", inst.Origin(), box)
	}
	instStruct := inst.Underlying().(*types.Struct)
	if instStruct == originStruct {
		t.Fatalf("instantiation reused origin's underlying; need a fixture where T appears in fields to force a fresh struct")
	}

	got := canonicalStructType(inst)
	if got != originStruct {
		t.Errorf("canonicalStructType(Box[int32]) = %p, want origin's struct %p (mutation drops .Origin())", got, originStruct)
	}
}

// TestCollectPositionalUsersFirstWins pins "first occurrence wins" — the diagnostic must
// point at the canonical (earliest) positional literal, not the last seen.
func TestCollectPositionalUsersFirstWins(t *testing.T) {
	src := `package test

type T struct {
	x byte
	y int32
	z byte
}

var _ = T{1, 2, 3} // first literal — line 9
var _ = T{4, 5, 6} // second literal — line 10
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	conf := types.Config{}
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Implicits:  make(map[ast.Node]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Scopes:     make(map[ast.Node]*types.Scope),
	}
	pkg, err := conf.Check("test", fset, []*ast.File{f}, info)
	if err != nil {
		t.Fatalf("type-check: %v", err)
	}
	insp := inspector.New([]*ast.File{f})
	pass := &analysis.Pass{Fset: fset, Pkg: pkg, TypesInfo: info, Files: []*ast.File{f}}

	users := collectPositionalUsers(insp, pass)
	tStruct := pkg.Scope().Lookup("T").Type().(*types.Named).Underlying().(*types.Struct)

	pos, ok := users[tStruct]
	if !ok {
		t.Fatal("collectPositionalUsers did not record T at all")
	}
	gotLine := fset.Position(pos).Line
	if gotLine != 9 {
		t.Errorf("users[T] line = %d, want 9 (first literal); a last-wins mutation would report line 10", gotLine)
	}
}

// TestCanonicalStructTypeAlias pins types.Unalias: a `type A = T` alias must
// collapse to T's underlying so positional literals through A pin the layout.
func TestCanonicalStructTypeAlias(t *testing.T) {
	pkg, _ := canonicalLoad(t, `package test
type T struct {
	x byte
	y int32
	z byte
}
type A = T
`)

	tNamed := pkg.Scope().Lookup("T").Type().(*types.Named)
	underT := tNamed.Underlying().(*types.Struct)

	aTyp := pkg.Scope().Lookup("A").Type() // an Alias wrapping T.
	got := canonicalStructType(aTyp)
	if got != underT {
		t.Errorf("canonicalStructType(A) = %p, want T's struct %p (mutation drops types.Unalias)", got, underT)
	}
}

// TestInitAnalyzerDoubleRegistrationIsNoop verifies that calling InitAnalyzer
// twice on the same *analysis.Analyzer does not panic. flag.FlagSet.BoolVar
// fatals on duplicate names, so without the guard a defensive caller that
// re-inits the package-level Analyzer (or any analyzer already passed
// through InitAnalyzer) would crash the host process.
func TestInitAnalyzerDoubleRegistrationIsNoop(t *testing.T) {
	a := &analysis.Analyzer{Name: "t", Doc: "t"}
	InitAnalyzer(a)
	if a.Run == nil {
		t.Fatal("first InitAnalyzer left Run nil")
	}
	// Second call must not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("second InitAnalyzer panicked: %v", r)
		}
	}()
	InitAnalyzer(a)
	if a.Run == nil {
		t.Fatal("Run cleared by second InitAnalyzer")
	}
}

// ─── Layer 24: applyToFile empty-buffer guard ────────────────────────────────

// TestApplyToFileRejectsEmptyBuffer verifies that applyToFile refuses to
// write an empty byte slice. The earlier upstream check in run() was
// consolidated into applyToFile so that the guarantee lives at the
// boundary, regardless of how the caller arrived there.
func TestApplyToFileRejectsEmptyBuffer(t *testing.T) {
	dir := t.TempDir()
	fn := filepath.Join(dir, "x.go")
	if err := os.WriteFile(fn, []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := applyToFile(fn, nil)
	if err == nil {
		t.Fatal("expected error for empty buffer, got nil")
	}
	if !errors.Is(err, ErrEmptyBuffer) {
		t.Errorf("errors.Is(err, ErrEmptyBuffer) = false; err = %v", err)
	}
	// File contents must be unchanged.
	got, err := os.ReadFile(fn)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "package x\n" {
		t.Errorf("file was modified despite empty-buffer guard: got %q", got)
	}
}
