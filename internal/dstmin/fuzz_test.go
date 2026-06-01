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
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode"
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

// commentTexts returns the sorted multiset of each comment's word characters
// (letters and digits only) — a permutation-invariant fingerprint for
// drop/duplication detection that is immune to gofmt's role-dependent comment
// handling. gofmt reformats comment text by position: it adjusts spacing,
// reindents block-comment continuation lines, smart-quotes paired backticks
// and apostrophes, and normalizes plus/star list bullets to a dash, but only
// for comments it treats as documentation. Reorder moves comments between doc
// and floating roles, so any
// of those rewrites would make a faithfully-preserved comment look changed.
// Keeping only word characters discards every punctuation/whitespace rewrite
// at once while still detecting any word-bearing comment that is dropped or
// duplicated; punctuation-only comments collapse to "" and are ignored as
// informationless.
func commentTexts(file *ast.File) []string {
	var out []string
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if t := wordChars(c.Text); t != "" {
				out = append(out, t)
			}
		}
	}
	sort.Strings(out)
	return out
}

// wordChars reduces s to its letters and digits, dropping every other rune
// (markers, punctuation, whitespace, control bytes). See commentTexts for why
// this is the fingerprint that survives gofmt's reformatting.
func wordChars(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, s)
}

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

// TestCommentTextsImmuneToBlockCommentReindent pins the contract that
// commentTexts must ignore gofmt's block-comment reindentation. gofmt
// reflows a /* */ comment's continuation lines by stripping their common
// leading prefix, and its commonPrefix predicate (c <= ' ' || c == '*')
// treats any byte at or below space — control characters included — as
// strippable indentation. Which run it absorbs is position-dependent, so a
// reorder that relocates a block comment can drop a leading control byte on
// one side of the oracle but not the other. That is crasher
// c909a0fd0249d7c6: a continuation line led by U+0019 that gofmt keeps in
// the floating baseline but folds into indentation in dstmin's reordered
// output.
func TestCommentTextsImmuneToBlockCommentReindent(t *testing.T) {
	parse := func(src string) *ast.File {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		return f
	}
	withCtrl := parse("package p\n\n/* a\n\x19  b */\ntype T int\n")
	plain := parse("package p\n\n/* a\n  b */\ntype T int\n")
	if got, want := commentTexts(withCtrl), commentTexts(plain); !slices.Equal(got, want) {
		t.Errorf("control char leaked into comment multiset\n withCtrl: %q\n plain:    %q", got, want)
	}
}

