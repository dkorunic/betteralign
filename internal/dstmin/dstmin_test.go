// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package dstmin

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// parseSource writes src into a temp file under t.TempDir() and parses it
// with parser.ParseComments. Returns the *ast.File and *token.FileSet bound
// to the tmp file. Decoration reads from disk, so source must exist on disk.
func parseSource(t *testing.T, src string) (*token.FileSet, *ast.File, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "input.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write tmp source: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse tmp source: %v", err)
	}
	return fset, f, path
}

func compare(t *testing.T, expect, got string) {
	t.Helper()
	if expect != got {
		t.Errorf("\nexpect:\n%s\n--- got:\n%s", expect, got)
	}
}

func TestDecorateFile_BuildsStructNodeMap(t *testing.T) {
	src := `package p

type S struct {
	a int
	b int
}
`
	fset, f, _ := parseSource(t, src)

	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}

	// Find the AST struct.
	var astStruct *ast.StructType
	ast.Inspect(f, func(n ast.Node) bool {
		if s, ok := n.(*ast.StructType); ok {
			astStruct = s
			return false
		}
		return true
	})
	if astStruct == nil {
		t.Fatal("no *ast.StructType found in fixture")
	}

	got, ok := dec.Dst.Nodes[astStruct].(*StructType)
	if !ok {
		t.Fatalf("dec.Dst.Nodes[astStruct] = %T, want *StructType", dec.Dst.Nodes[astStruct])
	}
	if got.ast != astStruct {
		t.Errorf("got.ast = %p, want %p", got.ast, astStruct)
	}
	if got.Fields == nil || len(got.Fields.List) != 2 {
		t.Errorf("Fields.List len = %d, want 2", len(got.Fields.List))
	}
	// df is non-nil whenever DecorateFile returned no error (checked above).
	if len(df.structs) != 1 {
		t.Errorf("df.structs len = %d, want 1", len(df.structs))
	}
}

func TestDecorateFile_RecordsFieldSpans(t *testing.T) {
	src := "package p\n\ntype S struct {\n\ta int\n\tb int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	if len(df.structs) != 1 {
		t.Fatalf("len(df.structs) = %d, want 1", len(df.structs))
	}
	st := df.structs[0]
	if got := string(df.source[st.bodyStart:st.bodyEnd]); got != "\ta int\n\tb int\n" {
		t.Errorf("struct body = %q, want %q", got, "\ta int\n\tb int\n")
	}
	if len(st.Fields.List) != 2 {
		t.Fatalf("len(Fields.List) = %d, want 2", len(st.Fields.List))
	}
	got := string(df.source[st.Fields.List[0].bodyStart:st.Fields.List[0].bodyEnd])
	if got != "\ta int\n" {
		t.Errorf("field 0 body = %q, want %q", got, "\ta int\n")
	}
	got = string(df.source[st.Fields.List[1].bodyStart:st.Fields.List[1].bodyEnd])
	if got != "\tb int\n" {
		t.Errorf("field 1 body = %q, want %q", got, "\tb int\n")
	}
	if st.indent != "\t" {
		t.Errorf("indent = %q, want %q", st.indent, "\t")
	}
}

func TestDecorateFile_LeadDocAttachedToField(t *testing.T) {
	src := "package p\n\ntype S struct {\n\t// lead for a\n\ta int\n\tb int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	st := df.structs[0]
	if got := st.Fields.List[0].lead; len(got) != 1 || got[0] != "// lead for a" {
		t.Errorf("field 0 lead = %q, want [%q]", got, "// lead for a")
	}
	if got := st.Fields.List[1].lead; len(got) != 0 {
		t.Errorf("field 1 lead = %q, want empty", got)
	}
}

func TestDecorateFile_MultiLineLeadDoc(t *testing.T) {
	src := "package p\n\ntype S struct {\n\t// line one\n\t// line two\n\ta int\n\tb int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	st := df.structs[0]
	got := st.Fields.List[0].lead
	want := []string{"// line one", "// line two"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("field a lead = %q, want %q", got, want)
	}
	if l := st.Fields.List[1].lead; len(l) != 0 {
		t.Errorf("field b lead = %q, want empty", l)
	}
}

func TestDecorateFile_BlockCommentLeadDoc(t *testing.T) {
	src := "package p\n\ntype S struct {\n\t/* lead */\n\ta int\n\tb int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	st := df.structs[0]
	if got := st.Fields.List[0].lead; len(got) != 1 || got[0] != "/* lead */" {
		t.Errorf("field a lead = %q, want [%q]", got, "/* lead */")
	}
	if l := st.Fields.List[1].lead; len(l) != 0 {
		t.Errorf("field b lead = %q, want empty", l)
	}
}

