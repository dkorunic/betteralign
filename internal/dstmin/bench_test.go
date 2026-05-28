// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package dstmin

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// buildLargeSource synthesises a Go file with N top-level struct types, each
// with M fields. Useful for exercising DecorateFileSrc's per-struct cost.
func buildLargeSource(numStructs, fieldsPerStruct int) []byte {
	var sb strings.Builder
	sb.WriteString("package bench\n\n")
	for s := 0; s < numStructs; s++ {
		fmt.Fprintf(&sb, "type S%d struct {\n", s)
		for f := 0; f < fieldsPerStruct; f++ {
			fmt.Fprintf(&sb, "\tf%d int\n", f)
		}
		sb.WriteString("}\n\n")
	}
	return []byte(sb.String())
}

// buildNestedSource synthesises a Go file with deeply nested struct types
// — useful for exercising the ancestor-stack walk.
func buildNestedSource(depth int) []byte {
	var sb strings.Builder
	sb.WriteString("package bench\n\ntype Nest struct {\n")
	for i := 0; i < depth; i++ {
		sb.WriteString(strings.Repeat("\t", i+1))
		fmt.Fprintf(&sb, "Level%d struct {\n", i)
	}
	// Innermost field.
	sb.WriteString(strings.Repeat("\t", depth+1))
	sb.WriteString("leaf int\n")
	// Close all the nested struct types.
	for i := depth; i > 0; i-- {
		sb.WriteString(strings.Repeat("\t", i))
		sb.WriteString("}\n")
	}
	sb.WriteString("}\n")
	return []byte(sb.String())
}

func parseBenchSource(b *testing.B, src []byte) (*token.FileSet, *ast.File, []byte) {
	b.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "bench.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		b.Fatalf("parse: %v", err)
	}
	return fset, f, src
}

func BenchmarkDecorateFileSrc_Small(b *testing.B) {
	fset, f, src := parseBenchSource(b, buildLargeSource(10, 5))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dec := NewDecorator(fset)
		_ = dec.DecorateFileSrc(f, src)
	}
}

func BenchmarkDecorateFileSrc_Medium(b *testing.B) {
	fset, f, src := parseBenchSource(b, buildLargeSource(100, 20))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dec := NewDecorator(fset)
		_ = dec.DecorateFileSrc(f, src)
	}
}

func BenchmarkDecorateFileSrc_Large(b *testing.B) {
	fset, f, src := parseBenchSource(b, buildLargeSource(500, 30))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dec := NewDecorator(fset)
		_ = dec.DecorateFileSrc(f, src)
	}
}

func BenchmarkDecorateFileSrc_Nested(b *testing.B) {
	fset, f, src := parseBenchSource(b, buildNestedSource(20))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dec := NewDecorator(fset)
		_ = dec.DecorateFileSrc(f, src)
	}
}

func BenchmarkFprint_NoDirty(b *testing.B) {
	fset, f, src := parseBenchSource(b, buildLargeSource(100, 20))
	dec := NewDecorator(fset)
	df := dec.DecorateFileSrc(f, src)
	var buf bytes.Buffer
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		_ = Fprint(&buf, df)
	}
}
