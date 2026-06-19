// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package betteralign

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

// makeBenchStruct constructs a struct with n alternating fields of varying
// alignment to exercise every branch of the optimalOrder comparator.
func makeBenchStruct(n int) *types.Struct {
	ptrInt := types.NewPointer(types.Typ[types.Int])
	kinds := []types.Type{
		types.Typ[types.Bool],
		types.Typ[types.Int32],
		types.Typ[types.Uint64],
		types.Typ[types.String],
		ptrInt,
		types.Typ[types.Int16],
		types.Typ[types.Float64],
	}
	fields := make([]*types.Var, n)
	for i := range n {
		fields[i] = types.NewVar(token.NoPos, nil, fmt.Sprintf("f%d", i), kinds[i%len(kinds)])
	}
	return types.NewStruct(fields, nil)
}

func BenchmarkOptimalOrder_Small(b *testing.B) {
	str := makeBenchStruct(5)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		sizes := newGCSizes(8, 8)
		_, _, _ = optimalOrder(str, sizes)
	}
}

func BenchmarkOptimalOrder_Medium(b *testing.B) {
	str := makeBenchStruct(20)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		sizes := newGCSizes(8, 8)
		_, _, _ = optimalOrder(str, sizes)
	}
}

func BenchmarkOptimalOrder_Large(b *testing.B) {
	str := makeBenchStruct(100)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		sizes := newGCSizes(8, 8)
		_, _, _ = optimalOrder(str, sizes)
	}
}

// makeOptimalBenchStruct constructs a struct already laid out in optimal
// field order: a pointer field first, then descending-alignment, pointer-free
// fields. layoutMetrics reports this shape as already optimal, so it
// exercises the zero-allocation fast path in the analyzer's main loop.
func makeOptimalBenchStruct(n int) *types.Struct {
	if n < 1 {
		n = 1
	}
	fields := make([]*types.Var, 0, n)
	fields = append(fields, types.NewVar(token.NoPos, nil, "p", types.NewPointer(types.Typ[types.Int])))
	tiers := []types.Type{
		types.Typ[types.Int64],
		types.Typ[types.Int32],
		types.Typ[types.Int16],
		types.Typ[types.Int8],
	}
	remaining := n - 1
	perTier := remaining / len(tiers)
	extra := remaining % len(tiers)
	for tier, t := range tiers {
		count := perTier
		if tier < extra {
			count++
		}
		for i := range count {
			fields = append(fields, types.NewVar(token.NoPos, nil, fmt.Sprintf("f%d_%d", tier, i), t))
		}
	}
	return types.NewStruct(fields, nil)
}

// BenchmarkDecisionPath_Optimal_NoFastPath measures the pre-optimization
// per-struct decision cost for an already-optimal struct: optimalOrder plus
// the Sizeof and ptrdata walks on the original layout. A fresh gcSizes per
// iteration mirrors the realistic per-struct cache-miss profile in a pass.
func BenchmarkDecisionPath_Optimal_NoFastPath(b *testing.B) {
	str := makeOptimalBenchStruct(20)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		sizes := newGCSizes(8, 8)
		_, optsz, optptrs := optimalOrder(str, sizes)
		sz := sizes.Sizeof(str)
		ptrs := sizes.ptrdata(str)
		if sz != optsz || ptrs != optptrs {
			b.Fatal("optimal struct: sizes should match")
		}
	}
}

// BenchmarkDecisionPath_Optimal_FastPath measures the analyzer's real
// report-mode hot path for an already-optimal struct: layoutMetrics returns
// optimal=true and the caller short-circuits before Sizeof/ptrdata and before
// any permutation allocation. The delta against the NoFastPath variant is the
// win on already-aligned structs; this path is now zero-allocation.
func BenchmarkDecisionPath_Optimal_FastPath(b *testing.B) {
	str := makeOptimalBenchStruct(20)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		sizes := newGCSizes(8, 8)
		optimal, _, _ := layoutMetrics(str, sizes)
		if !optimal {
			b.Fatal("optimal struct should be reported as optimal")
		}
	}
}

// BenchmarkDecisionPath_Suboptimal_FastPath measures the report-mode path when
// the struct is misaligned: layoutMetrics returns optimal=false plus the
// optimal size/ptrdata, then the caller reads the current Sizeof and ptrdata
// to build the diagnostic message. The permutation (optimalOrder) is not
// computed in report mode, only under -apply.
func BenchmarkDecisionPath_Suboptimal_FastPath(b *testing.B) {
	str := makeBenchStruct(20)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		sizes := newGCSizes(8, 8)
		optimal, optsz, optptrs := layoutMetrics(str, sizes)
		if optimal {
			b.Fatal("suboptimal struct should not be reported as optimal")
		}
		sz := sizes.Sizeof(str)
		ptrs := sizes.ptrdata(str)
		_ = sz == optsz
		_ = ptrs == optptrs
	}
}

// buildIgnoreScanSource builds a file with numStructs top-level structs, each
// carrying a type doc comment plus a lead comment on every field, so
// file.Comments scales with numStructs*(fieldsPerStruct+1). That is the
// worst-case surface for hasIgnoreCommentAST, which rescans file.Comments from
// the top on every call.
func buildIgnoreScanSource(numStructs, fieldsPerStruct int) []byte {
	var sb strings.Builder
	sb.WriteString("package bench\n\n")
	for s := range numStructs {
		fmt.Fprintf(&sb, "// S%d doc comment line.\ntype S%d struct {\n", s, s)
		for f := range fieldsPerStruct {
			fmt.Fprintf(&sb, "\t// f%d comment\n\tf%d int\n", f, f)
		}
		sb.WriteString("}\n\n")
	}
	return []byte(sb.String())
}

// collectStructTypes parses src and returns every *ast.StructType in source
// order, so a benchmark can replay the analyzer's per-struct calls.
func collectStructTypes(tb testing.TB, src []byte) (*token.FileSet, *ast.File, []*ast.StructType) {
	tb.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "bench.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		tb.Fatalf("parse: %v", err)
	}
	var sts []*ast.StructType
	ast.Inspect(f, func(n ast.Node) bool {
		if st, ok := n.(*ast.StructType); ok {
			sts = append(sts, st)
		}
		return true
	})
	return fset, f, sts
}

// BenchmarkHasIgnoreCommentAST_Scaling reproduces the analyzer's worst case:
// hasIgnoreCommentAST is called once per misaligned struct, and each call
// rescans file.Comments from the top until it reaches that struct's closing
// brace. Total work is therefore O(structs x comments). The fields-per-struct
// is fixed at 4 and only the struct count is swept, so if the quadratic
// dominates, doubling structs= should roughly quadruple ns/op.
func BenchmarkHasIgnoreCommentAST_Scaling(b *testing.B) {
	for _, n := range []int{50, 100, 200, 400} {
		fset, f, sts := collectStructTypes(b, buildIgnoreScanSource(n, 4))
		b.Run(fmt.Sprintf("structs=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				for _, st := range sts {
					_ = hasIgnoreCommentAST(fset, f, st)
				}
			}
		})
	}
}
