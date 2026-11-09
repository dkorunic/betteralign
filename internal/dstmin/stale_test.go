// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package dstmin

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDecorateFileRejectsStaleSource is the regression for decorating against
// bytes that no longer match the parsed AST — the file was edited (an editor
// save, a git checkout) between the driver's parse and DecorateFile's read.
// The AST offsets then index the wrong bytes: a shorter file panicked with an
// out-of-range slice, crashing the whole analysis run, and a same-length edit
// could splice garbage into a SuggestedFix or an -apply write. Both must be
// refused as ErrSourceRead instead.
func TestDecorateFileRejectsStaleSource(t *testing.T) {
	const orig = "package p\n\ntype S struct {\n\ta bool\n\tb int64\n\tc bool\n}\n"
	for name, edited := range map[string]string{
		"shrunk": "package p\n",
		"grown":  orig + "\nvar x = 1\n",
		// Two fields swapped: same length, every offset after the first field
		// now points at different bytes.
		"same length": "package p\n\ntype S struct {\n\tb int64\n\ta bool\n\tc bool\n}\n",
		// Every identifier in place, only the closing brace moved.
		"brace moved": "package p\n\ntype S struct {\n\ta bool\n\tb int64\n\tc bool\n\n}",
	} {
		t.Run(name, func(t *testing.T) {
			if name == "same length" && len(edited) != len(orig) {
				t.Fatalf("fixture lengths differ: %d vs %d", len(edited), len(orig))
			}
			path := filepath.Join(t.TempDir(), "p.go")
			if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
				t.Fatal(err)
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err = NewDecorator(fset).DecorateFile(f)
			if !errors.Is(err, ErrSourceRead) {
				t.Fatalf("DecorateFile on a file changed since parsing: err = %v, want ErrSourceRead", err)
			}
		})
	}
}

// TestDecorateFileRejectsStaleTag pins the literal half of the check: a host
// that parsed an unsaved buffer (gopls) holds a same-length tag edit the disk
// lacks, and a fix cut from the disk bytes reverted it. Identifiers and
// braces all match; only the *ast.BasicLit differs.
func TestDecorateFileRejectsStaleTag(t *testing.T) {
	const disk = "package p\n\ntype S struct {\n\ta bool `json:\"a\"`\n\tb int64\n\tc bool\n}\n"
	path := filepath.Join(t.TempDir(), "p.go")
	if err := os.WriteFile(path, []byte(disk), 0o644); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, strings.Replace(disk, `json:"a"`, `json:"b"`, 1), parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewDecorator(fset).DecorateFile(f); !errors.Is(err, ErrSourceRead) {
		t.Fatalf("DecorateFile with a tag the disk doesn't hold: err = %v, want ErrSourceRead", err)
	}
}

// TestDecorateFileRejectsStaleComment pins the comment half of the check: a
// lead doc is re-emitted from the AST's text, so a same-length edit to it
// was reverted, and one turning it into code (an embedded field) deleted the
// new field from the rewrite. A file with a carriage return anywhere is
// refused before the check (ErrCarriageReturn): its comment text drops them.
func TestDecorateFileRejectsStaleComment(t *testing.T) {
	const orig = "package p\n\ntype S struct {\n\tA bool\n\t// one two\n\tB int64\n\tC bool\n}\n"
	for name, edited := range map[string]string{
		"comment edited":       strings.Replace(orig, "one two", "new txt", 1),
		"comment becomes code": strings.Replace(orig, "one two", "one\ntwo", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if len(edited) != len(orig) {
				t.Fatalf("fixture lengths differ: %d vs %d", len(edited), len(orig))
			}
			path := filepath.Join(t.TempDir(), "p.go")
			if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
				t.Fatal(err)
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := NewDecorator(fset).DecorateFile(f); !errors.Is(err, ErrSourceRead) {
				t.Fatalf("err = %v, want ErrSourceRead", err)
			}
		})
	}
	for name, src := range map[string]string{
		// CRLF outside an LF struct: gofmt would normalise the other lines.
		"mixed": strings.Replace(orig, "\n", "\r\n", 1),
		"crlf":  strings.ReplaceAll(orig, "\n", "\r\n"),
	} {
		path := filepath.Join(t.TempDir(), "p.go")
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewDecorator(fset).DecorateFile(f); !errors.Is(err, ErrCarriageReturn) {
			t.Errorf("%s: err = %v, want ErrCarriageReturn", name, err)
		}
	}
}

// TestSourceMatchesASTLatchesMismatch pins that one mismatch fails the whole
// check even when later nodes match: ast.Inspect keeps visiting siblings after
// a false return, so with a non-latching flag the sibling identifier c in
// `var b, c, d` would reset it after a stale b and pass stale source.
func TestSourceMatchesASTLatchesMismatch(t *testing.T) {
	const src = "package p\n\nvar a = 1\n\nvar b, c, d = 2, 3, 4\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	tf := fset.File(f.Pos())
	if !sourceMatchesAST(f, tf, []byte(src)) {
		t.Fatal("identical source reported as stale")
	}
	// Rename b only; its siblings c and d (and everything after) still match.
	stale := []byte(src)
	stale[len("package p\n\nvar a = 1\n\nvar ")] = 'z'
	if sourceMatchesAST(f, tf, stale) {
		t.Error("an early mismatch followed by matching identifiers was accepted")
	}
}

// TestDecorateFileRereadsForNewParse is the regression for the process-wide
// source cache outliving its parse. A long-running host (an editor
// integration, a daemon embedding the analyzer) re-parses a file after each
// edit; with the cache keyed by path alone it spliced the first-ever bytes,
// and a same-length edit (a tag, a comment, an array length) passes
// sourceMatchesAST, so the reorder silently reverted the user's edit.
func TestDecorateFileRereadsForNewParse(t *testing.T) {
	const orig = "package p\n\ntype S struct {\n\ta bool // old\n\tb int64\n}\n"
	_, _, path := parseSource(t, orig)
	edited := strings.Replace(orig, "old", "new", 1)
	fset := token.NewFileSet()
	first, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewDecorator(fset).DecorateFile(first); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	out := reverseFirstStructInto(t, NewDecorator(fset), second)
	if !strings.Contains(out, "// new") || strings.Contains(out, "// old") {
		t.Errorf("reprint of the new parse used stale bytes:\n%s", out)
	}
}

// TestDecorateFileNoTrailingNewline pins the bound of sourceMatchesAST: a
// file whose very last byte is a struct's closing brace still matches.
func TestDecorateFileNoTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.go")
	if err := os.WriteFile(path, []byte("package p\n\ntype S struct {\n\ta bool\n}"), 0o644); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewDecorator(fset).DecorateFile(f); err != nil {
		t.Fatalf("DecorateFile: %v", err)
	}
}