func TestDecorateFile_TrailingLineCommentInField(t *testing.T) {
	src := "package p\n\ntype S struct {\n\ta int // trailing a\n\tb int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	st := df.structs[0]
	if got := string(df.source[st.Fields.List[0].bodyStart:st.Fields.List[0].bodyEnd]); got != "\ta int // trailing a\n" {
		t.Errorf("field 0 body = %q, want %q", got, "\ta int // trailing a\n")
	}
}

func TestDecorateFile_FloatingBetweenFieldsAttachesToPrevious(t *testing.T) {
	src := "package p\n\ntype S struct {\n\ta int\n\t// floating after a, before b\n\n\tb int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	st := df.structs[0]
	got := string(df.source[st.Fields.List[0].trailStart:st.Fields.List[0].trailEnd])
	if got != "\t// floating after a, before b\n\n" {
		t.Errorf("field 0 trail = %q, want %q", got, "\t// floating after a, before b\n\n")
	}
	if l := st.Fields.List[1].lead; len(l) != 0 {
		t.Errorf("field 1 lead = %q, want empty (blank line broke doc binding)", l)
	}
}

func TestDecorateFile_FloatingAfterLastFieldAttachesToLast(t *testing.T) {
	src := "package p\n\ntype S struct {\n\ta int\n\tb int\n\t// trailing\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	st := df.structs[0]
	got := string(df.source[st.Fields.List[1].trailStart:st.Fields.List[1].trailEnd])
	if got != "\t// trailing\n" {
		t.Errorf("field 1 trail = %q, want %q", got, "\t// trailing\n")
	}
}

// A comment on the { line is not routed to any field: the { line sits outside
// the body span, so the verbatim splice must preserve it across a reorder.
func TestDecorateFile_OpeningBraceComment(t *testing.T) {
	src := "package p\n\ntype S struct { // betteralign:ignore\n\ta int\n\tb int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	st := df.structs[0]
	for i, fld := range st.Fields.List {
		if len(fld.lead) != 0 {
			t.Errorf("field %d lead = %q, want empty ({-line comment must stay verbatim)", i, fld.lead)
		}
	}
	st.Fields.List[0], st.Fields.List[1] = st.Fields.List[1], st.Fields.List[0]
	var buf bytes.Buffer
	if err := Fprint(&buf, df); err != nil {
		t.Fatalf("Fprint: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("struct { // betteralign:ignore")) {
		t.Errorf("{-line comment lost after reorder:\n%s", buf.String())
	}
}

func TestDecorateFile_NestedStructCommentNotCapturedByOuter(t *testing.T) {
	src := "package p\n\ntype Outer struct {\n\tInner struct {\n\t\t// lead for a\n\t\ta int\n\t}\n\tb int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	if len(df.structs) != 2 {
		t.Fatalf("len(df.structs) = %d, want 2 (Outer + Inner)", len(df.structs))
	}
	// df.structs is in source order, so Outer is first (it spans Inner).
	outer := df.structs[0]
	inner := df.structs[1]
	for i, fld := range outer.Fields.List {
		if got := fld.lead; len(got) != 0 {
			t.Errorf("outer field %d lead = %q, want empty (comment belongs to Inner.a)", i, got)
		}
	}
	if got := inner.Fields.List[0].lead; len(got) != 1 || got[0] != "// lead for a" {
		t.Errorf("inner.a.lead = %q, want [%q]", got, "// lead for a")
	}
}

func TestDecorateFile_CommentBetweenIdentifierAndType(t *testing.T) {
	// Unnamed position in upstream dst; dstmin survives via byte-splicing.
	src := "package p\n\ntype S struct {\n\ta /*c*/ int\n\tb int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	st := df.structs[0]
	aBody := string(df.source[st.Fields.List[0].bodyStart:st.Fields.List[0].bodyEnd])
	if aBody != "\ta /*c*/ int\n" {
		t.Errorf("field a body = %q, want %q (inline comment must be inside body span)", aBody, "\ta /*c*/ int\n")
	}
	if got := st.Fields.List[0].lead; len(got) != 0 {
		t.Errorf("field a lead = %q, want empty (inline comment, not lead-doc)", got)
	}
}

func TestDecorateFile_MultiLineBlockCommentOnFieldLastLine(t *testing.T) {
	src := "package p\n\ntype S struct {\n\ta int /* start\n\t   end */\n\tb int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	st := df.structs[0]
	// Block comment is field a's trail; field b's lead must stay empty.
	if got := st.Fields.List[1].lead; len(got) != 0 {
		t.Errorf("field b lead = %q, want empty (block comment belongs to field a)", got)
	}
	// Field a's body span ends at the newline after `a int /* start`.
	aBody := string(df.source[st.Fields.List[0].bodyStart:st.Fields.List[0].bodyEnd])
	if aBody != "\ta int /* start\n" {
		t.Errorf("field a body = %q, want %q", aBody, "\ta int /* start\n")
	}
	// Field a's trail should cover the closing `end */` line.
	aTrail := string(df.source[st.Fields.List[0].trailStart:st.Fields.List[0].trailEnd])
	if aTrail != "\t   end */\n" {
		t.Errorf("field a trail = %q, want %q", aTrail, "\t   end */\n")
	}
}

func TestDecorateFile_FieldOnBraceLineIsSkipped(t *testing.T) {
	src := "package p\n\ntype S struct { a int\n\tb byte\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	if len(df.structs) != 0 {
		t.Errorf("len(df.structs) = %d, want 0 (first-field-on-brace-line is non-decoratable)", len(df.structs))
	}
	var out bytes.Buffer
	if err := Fprint(&out, df); err != nil {
		t.Fatalf("Fprint: %v", err)
	}
	if out.String() != src {
		t.Errorf("Fprint changed output for skipped struct:\ngot:\n%s\nwant:\n%s", out.String(), src)
	}
}

