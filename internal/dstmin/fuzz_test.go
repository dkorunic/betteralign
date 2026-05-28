// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package dstmin

import (
	"bytes"
	"errors"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// handCraftedSeeds covers edge cases the testdata corpus does not exercise.
var handCraftedSeeds = []string{
	"package p\n",
	"package p\n\ntype S struct{}\n",
	"package p\n\ntype S struct {\n\ta int\n\tb int\n}\n",
	"package p\n\ntype S struct { // betteralign:ignore\n\ta int\n\tb int\n}\n",
	"package p\n\ntype S struct {\n\t// doc\n\ta int\n\tb int\n\t// trailing\n}\n",
	"package p\n\ntype S struct {\n\tA, B int\n\tc string\n}\n",
	"package p\n\ntype Outer struct {\n\tInner struct {\n\t\tx int\n\t\ty int\n\t}\n\tb int\n}\n",
	"package p\n\ntype S struct {\n\tx int // line\n\ty int /* block */\n}\n",
	"package p\n\ntype S struct {\n\t// floating1\n\ta int\n\t// floating2\n\n\t// floating3\n\tb int\n}\n",
}

// loadTestdataSeeds walks the project's testdata/ tree from this package's
// directory and returns the bytes of every .go and .go.golden file as fuzz
// seeds. These cover realistic struct shapes the analyzer is exercised
// against: multi-package layouts, generated files, positional literals,
// opt-in directives, multi-name fields, embedded types, generic
// instantiations. Falls back to an empty slice on I/O failure so seeding
// remains a strict superset of handCraftedSeeds.
func loadTestdataSeeds(f *testing.F) []string {
	f.Helper()
	var seeds []string
	root := filepath.Join("..", "..", "testdata")
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".go.golden") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		seeds = append(seeds, string(data))
		return nil
	})
	if walkErr != nil {
		f.Logf("walk testdata at %s: %v (continuing with hand-crafted seeds only)", root, walkErr)
	}
	return seeds
}

func FuzzDecorateFileIdentity(f *testing.F) {
	for _, s := range handCraftedSeeds {
		f.Add(s)
	}
	for _, s := range loadTestdataSeeds(f) {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, src string) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "input.go", src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Skip("input not valid Go")
		}
		dec := NewDecorator(fset)
		df := dec.DecorateFileSrc(file, []byte(src))
		var buf bytes.Buffer
		if err := Fprint(&buf, df); err != nil {
			t.Fatalf("Fprint (no mutation): %v", err)
		}
		if got := buf.String(); got != src {
			t.Errorf("identity round-trip changed source:\nINPUT:\n%q\nOUTPUT:\n%q", src, got)
		}
	})
}

func FuzzDecorateFileReorder(f *testing.F) {
	reorderHandSeeds := []string{
		"package p\n\ntype S struct {\n\ta int\n\tb int\n}\n",
		"package p\n\ntype S struct {\n\ta byte\n\tb int64\n\tc byte\n}\n",
		"package p\n\ntype S struct {\n\t// doc for a\n\ta byte // trailing a\n\tb int64 // trailing b\n}\n",
		"package p\n\ntype S struct {\n\tA, B int\n\tc string\n}\n",
		"package p\n\ntype Outer struct {\n\tInner struct {\n\t\tx int\n\t}\n\tb int\n\ta int\n}\n",
	}
	for _, s := range reorderHandSeeds {
		f.Add(s)
	}
	for _, s := range loadTestdataSeeds(f) {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, src string) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "input.go", src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Skip("input not valid Go")
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
			t.Skip("no struct with >=2 fields")
		}
		// Target's ordinal among all *ast.StructType nodes — used to locate it in gofmt'd input and output.
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
			t.Fatal("target struct not found in source AST")
		}

		target.Fields.List[0], target.Fields.List[1] = target.Fields.List[1], target.Fields.List[0]

		var buf bytes.Buffer
		if err := Fprint(&buf, df); err != nil {
			if errors.Is(err, ErrFormat) {
				return
			}
			t.Fatalf("Fprint (reorder): %v", err)
		}
		outFset := token.NewFileSet()
		outFile, err := parser.ParseFile(outFset, "out.go", buf.Bytes(), parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Errorf("reorder produced invalid Go:\n=== OUTPUT ===\n%s\n=== PARSE ERROR ===\n%v\n=== INPUT ===\n%s", buf.String(), err, src)
			return
		}

		// Baseline expected: gofmt(input) with the same swap. Both sides go through
		// format.Source, so gofmt normalizations apply equally and cancel out.
		gofmtSrc, err := format.Source([]byte(src))
		if err != nil {
			t.Skip("input not formattable")
		}
		expFset := token.NewFileSet()
		expFile, err := parser.ParseFile(expFset, "gofmt.go", gofmtSrc, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Skip("gofmt'd input doesn't parse")
		}
		expStruct := nthStruct(expFile, targetIdx)
		if expStruct == nil || expStruct.Fields == nil || len(expStruct.Fields.List) < 2 {
			t.Skip("gofmt'd input lost the target struct")
		}
		expFields := append([]*ast.Field(nil), expStruct.Fields.List...)
		expFields[0], expFields[1] = expFields[1], expFields[0]

		outStruct := nthStruct(outFile, targetIdx)
		if outStruct == nil || outStruct.Fields == nil {
			t.Errorf("output lost the target struct\n=== OUTPUT ===\n%s\n=== INPUT ===\n%s", buf.String(), src)
			return
		}
		if len(outStruct.Fields.List) != len(expFields) {
			t.Errorf("field count: got %d, want %d\n=== OUTPUT ===\n%s\n=== INPUT ===\n%s",
				len(outStruct.Fields.List), len(expFields), buf.String(), src)
			return
		}
		for i, want := range expFields {
			got := outStruct.Fields.List[i]
			wantSig := fieldSig(expFset, want)
			gotSig := fieldSig(outFset, got)
			if wantSig != gotSig {
				t.Errorf("field %d signature mismatch\nWANT: %q\nGOT:  %q\n=== OUTPUT ===\n%s\n=== INPUT ===\n%s",
					i, wantSig, gotSig, buf.String(), src)
			}
		}
	})
}

// fieldSig is a canonical name+type signature; stable across gofmt normalizations when both inputs are gofmt'd.
func fieldSig(fset *token.FileSet, f *ast.Field) string {
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

// nthStruct returns the n-th *ast.StructType in preorder walk of file.
func nthStruct(file *ast.File, n int) *ast.StructType {
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

