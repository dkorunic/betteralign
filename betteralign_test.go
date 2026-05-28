// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package betteralign_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dkorunic/betteralign"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"
	"gotest.tools/v3/golden"
)

func removeOtherArches(paths []string) []string {
	var filtered []string
	arches := map[string]struct{}{
		"386":      {},
		"amd64":    {},
		"arm":      {},
		"arm64":    {},
		"loong64":  {},
		"mips":     {},
		"mipsle":   {},
		"mips64":   {},
		"mips64le": {},
		"ppc64":    {},
		"ppc64le":  {},
		"riscv64":  {},
		"s390x":    {},
		"wasm":     {},
	}

	delete(arches, runtime.GOARCH)

	suffixes := make([]string, 0, len(arches))
	for arch := range arches {
		suffixes = append(suffixes, "_"+arch+".go")
	}

	var blacklist bool
	for _, path := range paths {
		blacklist = false

		for _, suffix := range suffixes {
			if strings.Contains(path, suffix) {
				blacklist = true
				break
			}
		}

		if !blacklist {
			filtered = append(filtered, path)
		}
	}

	return filtered
}

func NewTestAnalyzer() *analysis.Analyzer {
	analyzer := &analysis.Analyzer{
		Name:     betteralign.Analyzer.Name,
		Doc:      betteralign.Analyzer.Doc,
		Requires: betteralign.Analyzer.Requires,
	}
	betteralign.InitAnalyzer(analyzer)
	return analyzer
}

func TestSuggestions(t *testing.T) {
	testdata := analysistest.TestData()
	analyzer := NewTestAnalyzer()
	analysistest.Run(t, testdata, analyzer, "a")
}

func TestApply(t *testing.T) {
	srcDir := filepath.Join("testdata", "src")
	workDir := filepath.Join(srcDir, "a")

	tmpDir, err := os.MkdirTemp(srcDir, "apply-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	tmpWorkDir := filepath.Join(tmpDir, "a")

	if err := os.Mkdir(tmpWorkDir, 0o750); err != nil {
		t.Fatal(err)
	}

	paths, err := filepath.Glob(filepath.Join(workDir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}

	paths = removeOtherArches(paths)

	for _, path := range paths {
		testBasename := filepath.Base(path)
		testTmpname := filepath.Join(tmpWorkDir, testBasename)

		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(testTmpname, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	testdata := analysistest.TestData()

	analyzer := NewTestAnalyzer()
	_ = analyzer.Flags.Set("apply", "true")

	analysistest.Run(t, testdata, analyzer, filepath.Join(filepath.Base(tmpDir), "a"))

	for _, path := range paths {
		testBasename := filepath.Base(path)
		testTmpname := filepath.Join(tmpWorkDir, testBasename)

		testResult, err := os.ReadFile(testTmpname)
		if err != nil {
			t.Fatal(err)
		}

		goldenFilename := filepath.Join("src", "a", strings.Join([]string{testBasename, ".golden"}, ""))
		golden.Assert(t, string(testResult), goldenFilename)
	}
}

// TestApplyViaFixAlias pins `-fix` propagating into `apply` (host-registered flag → analyzer).
func TestApplyViaFixAlias(t *testing.T) {
	srcDir := filepath.Join("testdata", "src")
	workDir := filepath.Join(srcDir, "a")

	tmpDir, err := os.MkdirTemp(srcDir, "fix-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	tmpWorkDir := filepath.Join(tmpDir, "a")
	if err := os.Mkdir(tmpWorkDir, 0o750); err != nil {
		t.Fatal(err)
	}

	paths, err := filepath.Glob(filepath.Join(workDir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	paths = removeOtherArches(paths)

	for _, path := range paths {
		testBasename := filepath.Base(path)
		testTmpname := filepath.Join(tmpWorkDir, testBasename)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(testTmpname, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	testdata := analysistest.TestData()

	analyzer := NewTestAnalyzer()
	// singlechecker/multichecker would register -fix; without a real host, register it ourselves.
	var fix bool
	analyzer.Flags.BoolVar(&fix, "fix", false, "alias for -apply (test harness only)")
	_ = analyzer.Flags.Set("fix", "true")

	analysistest.Run(t, testdata, analyzer, filepath.Join(filepath.Base(tmpDir), "a"))

	for _, path := range paths {
		testBasename := filepath.Base(path)
		testTmpname := filepath.Join(tmpWorkDir, testBasename)

		testResult, err := os.ReadFile(testTmpname)
		if err != nil {
			t.Fatal(err)
		}

		goldenFilename := filepath.Join("src", "a", strings.Join([]string{testBasename, ".golden"}, ""))
		golden.Assert(t, string(testResult), goldenFilename)
	}
}

func TestFlagExcludeDirs(t *testing.T) {
	t.Run("exclude none", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		_ = analyzer.Flags.Set("apply", "false")
		analysistest.Run(t, testdata, analyzer, "exclude/none/...")
	})

	t.Run("exclude all", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		_ = analyzer.Flags.Set("apply", "false")
		_ = analyzer.Flags.Set("exclude_dirs", "testdata/src/exclude/all/")
		analysistest.Run(t, testdata, analyzer, "exclude/all/...")
	})

	t.Run("exclude a", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		_ = analyzer.Flags.Set("apply", "false")
		_ = analyzer.Flags.Set("exclude_dirs", "testdata/src/exclude/a/a")
		analysistest.Run(t, testdata, analyzer, "exclude/a/...")
	})

	// Absolute paths used to fail closed via filepath.Rel; now normalized to wd-relative.
	t.Run("exclude a via absolute path", func(t *testing.T) {
		wd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		_ = analyzer.Flags.Set("apply", "false")
		_ = analyzer.Flags.Set("exclude_dirs", filepath.Join(wd, "testdata", "src", "exclude", "a", "a"))
		analysistest.Run(t, testdata, analyzer, "exclude/a/...")
	})

	// Regression for #34: glob patterns were silently ignored.
	t.Run("exclude a via glob pattern", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		_ = analyzer.Flags.Set("apply", "false")
		_ = analyzer.Flags.Set("exclude_dirs", "testdata/src/exclude/*/a")
		analysistest.Run(t, testdata, analyzer, "exclude/a/...")
	})
}

func TestFlagExcludeFiles(t *testing.T) {
	t.Run("exclude b", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		_ = analyzer.Flags.Set("apply", "false")
		_ = analyzer.Flags.Set("exclude_files", "testdata/src/exclude/b/b/*.go")
		analysistest.Run(t, testdata, analyzer, "exclude/b/...")
	})
}

func TestFlagOptInMode(t *testing.T) {
	t.Run("opt-in enabled, one bad struct opted in and another bad struct not opted in", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		_ = analyzer.Flags.Set("apply", "false")
		_ = analyzer.Flags.Set("opt_in", "true")
		analysistest.Run(t, testdata, analyzer, "optin/...")
	})

	t.Run("per-spec opt-in comment inside a grouped type declaration", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		_ = analyzer.Flags.Set("apply", "false")
		_ = analyzer.Flags.Set("opt_in", "true")
		analysistest.Run(t, testdata, analyzer, "optin/grouped")
	})
}

func TestFlagTestFiles(t *testing.T) {
	t.Run("test files excluded by default", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		analysistest.Run(t, testdata, analyzer, "testfiles/a")
	})

	t.Run("test files included with flag", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		_ = analyzer.Flags.Set("test_files", "true")
		analysistest.Run(t, testdata, analyzer, "testfiles/b")
	})
}