// BUG-31: last field on } line — symmetric to the existing first-field-on-{ guard.
func TestDecorateFile_LastFieldOnBraceLineIsSkipped(t *testing.T) {
	src := "package p\n\ntype S struct {\n\ta int\n\tb byte }\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	if len(df.structs) != 0 {
		t.Errorf("len(df.structs) = %d, want 0 (last-field-on-brace-line is non-decoratable)", len(df.structs))
	}
	var out bytes.Buffer
	if err := Fprint(&out, df); err != nil {
		t.Fatalf("Fprint: %v", err)
	}
	if out.String() != src {
		t.Errorf("Fprint changed output for skipped struct:\ngot:\n%s\nwant:\n%s", out.String(), src)
	}
}

// BUG-32: block comment opens on { line, closes inside body — splice halves get separated.
func TestDecorateFile_OpenBraceCrossingBlockCommentIsSkipped(t *testing.T) {
	src := "package p\n\ntype S struct { /*\n*/a int\n\tb int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	if len(df.structs) != 0 {
		t.Errorf("len(df.structs) = %d, want 0 (brace-crossing block comment is non-decoratable)", len(df.structs))
	}
	var out bytes.Buffer
	if err := Fprint(&out, df); err != nil {
		t.Fatalf("Fprint: %v", err)
	}
	if out.String() != src {
		t.Errorf("Fprint changed output for skipped struct:\ngot:\n%s\nwant:\n%s", out.String(), src)
	}
}

// BUG-32 symmetric: block comment opens inside body, closes on } line.
func TestDecorateFile_CloseBraceCrossingBlockCommentIsSkipped(t *testing.T) {
	src := "package p\n\ntype S struct {\n\ta int\n\tb int /*\n*/ }\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	if len(df.structs) != 0 {
		t.Errorf("len(df.structs) = %d, want 0 (close-brace crossing block comment is non-decoratable)", len(df.structs))
	}
	var out bytes.Buffer
	if err := Fprint(&out, df); err != nil {
		t.Fatalf("Fprint: %v", err)
	}
	if out.String() != src {
		t.Errorf("Fprint changed output for skipped struct:\ngot:\n%s\nwant:\n%s", out.String(), src)
	}
}

// BUG-33: multi-line block comment ends on a field's first line — halves cross partition.
func TestDecorateFile_InBodyMultiLineBlockCommentIsSkipped(t *testing.T) {
	src := "package p\n\ntype S struct {\n/*\n*/a int\n\tb int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	if len(df.structs) != 0 {
		t.Errorf("len(df.structs) = %d, want 0 (multi-line block comment in body is non-decoratable)", len(df.structs))
	}
	var out bytes.Buffer
	if err := Fprint(&out, df); err != nil {
		t.Fatalf("Fprint: %v", err)
	}
	if out.String() != src {
		t.Errorf("Fprint changed output for skipped struct:\ngot:\n%s\nwant:\n%s", out.String(), src)
	}
}

// BUG-44: a block comment on the } line (before the brace). Routed to a
// field's trail, the trail span runs to lineEndOffset of the } line and
// swallows the closing brace; on reorder the brace is emitted mid-body and
// the struct closes early, dropping a field. The single-line /* */ case
// slips past the cgStart==cgEnd skip and the multi-line brace-cross check in
// hasUnsafeBlockComment, so it must be rejected explicitly. Source from
// gosentry/LibAFL crasher 5330502779b79821, reduced.
func TestDecorateFile_BlockCommentOnCloseBraceLineIsSkipped(t *testing.T) {
	src := "package p\n\ntype S struct {\n\ta int\n\tb int\n\t/* note */ }\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	if len(df.structs) != 0 {
		t.Errorf("len(df.structs) = %d, want 0 (block comment on } line is non-decoratable)", len(df.structs))
	}
	var out bytes.Buffer
	if err := Fprint(&out, df); err != nil {
		t.Fatalf("Fprint: %v", err)
	}
	if out.String() != src {
		t.Errorf("Fprint changed output for skipped struct:\ngot:\n%s\nwant:\n%s", out.String(), src)
	}
}

