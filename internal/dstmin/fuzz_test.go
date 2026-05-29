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
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
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

	f.Fuzz(reorderInvariant)
}

// reorderInvariant is the shared body of the reorder fuzz target, factored
// out so the seed/fuzz driver (FuzzDecorateFileReorder) and the saved-crasher
// replay (TestReorderCrashersDoNotPanic) exercise byte-for-byte the same
// mutation and oracle. It reverses the first decoratable struct's fields,
// reprints, and asserts the output (a) parses, (b) preserves the comment
// multiset, and (c) places each field — by name+type signature — where the
// same reversal of the gofmt-normalised input would. Early-outs the fuzzer
// treats as "uninteresting" (unparseable input, no struct with >=2 fields,
// ErrFormat, gofmt rejecting the input) skip rather than fail.
//
// The dvyukov-tagged twin in dstmin_gofuzz.go must mirror this logic by hand:
// it lives under the gofuzz build tag and so cannot reach these _test.go
// helpers.
func reorderInvariant(t *testing.T, src string) {
	t.Helper()
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
	// Ordinal locates the same struct across the re-parsed files.
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

	// Reverse exercises every position and both blank-attachment paths.
	slices.Reverse(target.Fields.List)

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

	// gofmt'd baseline: both sides run format.Source, so normalizations cancel.
	gofmtSrc, err := format.Source([]byte(src))
	if err != nil {
		t.Skip("input not formattable")
	}
	expFset := token.NewFileSet()
	expFile, err := parser.ParseFile(expFset, "gofmt.go", gofmtSrc, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Skip("gofmt'd input doesn't parse")
	}

	// Reorder must not drop or duplicate comments; fieldSig can't see that.
	if got, want := commentTexts(outFile), commentTexts(expFile); !slices.Equal(got, want) {
		t.Errorf("reorder changed the comment multiset\nWANT: %q\nGOT:  %q\n=== OUTPUT ===\n%s\n=== INPUT ===\n%s",
			want, got, buf.String(), src)
	}

	expStruct := nthStruct(expFile, targetIdx)
	if expStruct == nil || expStruct.Fields == nil || len(expStruct.Fields.List) < 2 {
		t.Skip("gofmt'd input lost the target struct")
	}
	expFields := append([]*ast.Field(nil), expStruct.Fields.List...)
	slices.Reverse(expFields)

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

// commentTexts returns the sorted multiset of non-empty comment contents in
// file, with markers and all whitespace stripped — a permutation-invariant
// fingerprint for drop/duplication detection that is immune to gofmt's
// role-dependent comment handling. gofmt treats a comment differently by
// position: a doc comment "//0" gains a space ("// 0") and an empty doc
// comment is dropped, while the same comments floating between fields are
// left untouched. Reorder changes those roles, so raw text would diverge for
// reasons unrelated to preservation. Stripping markers+whitespace collapses
// the spacing variants, and dropping empty contents ignores informationless
// comments whose survival is immaterial.
//
// It also undoes gofmt's go/doc/comment typographic substitution: a pair
// of backticks becomes U+201C and a pair of apostrophes becomes U+201D,
// but only for comments gofmt treats as documentation. Because reorder
// can move a comment between a doc-comment role and a floating role, the
// same comment can serialise with curly quotes on one side of the oracle
// and the bare ASCII pair on the other; mapping the curly quotes back to
// their ASCII source keeps the fingerprint stable across that role
// change. These are the only two character-level rewrites go/doc/comment
// performs (dashes and the like are left verbatim).
func commentTexts(file *ast.File) []string {
	var out []string
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			t := c.Text
			switch {
			case strings.HasPrefix(t, "//"):
				t = t[2:]
			case strings.HasPrefix(t, "/*") && strings.HasSuffix(t, "*/"):
				t = t[2 : len(t)-2]
			}
			t = smartQuoteUndo.Replace(t)
			if t = strings.Join(strings.Fields(t), ""); t != "" {
				out = append(out, t)
			}
		}
	}
	sort.Strings(out)
	return out
}

// smartQuoteUndo reverses gofmt's go/doc/comment curly-quote substitution
// so commentTexts compares comment content, not gofmt's position-dependent
// styling. See commentTexts for why this matters across a reorder.
var smartQuoteUndo = strings.NewReplacer("“", "``", "”", "''")

// TestCommentTextsImmuneToSmartQuotes pins the contract that commentTexts
// must ignore gofmt's go/doc/comment typographic substitution. gofmt
// rewrites a backtick pair to U+201C and an apostrophe pair to U+201D,
// but only for comments it treats as documentation; a free-floating
// comment is left verbatim. Reorder can move a comment between a
// doc-comment role and a floating role, so the same comment legitimately
// serialises with the bare ASCII pair on one side of the oracle and the
// curly quote on the other — a difference of style, not of content.
// Without normalisation commentTexts reports that mismatch as a spurious
// "comment multiset changed", which is exactly crasher ed9d03123442f076
// (an apostrophe-pair comment that gofmt smart-quotes in the unreordered
// baseline but not in dstmin's reordered output).
func TestCommentTextsImmuneToSmartQuotes(t *testing.T) {
	parse := func(src string) *ast.File {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		return f
	}
	bare := parse("package p\n\n// a ''b'' and ``c``\ntype T int\n")
	smart := parse("package p\n\n// a ”b” and “c“\ntype T int\n")
	if got, want := commentTexts(bare), commentTexts(smart); !slices.Equal(got, want) {
		t.Errorf("smart-quote substitution leaked into comment multiset\n bare:  %q\n smart: %q", got, want)
	}
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
