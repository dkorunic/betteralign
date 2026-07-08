// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package betteralign

import (
	"go/ast"
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

	// Nil file and nil Pkg must not panic and report unknown.
	if got := targetGoVersion(pass, nil); got != "" {
		t.Errorf("unknown version: got %q, want empty", got)
	}
}
