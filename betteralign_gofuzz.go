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

// FuzzOptimalOrder is the go-fuzz entry corresponding to the testing.F
// version in betteralign_fuzz_test.go. Invariants enforced per named struct:
//
//   - optimalOrder does not panic
//   - returned indexes are a valid permutation of [0, NumFields)
//   - returned optSize never exceeds the struct's original Sizeof
//   - returned optPtrdata never exceeds the struct's original ptrdata
//   - neither optSize nor optPtrdata go negative
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

// FuzzGCSizes is the go-fuzz entry corresponding to the testing.F version in
// betteralign_fuzz_test.go. Invariants enforced per named type:
//
//   - Sizeof / Alignof / ptrdata do not panic
//   - Sizeof >= ptrdata (pointer-bearing prefix can't exceed total size)
//   - Alignof >= 1 (per the language spec)
//   - neither Sizeof nor ptrdata go negative
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

// typeCheckGoFuzz mirrors typeCheckFuzzInput from betteralign_fuzz_test.go,
// which is unreachable from non-test files because it takes *testing.T.
// Returns nil on parser failure, nil package, or upstream go/types panic —
// each treated by callers as the go-fuzz "uninteresting" signal.
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
		Error:    func(error) {},
		Importer: nil,
	}
	pkg, _ = conf.Check("fuzz", fset, []*ast.File{file}, nil)
	return pkg
}

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
