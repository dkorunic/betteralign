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

// buildCommentedSource synthesises a Go file with numStructs top-level
// structs, each carrying a type doc comment and a lead-doc comment on every
// field. Unlike buildLargeSource it populates f.Comments, so it exercises the
// comment-routing path (the three guards + decorateComments) that scales with
// the comment count — the quadratic-risk surface that commentRun narrows.
func buildCommentedSource(numStructs, fieldsPerStruct int) []byte {
	var sb strings.Builder
	sb.WriteString("package bench\n\n")
	for s := 0; s < numStructs; s++ {
		fmt.Fprintf(&sb, "// S%d is a doc comment.\ntype S%d struct {\n", s, s)
		for f := 0; f < fieldsPerStruct; f++ {
			fmt.Fprintf(&sb, "\t// f%d doc\n\tf%d int\n", f, f)
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

// BenchmarkDecorateFileSrc_Commented mirrors the Medium comment-free case in
// struct/field count but adds a doc comment per type and per field, so it
// drives the comment-routing path. Compare against _Medium to see the cost of
// comment handling; track it across changes to commentRun.
func BenchmarkDecorateFileSrc_Commented(b *testing.B) {
	fset, f, src := parseBenchSource(b, buildCommentedSource(100, 20))
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

// BenchmarkDecorateFileSrc_CommentScaling isolates decorateComments' per-comment
// field scans. Each case decorates a SINGLE struct with F fields so the
// comment-run length scales with F; decorateComments runs an O(F) rule sweep
// per comment, making the routing O(F^2) for one struct. The commented/ cases
// carry a lead-doc on every field (run length ~= F); the commentfree/ cases are
// the same field count with no comments (the O(F) baseline from buildStruct +
// Pass 4). If routing is the quadratic, commented/ grows ~4x per doubling while
// commentfree/ stays ~linear, and their gap widens with F.
func BenchmarkDecorateFileSrc_CommentScaling(b *testing.B) {
	for _, fpf := range []int{100, 200, 400, 800} {
		commented := buildCommentedSource(1, fpf)
		bare := buildLargeSource(1, fpf)
		b.Run(fmt.Sprintf("commented/fields=%d", fpf), func(b *testing.B) {
			fset, f, src := parseBenchSource(b, commented)
			b.ReportAllocs()
			for b.Loop() {
				dec := NewDecorator(fset)
				_ = dec.DecorateFileSrc(f, src)
			}
		})
		b.Run(fmt.Sprintf("commentfree/fields=%d", fpf), func(b *testing.B) {
			fset, f, src := parseBenchSource(b, bare)
			b.ReportAllocs()
			for b.Loop() {
				dec := NewDecorator(fset)
				_ = dec.DecorateFileSrc(f, src)
			}
		})
	}
}
