// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

//go:build !gofuzz

package dstmin

import (
	"bytes"
	"errors"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReorderCrashersDoNotPanic replays every saved go-fuzz crasher input
// through the same logic as FuzzDecorateFileReorder and asserts none of
// them panics or trips the field-count / signature mismatch. Acts as the
// regression net for crashes already fixed under fuzz/decoratefilereorder/
// — any future change that reintroduces one will fail the corresponding
// subtest. Inputs are read from ../../fuzz/decoratefilereorder/crashers,
// which is populated by `task fuzz-go-decoratefilereorder`; the test
// skips silently when that directory is absent so CI without the fuzz
// workdir stays green. Each subtest is named after the crasher's hash so
// failures point straight back at the offending input.
func TestReorderCrashersDoNotPanic(t *testing.T) {
	dir := filepath.Join("..", "..", "fuzz", "decoratefilereorder", "crashers")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("crashers dir unavailable: %v", err)
	}
	var inputs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.Contains(name, ".") {
			continue
		}
		inputs = append(inputs, name)
	}
	if len(inputs) == 0 {
		t.Skip("no crasher inputs found")
	}
	for _, name := range inputs {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("read crasher %s: %v", name, err)
			}
			runReorderInvariant(t, data)
		})
	}
}

// runReorderInvariant mirrors the body of FuzzDecorateFileReorder so the
// regression test exercises exactly the path go-fuzz instrumented: parse,
// decorate, swap the first two fields of the first struct with at least
// two fields, Fprint, re-parse, then compare field count and per-field
// name+type signature against the gofmt-normalised input. Failures
// surface via t.Errorf / t.Fatalf rather than panic so go test reports
// each crasher independently; inputs the original harness would have
// returned -1 for (parse failure, no decoratable struct, ErrFormat) are
// quietly accepted here, matching go-fuzz's "uninteresting" classification.
func runReorderInvariant(t *testing.T, data []byte) {
	t.Helper()
	src := string(data)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "input.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return
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
		return
	}
	targetIdx := -1
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
	if targetIdx < 0 {
		t.Fatal("target struct not found in source AST")
	}
	target.Fields.List[0], target.Fields.List[1] = target.Fields.List[1], target.Fields.List[0]
	var buf bytes.Buffer
	if err := Fprint(&buf, df); err != nil {
		if errors.Is(err, ErrFormat) {
			return
		}
		t.Fatalf("Fprint: %v", err)
	}
	outFset := token.NewFileSet()
	outFile, err := parser.ParseFile(outFset, "out.go", buf.Bytes(), parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("reorder produced invalid Go: %v\nOUTPUT:\n%s", err, buf.String())
	}
	gofmtSrc, err := format.Source([]byte(src))
	if err != nil {
		return
	}
	expFset := token.NewFileSet()
	expFile, err := parser.ParseFile(expFset, "gofmt.go", gofmtSrc, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return
	}
	expStruct := nthStruct(expFile, targetIdx)
	if expStruct == nil || expStruct.Fields == nil || len(expStruct.Fields.List) < 2 {
		return
	}
	expFields := append([]*ast.Field(nil), expStruct.Fields.List...)
	expFields[0], expFields[1] = expFields[1], expFields[0]
	outStruct := nthStruct(outFile, targetIdx)
	if outStruct == nil || outStruct.Fields == nil {
		t.Fatalf("output lost the target struct\nOUTPUT:\n%s", buf.String())
	}
	if len(outStruct.Fields.List) != len(expFields) {
		t.Errorf("field count: got %d, want %d\nOUTPUT:\n%s",
			len(outStruct.Fields.List), len(expFields), buf.String())
		return
	}
	for i, want := range expFields {
		got := outStruct.Fields.List[i]
		wantSig := fieldSig(expFset, want)
		gotSig := fieldSig(outFset, got)
		if wantSig != gotSig {
			t.Errorf("field %d signature mismatch\nWANT: %q\nGOT:  %q\nOUTPUT:\n%s",
				i, wantSig, gotSig, buf.String())
		}
	}
}
