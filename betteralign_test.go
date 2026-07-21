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
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"
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

// TestGreenTeaWording pins the Go 1.26+ scan-work framing of the pointer-bytes
// diagnostic; the pre-1.26 "bytes saved" wording is covered by TestSuggestions.
// Skips on 32-bit arches, where the fixture's uintptr math no longer holds.
func TestGreenTeaWording(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skipf("pointer-bytes fixture assumes 64-bit word size, GOARCH=%s", runtime.GOARCH)
	}
	testdata := analysistest.TestData()
	analyzer := NewTestAnalyzer()
	analysistest.Run(t, testdata, analyzer, "greentea")
}

// TestGenericLayoutSuppressed pins that a struct whose layout depends on a type
// parameter (a by-value type-param field or array-of-type-param) is not
// reported — its size/ptrdata would be a per-instantiation guess — while a
// non-generic struct and a generic struct whose fields have fixed layout (a
// pointer to the type param) are still reported.
func TestGenericLayoutSuppressed(t *testing.T) {
	testdata := analysistest.TestData()
	analyzer := NewTestAnalyzer()
	analysistest.Run(t, testdata, analyzer, "generics")
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

// analyzeWithFix loads pattern from the GOPATH-style testdata tree and runs the
// analyzer with -fix via checker.Analyze. Unlike analysistest.Run it doesn't
// enforce `// want` comments — needed when the base pass and its [pkg.test]
// variant must diverge on whether a shared file reports a diagnostic, which the
// per-root want model can't express. The -fix rewrites still happen as a side
// effect, as under analysistest.Run.
func analyzeWithFix(t *testing.T, pattern string) *checker.Graph {
	t.Helper()
	dir := analysistest.TestData()
	mode := packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
		packages.NeedImports | packages.NeedTypes | packages.NeedTypesSizes |
		packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedDeps | packages.NeedModule
	cfg := &packages.Config{
		Mode:  mode,
		Dir:   dir,
		Tests: true,
		Env:   append(os.Environ(), "GOPATH="+dir, "GO111MODULE=off", "GOWORK=off"),
	}
	pkgs, err := packages.Load(cfg, pattern)
	if err != nil {
		t.Fatalf("load %s: %v", pattern, err)
	}
	analyzer := NewTestAnalyzer()
	var fix bool
	analyzer.Flags.BoolVar(&fix, "fix", false, "alias for -apply (test harness only)")
	if err := analyzer.Flags.Set("fix", "true"); err != nil {
		t.Fatal(err)
	}
	g, err := checker.Analyze([]*analysis.Analyzer{analyzer}, pkgs, nil)
	if err != nil {
		t.Fatalf("analyze %s: %v", pattern, err)
	}
	return g
}

// TestApplyDefersPositionalLiteralInTestFile is the issue #36 sibling
// regression: a struct pinned by a positional literal in an in-package test
// file must survive -fix — the base pass defers -apply to the [pkg.test]
// variant. It also pins the reporting split: the base reports plain, the
// variant reports the caveat. That per-root divergence needs checker.Analyze
// rather than analysistest.Run.
func TestApplyDefersPositionalLiteralInTestFile(t *testing.T) {
	srcDir := filepath.Join("testdata", "src")
	workDir := filepath.Join(srcDir, "litguard")

	tmpDir, err := os.MkdirTemp(srcDir, "litguard-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	tmpWorkDir := filepath.Join(tmpDir, "litguard")
	if err := os.Mkdir(tmpWorkDir, 0o750); err != nil {
		t.Fatal(err)
	}

	paths, err := filepath.Glob(filepath.Join(workDir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	orig := make(map[string]string, len(paths))
	for _, path := range paths {
		base := filepath.Base(path)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		orig[base] = string(src)
		if err := os.WriteFile(filepath.Join(tmpWorkDir, base), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// act.Package.ID is slash-based even on Windows; match it.
	pattern := filepath.ToSlash(filepath.Join(filepath.Base(tmpDir), "litguard"))
	g := analyzeWithFix(t, pattern)

	got, err := os.ReadFile(filepath.Join(tmpWorkDir, "lit.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != orig["lit.go"] {
		t.Errorf("lit.go was rewritten despite a positional literal in the test file:\n--- got:\n%s\n--- want (unchanged):\n%s", got, orig["lit.go"])
	}

	// Only -apply is deferred: base reports plain, variant adds the caveat. Both
	// appear (the accepted duplicate) — see deferToTestVariant.
	var base, variant *checker.Action
	for _, act := range g.Roots {
		switch id := act.Package.ID; {
		case id == pattern:
			base = act
		case strings.HasPrefix(id, pattern+" [") && strings.HasSuffix(id, ".test]"):
			variant = act
		}
	}
	if base == nil {
		t.Fatalf("base package %q not among roots", pattern)
	}
	if variant == nil {
		t.Fatalf("[pkg.test] variant for %q not among roots", pattern)
	}
	if len(base.Diagnostics) == 0 {
		t.Errorf("base pass reported nothing; it must still cover the struct")
	}
	for _, d := range base.Diagnostics {
		if strings.Contains(d.Message, "reorder skipped") {
			t.Errorf("base pass emitted the caveat, which it can't know: %q", d.Message)
		}
	}
	caveat := false
	for _, d := range variant.Diagnostics {
		if strings.Contains(d.Message, "reorder skipped") {
			caveat = true
		}
	}
	if !caveat {
		t.Errorf("test variant did not report the caveat diagnostic; got %v", variant.Diagnostics)
	}
}

// TestReportsHealthyCodeWhenTestFileIllTyped is the regression guard for the
// deferral: a healthy non-test file must still be reported when an in-package
// test file fails to type-check. The driver skips the ill-typed [pkg.test]
// variant, so if the base pass deferred *reporting* too the struct would go
// silent. Only -apply is deferred, so the base pass still reports it.
func TestReportsHealthyCodeWhenTestFileIllTyped(t *testing.T) {
	srcDir := filepath.Join("testdata", "src")
	tmpDir, err := os.MkdirTemp(srcDir, "illtyped-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	pkgDir := filepath.Join(tmpDir, "illtyped")
	if err := os.Mkdir(pkgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Healthy non-test file with a misaligned struct.
	if err := os.WriteFile(filepath.Join(pkgDir, "p.go"),
		[]byte("package illtyped\n\ntype T struct {\n\tA bool\n\tB int64\n\tC bool\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Valid syntax, type error in the body → the [pkg.test] variant is ill-typed
	// and skipped, while the base package stays healthy.
	if err := os.WriteFile(filepath.Join(pkgDir, "p_test.go"),
		[]byte("package illtyped\n\nvar _ int = \"not an int\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pattern := filepath.ToSlash(filepath.Join(filepath.Base(tmpDir), "illtyped"))
	g := analyzeWithFix(t, pattern)

	var base *checker.Action
	for _, act := range g.Roots {
		if act.Package.ID == pattern {
			base = act
		}
	}
	if base == nil {
		t.Fatalf("base package %q not among roots", pattern)
	}
	found := false
	for _, d := range base.Diagnostics {
		if strings.Contains(d.Message, "could be") {
			found = true
		}
	}
	if !found {
		t.Errorf("base pass reported no diagnostic for the healthy struct; got %v (regression: an ill-typed test file blanked healthy code)", base.Diagnostics)
	}
}

// TestApplyRewritesWhenTestFileBuildExcluded guards the deferral from over-firing:
// a positional literal in a build-excluded test file is never compiled, so the
// struct must still be rewritten. The deferral must key on files go/packages
// actually loaded, not on a disk glob that ignores build constraints.
func TestApplyRewritesWhenTestFileBuildExcluded(t *testing.T) {
	srcDir := filepath.Join("testdata", "src")
	workDir := filepath.Join(srcDir, "litexcluded")

	tmpDir, err := os.MkdirTemp(srcDir, "litexcluded-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	tmpWorkDir := filepath.Join(tmpDir, "litexcluded")
	if err := os.Mkdir(tmpWorkDir, 0o750); err != nil {
		t.Fatal(err)
	}

	paths, err := filepath.Glob(filepath.Join(workDir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmpWorkDir, filepath.Base(path)), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	testdata := analysistest.TestData()
	analyzer := NewTestAnalyzer()
	var fix bool
	analyzer.Flags.BoolVar(&fix, "fix", false, "alias for -apply (test harness only)")
	_ = analyzer.Flags.Set("fix", "true")

	analysistest.Run(t, testdata, analyzer, filepath.Join(filepath.Base(tmpDir), "litexcluded"))

	got, err := os.ReadFile(filepath.Join(tmpWorkDir, "lit.go"))
	if err != nil {
		t.Fatal(err)
	}
	// Optimal order is b, c, a: the pointer field must move ahead of the bool.
	if b, a := strings.Index(string(got), "b *int"), strings.Index(string(got), "a bool"); b < 0 || a < 0 || b > a {
		t.Errorf("lit.go was not reordered (build-excluded test literal must not block the fix):\n%s", got)
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

// TestSingleLineStructReported pins that a misaligned struct whose source
// shape dstmin cannot decorate (a single-line `type S struct { ... }`) still
// produces the size diagnostic in report-only mode. Reporting is decoupled
// from rewritability: only the -apply rewrite needs the DST node, so a
// non-decoratable struct is reported and simply not rewritten — not silently
// dropped with a stderr warning, as it was before.
//
// The source is written to a temp package at runtime rather than committed:
// gofmt (via `task fmt`'s gofumpt -w .) would expand the single-line struct
// to multi-line, which is decoratable and would silently void the regression
// guard. Generating it on the fly keeps the un-formattable shape intact.
func TestSingleLineStructReported(t *testing.T) {
	srcDir := filepath.Join("testdata", "src")
	tmpDir, err := os.MkdirTemp(srcDir, "singleline-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	pkgDir := filepath.Join(tmpDir, "singleline")
	if err := os.Mkdir(pkgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	src := "package singleline\n\n" +
		"// Single-line struct: dstmin cannot decorate it, but the diagnostic must still fire.\n" +
		"type S struct { a bool; b int64; c bool } // want `struct of size 24 could be 16`\n"
	if err := os.WriteFile(filepath.Join(pkgDir, "singleline.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	testdata := analysistest.TestData()
	analyzer := NewTestAnalyzer()
	analysistest.Run(t, testdata, analyzer, filepath.Join(filepath.Base(tmpDir), "singleline"))
}