// BUG-41: a comment group holding more than one comment where any of them
// is a block comment (here a line comment immediately followed by a block
// comment, with no blank line, so the parser fuses them into one group).
// Routing assigns the group as a unit, so the trailing/lead bytes can't be
// kept consistent once reorder moves the anchor — reject the struct instead.
func TestDecorateFile_MultiCommentGroupWithBlockIsSkipped(t *testing.T) {
	src := "package p\n\ntype S struct {\n\ta int\n\t// c\n\t/* d */\n\tb int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	if len(df.structs) != 0 {
		t.Errorf("len(df.structs) = %d, want 0 (multi-comment group with block is non-decoratable)", len(df.structs))
	}
	var out bytes.Buffer
	if err := Fprint(&out, df); err != nil {
		t.Fatalf("Fprint: %v", err)
	}
	if out.String() != src {
		t.Errorf("Fprint changed output for skipped struct:\ngot:\n%s\nwant:\n%s", out.String(), src)
	}
}

// BUG-39: a floating comment that ends on the line just before a field but
// starts inside the *previous* field's already-extended trailing span — here
// `a`'s multi-line trailing block, on whose closing `*/` line a `//x` line
// comment also sits. Routing `//x` as `b`'s lead-doc would emit it twice on
// reorder; it must extend `a`'s trail instead. The struct stays decoratable,
// so assert that and that the field-reversed reprint preserves the comment
// multiset and field count (which it does not if the BUG-39 branch is gone).
func TestReorder_FloatingCommentOverlappingPrevFieldTrail(t *testing.T) {
	src := "package p\n\ntype S struct {\n\ta int /*\n\t*/ //x\n\tb int\n}\n"
	assertDecoratedThenReorderFaithful(t, src)
}

// BUG-36: a floating block comment whose end overshoots the next field's body
// start. Attaching it to the previous field's trail would double-write the
// overlapping bytes on reorder, so comment routing skips it. The shape is
// degenerate by nature — a clean layout that puts a comment past a field
// boundary is already rejected by the brace/field-line guards — so this is a
// reduced fuzz input; the `*/*/` run is what positions the comment across the
// boundary. reorderInvariant fails here if the skip is removed (the comment
// is duplicated and the field count changes).
func TestReorder_FloatingCommentOverlappingNextFieldBody(t *testing.T) {
	src := "package p\ntype S struct{\nd\n*/*/\n*/d//x\n}\n"
	assertDecoratedThenReorderFaithful(t, src)
}

// assertDecoratedThenReorderFaithful pins that src yields a decoratable
// struct (so the comment-routing path under test is actually exercised, not
// silently skipped) and that reversing its fields and reprinting stays
// faithful, reusing the FuzzDecorateFileReorder oracle.
func assertDecoratedThenReorderFaithful(t *testing.T, src string) {
	t.Helper()
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	if len(df.structs) == 0 {
		t.Fatalf("struct was not decorated; comment-routing path not exercised")
	}
	reorderInvariant(t, src)
}

// BUG-34: `A; B int` — two fields share a source line and a body span.
func TestDecorateFile_SameLineFieldsViaSemicolonIsSkipped(t *testing.T) {
	src := "package p\n\ntype S struct {\n\ta int; b int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	if len(df.structs) != 0 {
		t.Errorf("len(df.structs) = %d, want 0 (same-line fields are non-decoratable)", len(df.structs))
	}
	var out bytes.Buffer
	if err := Fprint(&out, df); err != nil {
		t.Fatalf("Fprint: %v", err)
	}
	if out.String() != src {
		t.Errorf("Fprint changed output for skipped struct:\ngot:\n%s\nwant:\n%s", out.String(), src)
	}
}

// BUG-42 regression. Cases sweep both placement (trailing on carrier, own
// line, no-args, last-field carrier) and prefix (//go:build, // +build with
// space, //+build glued) because each triggers gofmt's directive-promotion
// path via a different recogniser branch. Every form must skip decoration
// so dstmin re-emits the source untouched; the trailing assertion compares
// the buffer to the original input byte-for-byte.
func TestDecorateFile_MidStructBuildConstraintIsSkipped(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"go_build_trailing", "package p\n\ntype S struct {\n\ta int //go:build foo\n\tb int\n}\n"},
		{"go_build_own_line", "package p\n\ntype S struct {\n\ta int\n\t//go:build foo\n\tb int\n}\n"},
		{"go_build_no_args", "package p\n\ntype S struct {\n\ta int //go:build\n\tb int\n}\n"},
		{"go_build_last_field", "package p\n\ntype S struct {\n\ta int\n\tb int //go:build foo\n}\n"},
		{"plus_build_spaced", "package p\n\ntype S struct {\n\ta int // +build foo\n\tb int\n}\n"},
		{"plus_build_glued", "package p\n\ntype S struct {\n\ta int //+build foo\n\tb int\n}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset, f, _ := parseSource(t, tc.src)
			dec := NewDecorator(fset)
			df, err := dec.DecorateFile(f)
			if err != nil {
				t.Fatalf("DecorateFile: %v", err)
			}
			if len(df.structs) != 0 {
				t.Errorf("len(df.structs) = %d, want 0 (build constraint mid-struct is non-decoratable)", len(df.structs))
			}
			var out bytes.Buffer
			if err := Fprint(&out, df); err != nil {
				t.Fatalf("Fprint: %v", err)
			}
			if out.String() != tc.src {
				t.Errorf("Fprint changed output for skipped struct:\ngot:\n%s\nwant:\n%s", out.String(), tc.src)
			}
		})
	}
}

