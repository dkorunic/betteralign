// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package betteralign

import (
	"fmt"
	"go/token"
	"go/types"
	"testing"
)

// benchSizes is a 64-bit gcSizes shared across benchmarks. Caches are populated
// once per BenchmarkOptimalOrder_* invocation, so cache cost shows up only in
// the first iteration; subsequent iterations exercise the sort + reorder path.
var benchSizes = newGCSizes(8, 8)

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
		_, _, _ = optimalOrder(str, benchSizes)
	}
}

func BenchmarkOptimalOrder_Medium(b *testing.B) {
	str := makeBenchStruct(20)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _, _ = optimalOrder(str, benchSizes)
	}
}

func BenchmarkOptimalOrder_Large(b *testing.B) {
	str := makeBenchStruct(100)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _, _ = optimalOrder(str, benchSizes)
	}
}

// makeOptimalBenchStruct constructs a struct already laid out in optimal
// field order: a pointer field first, then descending-alignment, pointer-free
// fields. optimalOrder returns the identity permutation for this shape, so
// it exercises the isIdentityOrder fast path in the analyzer's main loop.
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

// BenchmarkDecisionPath_Optimal_FastPath measures the same shape under the
// post-optimization path: the identity check short-circuits before Sizeof
// and ptrdata are called. The delta against the NoFastPath variant is the
// win on already-aligned structs.
func BenchmarkDecisionPath_Optimal_FastPath(b *testing.B) {
	str := makeOptimalBenchStruct(20)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		sizes := newGCSizes(8, 8)
		indexes, _, _ := optimalOrder(str, sizes)
		if !isIdentityOrder(indexes) {
			b.Fatal("optimal struct should produce identity permutation")
		}
	}
}

// BenchmarkDecisionPath_Suboptimal_FastPath measures the slow-path cost when
// the identity check misses: optimalOrder, then isIdentityOrder (returns
// false), then Sizeof and ptrdata. Delta against an analogous run without
// isIdentityOrder is the fast path's overhead when it doesn't fire.
func BenchmarkDecisionPath_Suboptimal_FastPath(b *testing.B) {
	str := makeBenchStruct(20)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		sizes := newGCSizes(8, 8)
		indexes, optsz, optptrs := optimalOrder(str, sizes)
		if isIdentityOrder(indexes) {
			b.Fatal("suboptimal struct should not produce identity permutation")
		}
		sz := sizes.Sizeof(str)
		ptrs := sizes.ptrdata(str)
		_ = sz == optsz
		_ = ptrs == optptrs
	}
}