// TestCurrentSkipResetsPerFile pins the per-file `currentSkip = false` reset:
// a skipped file (generated header) must not leak its skip onto later files.
func TestCurrentSkipResetsPerFile(t *testing.T) {
	testdata := analysistest.TestData()
	analyzer := NewTestAnalyzer()
	analysistest.Run(t, testdata, analyzer, "skipcarry")
}

func TestFlagGeneratedFiles(t *testing.T) {
	t.Run("generated files excluded by default", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		analysistest.Run(t, testdata, analyzer, "generated/a")
	})

	t.Run("generated files included with flag", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		_ = analyzer.Flags.Set("generated_files", "true")
		analysistest.Run(t, testdata, analyzer, "generated/b")
	})

	// Subtests above cover the comment-based path; these cover the filename-suffix path.
	t.Run("generated suffix _gen.go excluded by default", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		analysistest.Run(t, testdata, analyzer, "generated_suffix/a")
	})

	t.Run("generated suffix _gen.go included with flag", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		_ = analyzer.Flags.Set("generated_files", "true")
		analysistest.Run(t, testdata, analyzer, "generated_suffix/b")
	})
}

// TestAnonymousStructsSkipped pins the documented contract that only struct
// types declared via `type T struct { ... }` are analysed. Anonymous struct
// types — appearing as var types, function parameter or return types,
// composite literal element types, map values, and as inline field types of
// a named struct — must never produce diagnostics. The testdata file
// contains intentionally misaligned anonymous structs whose field order
// would otherwise trigger a "struct of size 12 could be 8" diagnostic;
// the absence of any `// want` annotations is the assertion.
func TestAnonymousStructsSkipped(t *testing.T) {
	testdata := analysistest.TestData()
	analyzer := NewTestAnalyzer()
	analysistest.Run(t, testdata, analyzer, "anonymous")
}

// TestPositionalLiteralSuppressesReorder pins the safety contract that a
// named struct constructed via a positional composite literal anywhere in
// the package must not be silently reordered: the analyzer reports the
// would-be saving but appends a "reorder skipped" notice pointing at the
// offending literal so the user can convert it to a keyed form. Structs
// only used through keyed literals (or the zero-element `T{}` form) keep
// the original diagnostic verbatim. Covers plain literals, elided literals
// nested in slice composites, the address-of pattern (`&T{...}`), and a
// `type S T` chain whose positional usage transitively pins the parent.
func TestPositionalLiteralSuppressesReorder(t *testing.T) {
	testdata := analysistest.TestData()
	analyzer := NewTestAnalyzer()
	analysistest.Run(t, testdata, analyzer, "positional")
}
