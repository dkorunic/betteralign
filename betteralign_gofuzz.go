// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

//go:build gofuzz

// go-fuzz entry points for the betteralign analyzer.
//
// These wrappers mirror the native FuzzOptimalOrder / FuzzGCSizes targets in
// betteralign_fuzz_test.go but expose the dvyukov/go-fuzz signature
// `func(data []byte) int`. They are gated by the `gofuzz` build tag — set
// automatically by go-fuzz-build — so they neither participate in `go build`
// nor in `go test ./...`.
//
// Build & run (one zip per fuzz function):
//
//	go install github.com/dvyukov/go-fuzz/go-fuzz@latest
//	go install github.com/dvyukov/go-fuzz/go-fuzz-build@latest
//
//	go-fuzz-build -func=FuzzOptimalOrder -o optimalorder-fuzz.zip
//	go-fuzz       -bin=optimalorder-fuzz.zip -workdir=fuzz/optimalorder
//
//	go-fuzz-build -func=FuzzGCSizes -o gcsizes-fuzz.zip
//	go-fuzz       -bin=gcsizes-fuzz.zip -workdir=fuzz/gcsizes
//
// A seed corpus derived from testdata/ lives at testdata/fuzz-corpus/ (one
// file per .go / .go.golden source, names path-mangled with __ separators).
// `task fuzz-go-build` automates the entire setup — building the four zips
// and copying the corpus into each fuzz/<target>/corpus/ — or you can do it
// by hand:
//
//	task fuzz-go-corpus   # (re)generate testdata/fuzz-corpus from testdata/src
//	mkdir -p fuzz/optimalorder/corpus
//	cp testdata/fuzz-corpus/* fuzz/optimalorder/corpus/
//
// Return codes follow the go-fuzz protocol:
//
//	-1  input rejected (parse failure / nothing inspectable)
//	 0  input accepted but uninteresting
//	 1  input accepted and worth keeping in the corpus
//
// Invariant violations are reported by panic; go-fuzz captures the panicking
// input under <workdir>/crashers/.

package betteralign

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
)

// FuzzOptimalOrder is the dvyukov/go-fuzz entry corresponding to the
// testing.F harness in betteralign_fuzz_test.go. The two harnesses share
// invariant-check helpers via this package-internal layer because the
// native testing.F path takes *testing.T (which doesn't exist outside
// _test.go) while the go-fuzz path uses panic for failure reporting.
// Invariants enforced per named struct in the type-checked input:
//
//   - optimalOrder does not panic
//   - returned indexes are a valid permutation of [0, NumFields)
//   - returned optSize never exceeds the struct's original Sizeof
//   - returned optPtrdata never exceeds the struct's original ptrdata
//   - neither optSize nor optPtrdata go negative
//
// Return code follows the go-fuzz protocol: -1 reject (input rejected at
// parse/type-check, or no inspectable structs), 0 keep-uninteresting (not
// emitted here), 1 keep-interesting (at least one struct was checked).
// Invariant violations surface as panics that go-fuzz routes into the
// workdir's crashers/ directory.
func FuzzOptimalOrder(data []byte) int {
	pkg := typeCheckGoFuzz(string(data))
	if pkg == nil {
		return -1
	}
	sizes := newGCSizes(8, 8)
	checked := 0
	for _, name := range pkg.Scope().Names() {
		tn, ok := pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			continue
		}
		st, ok := named.Origin().Underlying().(*types.Struct)
		if !ok {
			continue
		}
		if st.NumFields() == 0 {
			continue
		}
		checkOptimalOrderInvariantsGoFuzz(name, st, sizes)
		checked++
	}
	if checked == 0 {
		return -1
	}
	return 1
}

// FuzzGCSizes is the dvyukov/go-fuzz entry for the gcSizes layer,
// mirroring the testing.F harness in betteralign_fuzz_test.go. Targets the
// arithmetic and overflow saturation in Alignof/Sizeof/ptrdata rather
// than the higher-level optimalOrder algorithm. Invariants enforced per
// named type in the type-checked input:
//
//   - Sizeof / Alignof / ptrdata do not panic
//   - Sizeof >= ptrdata (pointer-bearing prefix can't exceed total size)
//   - Alignof >= 1 (per the language spec)
//   - neither Sizeof nor ptrdata go negative (overflow saturation works)
//
// Return code follows the go-fuzz protocol; see FuzzOptimalOrder for the
// shared contract.
func FuzzGCSizes(data []byte) int {
	pkg := typeCheckGoFuzz(string(data))
	if pkg == nil {
		return -1
	}
	sizes := newGCSizes(8, 8)
	checked := 0
	for _, name := range pkg.Scope().Names() {
		tn, ok := pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok {
			continue
		}
		checkGCSizesInvariantsGoFuzz(name, tn.Type(), sizes)
		checked++
	}
	if checked == 0 {
		return -1
	}
	return 1
}

