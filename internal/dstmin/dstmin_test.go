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
	if df == nil {
		t.Fatal("DecorateFile returned nil *File")
	}
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
	if got := st.Fields.List[0].lead.All(); len(got) != 1 || got[0] != "// lead for a" {
		t.Errorf("field 0 lead = %q, want [%q]", got, "// lead for a")
	}
	if got := st.Fields.List[1].lead.All(); len(got) != 0 {
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
	got := st.Fields.List[0].lead.All()
	want := []string{"// line one", "// line two"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("field a lead = %q, want %q", got, want)
	}
	if l := st.Fields.List[1].lead.All(); len(l) != 0 {
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
	if got := st.Fields.List[0].lead.All(); len(got) != 1 || got[0] != "/* lead */" {
		t.Errorf("field a lead = %q, want [%q]", got, "/* lead */")
	}
	if l := st.Fields.List[1].lead.All(); len(l) != 0 {
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
	if l := st.Fields.List[1].lead.All(); len(l) != 0 {
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

func TestDecorateFile_OpeningBraceComment(t *testing.T) {
	src := "package p\n\ntype S struct { // betteralign:ignore\n\ta int\n\tb int\n}\n"
	fset, f, _ := parseSource(t, src)
	dec := NewDecorator(fset)
	df, err := dec.DecorateFile(f)
	if err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
	st := df.structs[0]
	got := st.Fields.Decs.Opening.All()
	if len(got) != 1 || got[0] != "// betteralign:ignore" {
		t.Errorf("Opening = %q, want [%q]", got, "// betteralign:ignore")
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
	if got := outer.Fields.Decs.Opening.All(); len(got) != 0 {
		t.Errorf("outer.Opening = %q, want empty (comment belongs to Inner.a)", got)
	}
	if got := inner.Fields.List[0].lead.All(); len(got) != 1 || got[0] != "// lead for a" {
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
	if got := st.Fields.List[0].lead.All(); len(got) != 0 {
		t.Errorf("field a lead = %q, want empty (inline comment, not lead-doc)", got)
	}
	if got := st.Fields.Decs.Opening.All(); len(got) != 0 {
		t.Errorf("Opening = %q, want empty", got)
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
	if got := st.Fields.List[1].lead.All(); len(got) != 0 {
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
		if got := fld.lead.All(); len(got) != 0 {
			t.Errorf("outer.Fields.List[%d].lead = %q, want empty (comment belongs to rejected Inner)", i, got)
		}
	}
	if got := outer.Fields.Decs.Opening.All(); len(got) != 0 {
		t.Errorf("outer.Opening = %q, want empty", got)
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