// TestCommentTextsImmuneToListBullets pins the contract that commentTexts
// must ignore gofmt's go/doc/comment list-bullet normalization, which
// rewrites "+" and "*" item markers to "-" but only for comments in a
// doc-comment role. A reorder can move a list comment between a doc and a
// floating role, so the same comment serialises with a "+" bullet on one
// side of the oracle and "-" on the other. That is crasher e6acc7a2ace9d346.
func TestCommentTextsImmuneToListBullets(t *testing.T) {
	parse := func(src string) *ast.File {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		return f
	}
	plus := parse("package p\n\n//\t+\titem\ntype T int\n")
	dash := parse("package p\n\n//\t- item\ntype T int\n")
	if got, want := commentTexts(plus), commentTexts(dash); !slices.Equal(got, want) {
		t.Errorf("list bullet leaked into comment multiset\n plus: %q\n dash: %q", got, want)
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

// FuzzDecorateFileReorderAll reverses the field list of every NON-NESTED
// decoratable struct with >=2 fields, not just the first. With two or more
// such mutually non-nested structs this dirties several at once, driving
// spliceDirty's multi-splice cursor — the path FuzzDecorateFileReorder's
// single-struct reverse never reaches. Nested structs are deliberately left
// alone: betteralign never reorders an anonymous field-type struct, and doing
// so would permute a struct that another field prints inline, changing that
// field's signature and tripping the oracle. The oracle is order-independent —
// a reversal permutes fields but must preserve both the comment multiset and
// the file-wide field-signature multiset — so it holds however many structs
// were spliced.
func FuzzDecorateFileReorderAll(f *testing.F) {
	seeds := []string{
		"package p\n\ntype A struct {\n\ta int\n\tb int\n}\n\ntype B struct {\n\tc int\n\td int\n}\n",
		"package p\n\ntype A struct {\n\ta byte\n\tb int64\n}\n\ntype B struct {\n\t// doc c\n\tc int8 // trail c\n\td int64\n}\n\ntype C struct {\n\te int\n\tf int\n}\n",
		// Regression: a non-nested struct with an anonymous-struct field type
		// (inner) plus a sibling. inner must NOT be reversed, so its printed
		// signature stays stable on both sides of the oracle.
		"package p\n\ntype T struct {\n\tinner struct {\n\t\tx int\n\t\ty int\n\t}\n\tb int\n\ta int\n}\n\ntype U struct {\n\tc int\n\td int\n}\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	for _, s := range loadTestdataSeeds(f) {
		f.Add(s)
	}

	f.Fuzz(reorderAllInvariant)
}

// reorderAllInvariant is FuzzDecorateFileReorderAll's fuzz body. It reverses
// every non-nested decoratable >=2-field struct (nested ones are skipped — see
// FuzzDecorateFileReorderAll), requires at least two reversed so the multi-dirty
// splice path is actually exercised, and asserts the reprint parses and
// preserves the comment and field-signature multisets. Skips uninteresting
// inputs (unparseable, <2 reversible structs, ErrFormat, gofmt rejecting the
// input) rather than failing.
func reorderAllInvariant(t *testing.T, src string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "input.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Skip("input not valid Go")
	}
	dec := NewDecorator(fset)
	df := dec.DecorateFileSrc(file, []byte(src))

	// Only reverse structs not nested inside another struct. Reversing a struct
	// that is a field's anonymous type permutes a body another field prints
	// inline, changing that field's signature and breaking the order-independent
	// oracle below — and betteralign never reorders such a struct. Mutually
	// non-nested structs are exactly the siblings that drive the multi-splice
	// cursor.
	var allStructs []*ast.StructType
	ast.Inspect(file, func(n ast.Node) bool {
		if s, ok := n.(*ast.StructType); ok && s.Fields != nil {
			allStructs = append(allStructs, s)
		}
		return true
	})
	nested := func(c *ast.StructType) bool {
		for _, o := range allStructs {
			if o != c && o.Fields.Opening < c.Fields.Opening && c.Fields.Closing < o.Fields.Closing {
				return true
			}
		}
		return false
	}

	reversed := 0
	for _, st := range df.structs {
		if len(st.Fields.List) >= 2 && !nested(st.ast) {
			slices.Reverse(st.Fields.List)
			reversed++
		}
	}
	if reversed < 2 {
		t.Skip("fewer than two non-nested decoratable >=2-field structs")
	}

	var buf bytes.Buffer
	if err := Fprint(&buf, df); err != nil {
		if errors.Is(err, ErrFormat) {
			return
		}
		t.Fatalf("Fprint (reorder-all): %v", err)
	}
	outFset := token.NewFileSet()
	outFile, err := parser.ParseFile(outFset, "out.go", buf.Bytes(), parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Errorf("reorder-all produced invalid Go:\n=== OUTPUT ===\n%s\n=== PARSE ERROR ===\n%v\n=== INPUT ===\n%s", buf.String(), err, src)
		return
	}

	gofmtSrc, err := format.Source([]byte(src))
	if err != nil {
		t.Skip("input not formattable")
	}
	expFset := token.NewFileSet()
	expFile, err := parser.ParseFile(expFset, "gofmt.go", gofmtSrc, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Skip("gofmt'd input doesn't parse")
	}

	// Both fingerprints are order-independent: reversal permutes fields but
	// must never drop, duplicate, or byte-garble a comment or a field.
	if got, want := commentTexts(outFile), commentTexts(expFile); !slices.Equal(got, want) {
		t.Errorf("reorder-all changed the comment multiset\nWANT: %q\nGOT:  %q\n=== OUTPUT ===\n%s\n=== INPUT ===\n%s",
			want, got, buf.String(), src)
	}
	if got, want := allFieldSigs(outFset, outFile), allFieldSigs(expFset, expFile); !slices.Equal(got, want) {
		t.Errorf("reorder-all changed the field-signature multiset\nWANT: %q\nGOT:  %q\n=== OUTPUT ===\n%s\n=== INPUT ===\n%s",
			want, got, buf.String(), src)
	}
}

// allFieldSigs returns the sorted multiset of every struct field's signature
// in file. Reversing field lists permutes fields but must leave this exact
// multiset unchanged, so a field dropped, duplicated, or byte-garbled by a bad
// splice shows up as a multiset difference regardless of position.
func allFieldSigs(fset *token.FileSet, file *ast.File) []string {
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		if st, ok := n.(*ast.StructType); ok && st.Fields != nil {
			for _, fld := range st.Fields.List {
				out = append(out, fieldSig(fset, fld))
			}
		}
		return true
	})
	sort.Strings(out)
	return out
}

// FuzzCommentRun differential-tests commentRun's binary-search narrowing
// against the bruteCommentRun linear reference on arbitrary parser-accepted
// source, extending TestCommentRun_MatchesBruteForceForEveryStruct past its
// fixed fixtures. The two must agree for every struct body: go/parser
// guarantees f.Comments is position-sorted and non-overlapping, the invariant
// commentRun's two binary searches rely on.
func FuzzCommentRun(f *testing.F) {
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
		ast.Inspect(file, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			got := commentRun(file.Comments, st.Fields.Opening, st.Fields.Closing)
			want := bruteCommentRun(file.Comments, st.Fields.Opening, st.Fields.Closing)
			if len(got) != len(want) {
				t.Fatalf("commentRun len=%d want=%d\nSRC:\n%s", len(got), len(want), src)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("commentRun[%d]=%p want %p\nSRC:\n%s", i, got[i], want[i], src)
				}
			}
			return true
		})
	})
}
