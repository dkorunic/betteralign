// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package betteralign

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/analysis"
)

func TestGreenTeaGC(t *testing.T) {
	cases := map[string]bool{
		"":          false, // unknown version: treat as pre-1.26
		"go1.25":    false,
		"go1.26":    true, // Green Tea GC became the default here
		"go1.26rc1": true,
		"go1.27":    true,
		"go2.0":     true,
		"garbage":   false, // invalid: must not be read as >= go1.26
		"1.26":      false, // missing "go" prefix is invalid
	}
	for v, want := range cases {
		if got := greenTeaGC(v); got != want {
			t.Errorf("greenTeaGC(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestTargetGoVersion(t *testing.T) {
	// A per-file //go:build go1.x constraint wins over the module version.
	pass := &analysis.Pass{}
	file := &ast.File{GoVersion: "go1.26"}
	if got := targetGoVersion(pass, file); got != "go1.26" {
		t.Errorf("file version precedence: got %q, want %q", got, "go1.26")
	}

	// With a module version, the newer of the two wins: an older constraint
	// is a minimum the module's own version already exceeds.
	fset := token.NewFileSet()
	pf, err := parser.ParseFile(fset, "p.go", "package p\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := (&types.Config{GoVersion: "go1.26"}).Check("example.com/p", fset, []*ast.File{pf}, nil)
	if err != nil {
		t.Fatal(err)
	}
	modPass := &analysis.Pass{Pkg: pkg}
	for file, want := range map[string]string{"go1.20": "go1.26", "go1.27": "go1.27", "": "go1.26"} {
		if got := targetGoVersion(modPass, &ast.File{GoVersion: file}); got != want {
			t.Errorf("file %q in a go1.26 module: got %q, want %q", file, got, want)
		}
	}
	if got := targetGoVersion(&analysis.Pass{Pkg: types.NewPackage("p", "p")}, &ast.File{GoVersion: "go1.20"}); got != "go1.20" {
		t.Errorf("file go1.20, no module version: got %q, want go1.20", got)
	}

	// Nil file and nil Pkg must not panic and report unknown.
	if got := targetGoVersion(pass, nil); got != "" {
		t.Errorf("unknown version: got %q, want empty", got)
	}
}
