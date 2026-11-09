// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package betteralign_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
	"gotest.tools/v3/golden"
)

// TestCrossPackageAtomicHolder pins holders of another package's atomically
// accessed struct: dep's own methods pass &c.v to a 64-bit sync/atomic
// function, out of user's sight, so reordering user's holder put the word at
// offset 4 on 386 and panicked there (go.uber.org/atomic.Int64, os.file's
// poll.FD). Found in dep's source by field name, also one hop through a
// local pointer (cache := &d.recent[k]; &cache[i]); a holder of a struct dep
// never accesses atomically is still reordered (the golden).
func TestCrossPackageAtomicHolder(t *testing.T) {
	skipUnless64Bit(t)
	analysistest.Run(t, analysistest.TestData(), NewTestAnalyzer(), "xatomic/user")
	tmpDir := applyBuilds(t, "xatomic", nil)
	got := strings.ReplaceAll(readGoFiles(t, filepath.Join(tmpDir, "xatomic", "user"))["user.go"], filepath.Base(tmpDir)+"/", "")
	golden.Assert(t, got, filepath.Join("src", "xatomic", "user", "user.go.golden"))
}

// TestExtTestPositional pins structs built positionally in the external test
// package (package p_test in p's directory, renamed import, elided slice
// element). It loads as another package, so no pass of p sees the literal;
// read from disk, both p and its test variant pin alike. The keyed control is
// reordered (the golden).
func TestExtTestPositional(t *testing.T) {
	skipUnless64Bit(t)
	analysistest.RunWithSuggestedFixes(t, analysistest.TestData(), NewTestAnalyzer(), "exttest")
}

// TestExtTestPositionalBinary covers what analysistest can't load: a file
// argument (command-line-arguments, which no external test imports: the
// literal's import was matched against that path and missed) and an external
// test that doesn't parse (its literal was dropped with the whole file). In
// both, T is built positionally by p_test and must stay.
func TestExtTestPositionalBinary(t *testing.T) {
	skipUnless64Bit(t)
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin, _ := xconvModule(t)
	const p = "package p\n\ntype T struct {\n\tA int8\n\tB int64\n\tC int8\n}\n"
	const ext = "package p_test\n\nimport \"example.com/ext/p\"\n\nvar _ = p.T{1, 2, 3}\n"
	for _, tc := range []struct{ name, ext, arg string }{
		{"file argument", ext, "p/p.go"},
		{"syntax error", ext + "\nfunc broken() {\n\tx :=\n}\n", "./..."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mod := t.TempDir()
			writeTree(t, mod, map[string]string{
				"go.mod":          "module example.com/ext\n\ngo 1.24\n",
				"p/p.go":          p,
				"p/p_ext_test.go": tc.ext,
			})
			cmd := exec.Command(bin, "-apply", tc.arg)
			cmd.Dir = mod
			out, _ := cmd.CombinedOutput() // exits non-zero when it reports
			if !strings.Contains(string(out), "reorder skipped: positional composite literal at") {
				t.Errorf("T not pinned by p_test's literal:\n%s", out)
			}
			if got, err := os.ReadFile(filepath.Join(mod, "p", "p.go")); err != nil || string(got) != p {
				t.Errorf("p.go rewritten (err %v):\n%s\ntool output:\n%s", err, got, out)
			}
		})
	}
}

// TestIndirectPins pins one hop of indirection between a struct and the call
// depending on its layout: a variable defined once (not one reassigned), a
// pointer defined once into an array or struct field as the base of an
// atomic word's address (cache := &d.recent[k]; &cache[i], not into a
// slice's elements), a function forwarding its parameter to a 64-bit atomic, a codec or
// unsafe.Pointer (generic ones by their instance), pointer arithmetic from a
// field's address, and a reflect field read by constant index. A forwarder
// that only reads its parameter and FieldByName pin nothing (the golden).
func TestIndirectPins(t *testing.T) {
	skipUnless64Bit(t)
	results := analysistest.RunWithSuggestedFixes(t, analysistest.TestData(), NewTestAnalyzer(), "indirect")
	assertRawFixesMatchGolden(t, results, "indirect")
}
