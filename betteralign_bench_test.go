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