// BUG-43 regression. Each case carries a Go line directive in a different
// physical shape: block form alone on a line, block form trailing on a
// field, multi-line block (the form that drives every fuzz crasher in
// fuzz/decoratefilereorder/crashers), and the column-1 //line form. All
// four must skip decoration since the directive remaps tf.Line() and
// silently invalidates the per-line guards every other check relies on.
func TestDecorateFile_MidStructLineDirectiveIsSkipped(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"block_form_alone", "package p\n\ntype S struct {\n\ta int\n\t/*line foo.go:42*/\n\tb int\n}\n"},
		{"block_form_trailing", "package p\n\ntype S struct {\n\ta int /*line foo.go:42*/\n\tb int\n}\n"},
		{"block_form_multiline", "package p\n\ntype S struct {\n\ta int\n\t/*line \n\t:42*/b int\n}\n"},
		{"line_form_column1", "package p\n\ntype S struct {\n\ta int\n//line foo.go:42\n\tb int\n}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset, f, _ := parseSource(t, tc.src)
			dec := NewDecorator(fset)
			df, err := dec.DecorateFile(f)
			if err != nil {
				t.Fatalf("DecorateFile: %v", err)
			}
			if len(df.structs) != 0 {
				t.Errorf("len(df.structs) = %d, want 0 (line directive mid-struct is non-decoratable)", len(df.structs))
			}
			var out bytes.Buffer
			if err := Fprint(&out, df); err != nil {
				t.Fatalf("Fprint: %v", err)
			}
			if out.String() != tc.src {
				t.Errorf("Fprint changed output for skipped struct:\ngot:\n%s\nwant:\n%s", out.String(), tc.src)
			}
		})
	}
}

// Narrowness check for the BUG-42 / BUG-43 guards. Each case looks like a
// directive but fails the recogniser the production code defers to: no
// word boundary after go:build / +build, missing space after //line, or
// no space before the colon in /*line:..*/. The guards must let these
// through so betteralign keeps reordering structs that carry incidental
// comments with directive-like prefixes.
func TestDecorateFile_NonPromotedDirectivesStillDecoratable(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"go_buildtag_glued", "package p\n\ntype S struct {\n\ta int //go:buildtag\n\tb int\n}\n"},
		{"plus_buildfoo_glued", "package p\n\ntype S struct {\n\ta int //+buildfoo\n\tb int\n}\n"},
		{"go_nosplit", "package p\n\ntype S struct {\n\ta int //go:nosplit\n\tb int\n}\n"},
		{"line_no_space", "package p\n\ntype S struct {\n\ta int //line\n\tb int\n}\n"},
		{"block_line_colon_no_space", "package p\n\ntype S struct {\n\ta int /*line:42*/\n\tb int\n}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset, f, _ := parseSource(t, tc.src)
			dec := NewDecorator(fset)
			df, err := dec.DecorateFile(f)
			if err != nil {
				t.Fatalf("DecorateFile: %v", err)
			}
			if len(df.structs) != 1 {
				t.Errorf("len(df.structs) = %d, want 1 (non-promoted directive must not block decoration)", len(df.structs))
			}
		})
	}
}

func TestDecorateFile_SingleLineStructIsSkipped(t *testing.T) {
	src := "package p\n\ntype S struct { b int32; a byte }\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	if len(df.structs) != 0 {
		t.Errorf("len(df.structs) = %d, want 0 (single-line struct is non-decoratable)", len(df.structs))
	}
	var astStruct *ast.StructType
	ast.Inspect(f, func(n ast.Node) bool {
		if s, ok := n.(*ast.StructType); ok {
			astStruct = s
			return false
		}
		return true
	})
	if astStruct == nil {
		t.Fatal("no *ast.StructType in fixture")
	}
	if _, ok := dec.Dst.Nodes[astStruct]; ok {
		t.Errorf("single-line struct should not be registered in Dst.Nodes")
	}
	var out bytes.Buffer
	if err := Fprint(&out, df); err != nil {
		t.Fatalf("Fprint: %v", err)
	}
	if out.String() != src {
		t.Errorf("Fprint changed output for clean file:\ngot:\n%s\nwant:\n%s", out.String(), src)
	}
}