// typeCheckGoFuzz produces a *types.Package from arbitrary input bytes,
// or nil for any failure mode the caller should treat as "uninteresting"
// per the go-fuzz protocol. Exists as a separate function because the
// testing.F version (typeCheckFuzzInput) takes *testing.T and is
// therefore unreachable from this build-tag-gated file.
//
// Three failure paths funnel into the nil return: parser rejection
// (deferred discard via the named return), nil *types.Package from
// go/types when the type-checker rejects everything, and an upstream
// panic from go/types (which it does on adversarial inputs in rare
// corner cases). The panic-recovery defer is the load-bearing bit — a
// panic during type-checking would otherwise terminate go-fuzz's worker
// process and the input would be saved as a crasher even though the
// failure is in the type-checker, not the harness invariants.
//
// types.Config is configured with a no-op error handler so partial
// type-check results survive past the first reported error, Importer: nil
// to avoid going to disk for arbitrary import paths the fuzzer might
// fabricate, and IgnoreFuncBodies: true so function-body constant folding
// can't hang the harness (BUG-44: 134291756e439044200 minus a sequence of
// small ints produced an 8.5 s type-check on a 119-byte input; the
// harness only inspects package-scope named types, so skipping bodies is
// invisible to its checks).
func typeCheckGoFuzz(src string) (pkg *types.Package) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "input.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			pkg = nil
		}
	}()
	conf := types.Config{
		Error:            func(error) {},
		Importer:         nil,
		IgnoreFuncBodies: true,
	}
	pkg, _ = conf.Check("fuzz", fset, []*ast.File{file}, nil)
	return pkg
}

// checkOptimalOrderInvariantsGoFuzz panics on any violation of the
// FuzzOptimalOrder invariants. Centralised so the assertions stay in lock-
// step with the testing.F path even when the latter grows new checks; the
// name parameter is interpolated into every panic so go-fuzz crashers
// point straight at the offending type. Five invariants are checked in
// order from cheapest (index validity) to most expensive (recomputing
// sizes.Sizeof on the original struct), so a malformed permutation
// surfaces before the layout-comparison work even runs.
func checkOptimalOrderInvariantsGoFuzz(name string, st *types.Struct, sizes *gcSizes) {
	nf := st.NumFields()
	indexes, optSize, optPtrdata := optimalOrder(st, sizes)
	if len(indexes) != nf {
		panic(fmt.Sprintf("%s: len(indexes)=%d, want %d", name, len(indexes), nf))
	}
	seen := make([]bool, nf)
	for _, idx := range indexes {
		if idx < 0 || idx >= nf {
			panic(fmt.Sprintf("%s: idx %d out of range [0,%d)", name, idx, nf))
		}
		if seen[idx] {
			panic(fmt.Sprintf("%s: idx %d appears twice", name, idx))
		}
		seen[idx] = true
	}
	if optSize < 0 {
		panic(fmt.Sprintf("%s: optSize=%d (negative; integer overflow in accumulator)", name, optSize))
	}
	if optPtrdata < 0 {
		panic(fmt.Sprintf("%s: optPtrdata=%d (negative; integer overflow in accumulator)", name, optPtrdata))
	}
	origSize := sizes.Sizeof(st)
	if optSize > origSize {
		panic(fmt.Sprintf("%s: optSize=%d > origSize=%d", name, optSize, origSize))
	}
	origPtrdata := sizes.ptrdata(st)
	if optPtrdata > origPtrdata {
		panic(fmt.Sprintf("%s: optPtrdata=%d > origPtrdata=%d", name, optPtrdata, origPtrdata))
	}
}

// checkGCSizesInvariantsGoFuzz panics on any violation of the FuzzGCSizes
// invariants. The four checks (sz ≥ 0, pd ≥ 0, al ≥ 1, pd ≤ sz) together
// confirm the arithmetic primitives (align, mulSize, addSize) saturated
// rather than wrapped, the spec's Alignof lower bound holds, and the
// ptrdata-cannot-exceed-Sizeof invariant the GC relies on is preserved.
// The name parameter is interpolated into every panic so go-fuzz crashers
// point straight at the offending type.
func checkGCSizesInvariantsGoFuzz(name string, typ types.Type, sizes *gcSizes) {
	sz := sizes.Sizeof(typ)
	al := sizes.Alignof(typ)
	pd := sizes.ptrdata(typ)
	if sz < 0 {
		panic(fmt.Sprintf("%s: Sizeof=%d (negative; integer overflow)", name, sz))
	}
	if pd < 0 {
		panic(fmt.Sprintf("%s: ptrdata=%d (negative; integer overflow)", name, pd))
	}
	if al < 1 {
		panic(fmt.Sprintf("%s: Alignof=%d, want >=1", name, al))
	}
	if pd > sz {
		panic(fmt.Sprintf("%s: ptrdata=%d > Sizeof=%d", name, pd, sz))
	}
}
