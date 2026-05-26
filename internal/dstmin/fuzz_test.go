// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package dstmin

import (
	"bytes"
	"errors"
	"go/parser"
	"go/scanner"
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
		// Find the first struct with >=2 fields. Skip if none.
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

		// Capture pre-mutation field bytes; each must survive in the reordered output.
		origFields := make([]string, len(target.Fields.List))
		for i, fld := range target.Fields.List {
			body := string(df.source[fld.bodyStart:fld.bodyEnd])
			if fld.trailEnd > fld.trailStart {
				body += string(df.source[fld.trailStart:fld.trailEnd])
			}
			origFields[i] = body
		}

		target.Fields.List[0], target.Fields.List[1] = target.Fields.List[1], target.Fields.List[0]

		var buf bytes.Buffer
		if err := Fprint(&buf, df); err != nil {
			// ErrFormat is the safety-net path; not a fuzzer-flagged bug.
			if errors.Is(err, ErrFormat) {
				return
			}
			t.Fatalf("Fprint (reorder): %v", err)
		}
		// The output MUST be valid Go.
		if _, err := parser.ParseFile(token.NewFileSet(), "out.go", buf.Bytes(), parser.ParseComments|parser.SkipObjectResolution); err != nil {
			t.Errorf("reorder produced invalid Go:\n=== OUTPUT ===\n%s\n=== PARSE ERROR ===\n%v\n=== INPUT ===\n%s", buf.String(), err, src)
		}
		// Token-level match tolerates gofmt whitespace normalizations.
		outToks := goTokens(buf.Bytes())
		for i, body := range origFields {
			needle := goTokens([]byte(body))
			if len(needle) == 0 {
				continue
			}
			if !containsTokenSeq(outToks, needle) {
				t.Errorf("field %d token sequence missing from reordered output\nNEEDLE: %v\nOUTPUT:\n%s\nINPUT:\n%s", i, needle, buf.String(), src)
			}
		}
	})
}

// goTokens tokenizes src, skipping comments and semicolons (gofmt rewrites
// both independently of dstmin).
func goTokens(src []byte) []string {
	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", -1, len(src))
	s.Init(file, src, nil, 0) // don't ScanComments
	var toks []string
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return toks
		}
		if tok == token.SEMICOLON {
			continue
		}
		if lit != "" {
			toks = append(toks, lit)
			continue
		}
		toks = append(toks, tok.String())
	}
}

func containsTokenSeq(hay, needle []string) bool {
	if len(needle) > len(hay) {
		return false
	}
	for i := 0; i <= len(hay)-len(needle); i++ {
		match := true
		for j, n := range needle {
			if hay[i+j] != n {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