// TestDecorateFile_IndentDetectedFromSpaces pins indent detection on non-tab whitespace.
func TestDecorateFile_IndentDetectedFromSpaces(t *testing.T) {
	src := "package p\n\ntype S struct {\n    a int\n    b int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	if len(df.structs) != 1 {
		t.Fatalf("len(df.structs) = %d, want 1", len(df.structs))
	}
	if got := df.structs[0].indent; got != "    " {
		t.Errorf("indent = %q, want %q (four spaces)", got, "    ")
	}
}

// TestDecorateFile_LeadDocIndentPreserved pins indent on lead-doc lines.
// Inspects the pre-gofmt buffer because gofmt re-indents and masks the bug.
func TestDecorateFile_LeadDocIndentPreserved(t *testing.T) {
	src := "package p\n\ntype S struct {\n\t// lead for b\n\ta int\n\tb int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	if len(df.structs) != 1 {
		t.Fatalf("len(df.structs) = %d, want 1", len(df.structs))
	}
	st := df.structs[0]
	st.Fields.List[0], st.Fields.List[1] = st.Fields.List[1], st.Fields.List[0]

	// Inspect raw spliced bytes; gofmt would rewrite indentation post-format.
	raw := spliceDirty(df, dirtyStructs(df))
	idx := bytes.Index(raw, []byte("// lead for b"))
	if idx < 0 {
		t.Fatalf("lead-doc comment dropped from raw splice:\n%s", string(raw))
	}
	if idx == 0 || string(raw[idx-len(st.indent):idx]) != st.indent {
		t.Errorf("lead-doc indent dropped; indent=%q, raw bytes around comment:\n%s",
			st.indent, string(raw[max(0, idx-8):min(len(raw), idx+24)]))
	}
}

func TestDecorateFile_CommentInRejectedInnerStructDoesNotLeakToOuter(t *testing.T) {
	// Inner is rejected (field on { line); its comment must not leak to Outer.
	src := "package p\n\ntype Outer struct {\n\tInner struct { a int\n\t\t// lead comment\n\t\tb int\n\t}\n\tc int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	if len(df.structs) != 1 {
		t.Fatalf("len(df.structs) = %d, want 1 (Inner is rejected)", len(df.structs))
	}
	outer := df.structs[0]
	for i, fld := range outer.Fields.List {
		if got := fld.lead; len(got) != 0 {
			t.Errorf("outer.Fields.List[%d].lead = %q, want empty (comment belongs to rejected Inner)", i, got)
		}
	}
}

