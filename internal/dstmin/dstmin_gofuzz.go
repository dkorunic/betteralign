// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

//go:build gofuzz

// go-fuzz entry points for the dstmin decorator.
//
// Mirrors FuzzDecorateFileIdentity / FuzzDecorateFileReorder from
// fuzz_test.go using the dvyukov/go-fuzz signature. Gated by the `gofuzz`
// build tag (set automatically by go-fuzz-build) so the file is invisible to
// `go build` and `go test ./...`.
//
// Build & run:
//
//	go-fuzz-build -func=FuzzDecorateFileIdentity -o identity-fuzz.zip ./internal/dstmin
//	go-fuzz       -bin=identity-fuzz.zip -workdir=fuzz/decoratefileidentity
//
//	go-fuzz-build -func=FuzzDecorateFileReorder  -o reorder-fuzz.zip  ./internal/dstmin
//	go-fuzz       -bin=reorder-fuzz.zip  -workdir=fuzz/decoratefilereorder
//
// A starter seed corpus derived from the project testdata/ lives at
// ../../testdata/fuzz-corpus/. `task fuzz-go-build` (run from the repo root)
// automates the whole setup, including copying the corpus into each
// fuzz/<target>/corpus/ directory.
//
// Return codes: -1 reject, 0 keep-uninteresting, 1 keep-interesting. Bugs
// surface as panics that go-fuzz routes into <workdir>/crashers/.

package dstmin

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
)

// FuzzDecorateFileIdentity asserts that decoration followed by Fprint with no
// mutation reproduces the input byte-for-byte.
func FuzzDecorateFileIdentity(data []byte) int {
	src := string(data)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "input.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return -1
	}
	dec := NewDecorator(fset)
	df := dec.DecorateFileSrc(file, []byte(src))
	var buf bytes.Buffer
	if err := Fprint(&buf, df); err != nil {
		panic(fmt.Sprintf("Fprint (no mutation): %v", err))
	}
	if got := buf.String(); got != src {
		panic(fmt.Sprintf("identity round-trip changed source:\nINPUT:\n%q\nOUTPUT:\n%q", src, got))
	}
	return 1
}

// FuzzDecorateFileReorder swaps the first two fields of the first struct with
// >=2 fields, then checks that the printed output:
//
//   - parses as valid Go
//   - has the swapped fields in positions [0]/[1] of the same struct,
//     measured by canonical name+type signature
//
// Inputs that fail Fprint with ErrFormat are kept (return 0) — that's a
// documented dstmin rejection path, not a bug.
func FuzzDecorateFileReorder(data []byte) int {
	src := string(data)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "input.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return -1
	}
	dec := NewDecorator(fset)
	df := dec.DecorateFileSrc(file, []byte(src))
	var target *StructType
	for _, st := range df.structs {
		if len(st.Fields.List) >= 2 {
			target = st
			break
		}
	}
	if target == nil {
		return -1
	}
	targetIdx := -1
	{
		i := 0
		ast.Inspect(file, func(n ast.Node) bool {
			if targetIdx >= 0 {
				return false
			}
			if s, ok := n.(*ast.StructType); ok {
				if s == target.ast {
					targetIdx = i
				}
				i++
			}
			return true
		})
	}
	if targetIdx < 0 {
		panic("target struct not found in source AST")
	}

	target.Fields.List[0], target.Fields.List[1] = target.Fields.List[1], target.Fields.List[0]

	var buf bytes.Buffer
	if err := Fprint(&buf, df); err != nil {
		if errors.Is(err, ErrFormat) {
			return 0
		}
		panic(fmt.Sprintf("Fprint (reorder): %v", err))
	}
	outFset := token.NewFileSet()
	outFile, err := parser.ParseFile(outFset, "out.go", buf.Bytes(), parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		panic(fmt.Sprintf("reorder produced invalid Go:\n=== OUTPUT ===\n%s\n=== PARSE ERROR ===\n%v\n=== INPUT ===\n%s",
			buf.String(), err, src))
	}

	gofmtSrc, err := format.Source([]byte(src))
	if err != nil {
		return -1
	}
	expFset := token.NewFileSet()
	expFile, err := parser.ParseFile(expFset, "gofmt.go", gofmtSrc, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return -1
	}
	expStruct := nthStructGoFuzz(expFile, targetIdx)
	if expStruct == nil || expStruct.Fields == nil || len(expStruct.Fields.List) < 2 {
		return -1
	}
	expFields := append([]*ast.Field(nil), expStruct.Fields.List...)
	expFields[0], expFields[1] = expFields[1], expFields[0]

	outStruct := nthStructGoFuzz(outFile, targetIdx)
	if outStruct == nil || outStruct.Fields == nil {
		panic(fmt.Sprintf("output lost the target struct\n=== OUTPUT ===\n%s\n=== INPUT ===\n%s", buf.String(), src))
	}
	if len(outStruct.Fields.List) != len(expFields) {
		panic(fmt.Sprintf("field count: got %d, want %d\n=== OUTPUT ===\n%s\n=== INPUT ===\n%s",
			len(outStruct.Fields.List), len(expFields), buf.String(), src))
	}
	for i, want := range expFields {
		got := outStruct.Fields.List[i]
		wantSig := fieldSigGoFuzz(expFset, want)
		gotSig := fieldSigGoFuzz(outFset, got)
		if wantSig != gotSig {
			panic(fmt.Sprintf("field %d signature mismatch\nWANT: %q\nGOT:  %q\n=== OUTPUT ===\n%s\n=== INPUT ===\n%s",
				i, wantSig, gotSig, buf.String(), src))
		}
	}
	return 1
}

// fieldSigGoFuzz mirrors fieldSig from fuzz_test.go (unreachable here because
// it lives in a _test.go file). Canonical name+type signature, stable across
// gofmt normalizations when both inputs are gofmt'd.
func fieldSigGoFuzz(fset *token.FileSet, f *ast.Field) string {
	var buf bytes.Buffer
	for i, name := range f.Names {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(name.Name)
	}
	if len(f.Names) > 0 {
		buf.WriteByte(' ')
	}
	_ = printer.Fprint(&buf, fset, f.Type)
	return buf.String()
}

// nthStructGoFuzz returns the n-th *ast.StructType in preorder walk of file.
// Mirrors nthStruct from fuzz_test.go.
func nthStructGoFuzz(file *ast.File, n int) *ast.StructType {
	var found *ast.StructType
	i := 0
	ast.Inspect(file, func(node ast.Node) bool {
		if found != nil {
			return false
		}
		s, ok := node.(*ast.StructType)
		if !ok {
			return true
		}
		if i == n {
			found = s
			return false
		}
		i++
		return true
	})
	return found
}