// TestFprint consolidates output-level Fprint tests into a table-driven form.
// Each case parses a source string, optionally mutates the first struct's field
// list, calls Fprint, and compares the full output to want.
// When want is empty and mutate is nil the identity invariant is checked
// (out == code).
func TestFprint(t *testing.T) {
	tests := []struct {
		skip, solo bool
		name       string
		code       string
		mutate     func(df *File)
		want       string
	}{
		{
			name: "identity returns input",
			code: "package p\n\ntype S struct {\n\ta int\n\tb int\n}\n",
		},
		{
			name: "reorder two fields",
			code: "package p\n\ntype S struct { // want \"struct of size 12 could be 8\"\n\tx byte\n\ty int32\n\tz byte\n}\n",
			mutate: func(df *File) {
				// Reorder: y, x, z (= original indexes 1, 0, 2).
				st := df.structs[0]
				st.Fields.List = []*Field{st.Fields.List[1], st.Fields.List[0], st.Fields.List[2]}
			},
			want: "package p\n\ntype S struct { // want \"struct of size 12 could be 8\"\n\ty int32\n\tx byte\n\tz byte\n}\n",
		},
		{
			name: "struct tags survive reorder",
			code: "package p\n\ntype S struct {\n\tX byte  `json:\"x\"`\n\tY int32 `json:\"y\"`\n}\n",
			mutate: func(df *File) {
				st := df.structs[0]
				st.Fields.List[0], st.Fields.List[1] = st.Fields.List[1], st.Fields.List[0]
			},
			want: "package p\n\ntype S struct {\n\tY int32 `json:\"y\"`\n\tX byte  `json:\"x\"`\n}\n",
		},
		{
			// Multi-name group is one *Field; swap must keep names together.
			name: "multi-name field stays grouped under reorder",
			code: "package p\n\ntype S struct {\n\tA, B int\n\tc    string\n}\n",
			mutate: func(df *File) {
				st := df.structs[0]
				st.Fields.List[0], st.Fields.List[1] = st.Fields.List[1], st.Fields.List[0]
			},
			want: "package p\n\ntype S struct {\n\tc    string\n\tA, B int\n}\n",
		},
		{
			// Embedded field has empty Names; spans only the type identifier.
			name: "embedded field survives reorder",
			code: "package p\n\ntype T int\n\ntype S struct {\n\tT\n\ta int\n}\n",
			mutate: func(df *File) {
				st := df.structs[0]
				st.Fields.List[0], st.Fields.List[1] = st.Fields.List[1], st.Fields.List[0]
			},
			want: "package p\n\ntype T int\n\ntype S struct {\n\ta int\n\tT\n}\n",
		},
		{
			name: "preserves lead and trailing line comment",
			code: "package p\n\ntype S struct {\n\t// lead for x\n\tx byte // trailing x\n\ty int32 // trailing y\n}\n",
			mutate: func(df *File) {
				st := df.structs[0]
				st.Fields.List = []*Field{st.Fields.List[1], st.Fields.List[0]}
			},
			want: "package p\n\ntype S struct {\n\ty int32 // trailing y\n\t// lead for x\n\tx byte // trailing x\n}\n",
		},
		{
			name: "nested dirty outer reorder applied",
			code: "package p\n\ntype Outer struct {\n\tInner struct {\n\t\ty int32\n\t\tx byte\n\t}\n\tb bool\n}\n",
			mutate: func(df *File) {
				// filterOutermost drops the inner reorder; the outer reorder must apply.
				outer, inner := df.structs[0], df.structs[1]
				outer.Fields.List = []*Field{outer.Fields.List[1], outer.Fields.List[0]}
				inner.Fields.List = []*Field{inner.Fields.List[1], inner.Fields.List[0]}
			},
			want: "package p\n\ntype Outer struct {\n\tb     bool\n\tInner struct {\n\t\ty int32\n\t\tx byte\n\t}\n}\n",
		},
		{
			name: "blank line attaches to both prev and next field",
			// Dual-attachment of inter-field blanks; gofmt coalesces duplicates.
			code: "package p\n\n" +
				"type S struct {\n" +
				"\tA byte\n\n" +
				"\tB int64\n\n" +
				"\tC byte\n" +
				"}\n",
			mutate: func(df *File) {
				// Move B (index 1) to first; keep A and C in their order.
				st := df.structs[0]
				st.Fields.List = []*Field{st.Fields.List[1], st.Fields.List[0], st.Fields.List[2]}
			},
			want: "package p\n\n" +
				"type S struct {\n" +
				"\tB int64\n\n" +
				"\tA byte\n\n" +
				"\tC byte\n" +
				"}\n",
		},
		{
			name: "group separator blanks travel with following field",
			// Group-separator blanks must travel with the field they precede.
			code: "package p\n" +
				"type T interface{ x() }\n\n" +
				"type S struct {\n\n" +
				"\t// gf\n" +
				"\tGetFetch func() string\n\n" +
				"\t// lvn\n" +
				"\tLogVarName string\n\n" +
				"\t// lk\n" +
				"\tLogKey string\n" +
				"\t// ssp\n" +
				"\tShouldBeSetInScp T\n" +
				"}\n",
			mutate: func(df *File) {
				// Move ShouldBeSetInScp (last field, index 3) to the front.
				st := df.structs[0]
				st.Fields.List = []*Field{st.Fields.List[3], st.Fields.List[0], st.Fields.List[1], st.Fields.List[2]}
			},
			want: "package p\n\n" +
				"type T interface{ x() }\n\n" +
				"type S struct {\n" +
				"\t// ssp\n" +
				"\tShouldBeSetInScp T\n\n" +
				"\t// gf\n" +
				"\tGetFetch func() string\n\n" +
				"\t// lvn\n" +
				"\tLogVarName string\n\n" +
				"\t// lk\n" +
				"\tLogKey string\n" +
				"}\n",
		},
		{
			name: "blank line between fields preserved on reorder",
			// Inter-field blanks were lost when no field's span claimed them.
			code: "package p\n\ntype S struct {\n\ta int32\n\n\tb byte\n}\n",
			mutate: func(df *File) {
				st := df.structs[0]
				st.Fields.List[0], st.Fields.List[1] = st.Fields.List[1], st.Fields.List[0]
			},
			want: "package p\n\ntype S struct {\n\tb byte\n\ta int32\n}\n",
		},
		{
			name: "blank after opening brace preserved on reorder",
			// Blanks after { travel with their following field on reorder.
			code: "package p\n\ntype S struct {\n\n\ta int32\n\tb byte\n}\n",
			mutate: func(df *File) {
				st := df.structs[0]
				st.Fields.List[0], st.Fields.List[1] = st.Fields.List[1], st.Fields.List[0]
			},
			want: "package p\n\ntype S struct {\n\tb byte\n\n\ta int32\n}\n",
		},
		{
			name: "blank before closing brace preserved on reorder",
			// Blank before } stays with the last field's owned range on reorder.
			code: "package p\n\ntype S struct {\n\ta int32\n\tb byte\n\n}\n",
			mutate: func(df *File) {
				st := df.structs[0]
				st.Fields.List[0], st.Fields.List[1] = st.Fields.List[1], st.Fields.List[0]
			},
			want: "package p\n\ntype S struct {\n\tb byte\n\n\ta int32\n}\n",
		},
	}

	var solo bool
	for _, tc := range tests {
		if tc.solo {
			solo = true
		}
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if solo && !tc.solo {
				t.Skip()
			}
			if tc.skip {
				t.Skip()
			}

			fset, f, _ := parseSource(t, tc.code)
			dec := NewDecorator(fset)
			df, err := dec.DecorateFile(f)
			if err != nil {
				t.Fatalf("DecorateFile: %v", err)
			}

			if tc.mutate != nil {
				tc.mutate(df)
			}

			var out bytes.Buffer
			if err := Fprint(&out, df); err != nil {
				t.Fatalf("Fprint: %v", err)
			}

			expect := tc.want
			if expect == "" {
				// Identity case: no mutation, no explicit want — output must equal input.
				expect = tc.code
			}
			compare(t, expect, out.String())
		})
	}
}

func TestFprint_GofmtCorruptedOutputIsRejected(t *testing.T) {
	// Form-feed in an import path: gofmt emits invalid Go silently; re-parse catches it.
	src := "package A\nimport\"00000000\f\"\n//\ntype A struct{\nA\nA\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	if len(df.structs) != 1 {
		t.Fatalf("len(df.structs) = %d, want 1", len(df.structs))
	}
	st := df.structs[0]
	if len(st.Fields.List) < 2 {
		t.Fatalf("want >=2 fields, got %d", len(st.Fields.List))
	}
	st.Fields.List[0], st.Fields.List[1] = st.Fields.List[1], st.Fields.List[0]
	var out bytes.Buffer
	err = Fprint(&out, df)
	if err == nil {
		t.Fatalf("Fprint returned nil error; expected ErrFormat. Output:\n%s", out.String())
	}
	if !errors.Is(err, ErrFormat) {
		t.Errorf("Fprint err = %v; want errors.Is(err, ErrFormat)", err)
	}
}

// reverseFirstStructInto reprints f with its first >=2-field struct reversed,
// mirroring the reorder the analyzer applies under -fix.
func reverseFirstStructInto(t *testing.T, dec *Decorator, f *ast.File) string {
	t.Helper()
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	var target *StructType
	for _, st := range df.structs {
		if len(st.Fields.List) >= 2 {
			target = st
			break
		}
	}
	if target == nil {
		t.Fatal("no decoratable struct with >=2 fields")
	}
	slices.Reverse(target.Fields.List)
	var buf bytes.Buffer
	if err := Fprint(&buf, df); err != nil {
		t.Fatalf("Fprint: %v", err)
	}
	return buf.String()
}

// TestDecorateFile_ReprintUsesParsedNotDiskBytes is the regression for issue #36:
// two passes (the base + [p.test] variants) reverse the same parsed struct, but
// the first writes its result to disk before the second decorates. Both must
// reprint identical valid Go — the second has to splice against its parsed bytes,
// not the first pass's rewrite, or fields duplicate/drop.
func TestDecorateFile_ReprintUsesParsedNotDiskBytes(t *testing.T) {
	src := `package p

type (
	// Handler is a callback.
	Handler func(string) error

	// Scanner scans things.
	Scanner struct {
		// enabled toggles scanning.
		enabled bool
		// cache holds results.
		cache map[string]int
		// name identifies the scanner.
		name string
		// logger writes logs.
		//
		// It is multi-line on purpose.
		logger *int
		// count tracks totals.
		count int64
		// pool is a worker pool.
		pool *int
	}
)
`
	// Both variants parse the pristine file before any rewrite, as go/packages does.
	fset, f1, path := parseSource(t, src)
	f2, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}

	// Pass 1: reorder and write to disk, as -apply does.
	out1 := reverseFirstStructInto(t, NewDecorator(fset), f1)
	if err := os.WriteFile(path, []byte(out1), 0o644); err != nil {
		t.Fatalf("apply pass 1: %v", err)
	}

	// Pass 2: same reorder of the same parsed struct — must match pass 1.
	out2 := reverseFirstStructInto(t, NewDecorator(fset), f2)

	if out1 != out2 {
		t.Errorf("second pass reprint diverged after disk rewrite (issue #36)\n=== PASS 1 ===\n%s\n=== PASS 2 ===\n%s", out1, out2)
	}

	// Corruption also shows as dup/dropped fields: output must parse with each name once.
	outFile, err := parser.ParseFile(token.NewFileSet(), "out.go", []byte(out2), parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("pass 2 produced invalid Go (issue #36):\n%s\nparse error: %v", out2, err)
	}
	counts := map[string]int{}
	ast.Inspect(outFile, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok {
			return true
		}
		for _, fld := range st.Fields.List {
			for _, nm := range fld.Names {
				counts[nm.Name]++
			}
		}
		return true
	})
	for _, name := range []string{"enabled", "cache", "name", "logger", "count", "pool"} {
		if counts[name] != 1 {
			t.Errorf("field %q appears %d times, want 1 (issue #36 dup/drop)\n%s", name, counts[name], out2)
		}
	}
}
