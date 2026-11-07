// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package betteralign_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dkorunic/betteralign"
	"github.com/dkorunic/betteralign/internal/diff"
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

// TestSuggestions checks the diagnostics of package a and that its
// SuggestedFixes, once applied, reproduce the -apply goldens: the two rewrite
// paths must agree.
func TestSuggestions(t *testing.T) {
	testdata := analysistest.TestData()
	analyzer := NewTestAnalyzer()
	results := analysistest.RunWithSuggestedFixes(t, testdata, analyzer, "a")
	assertRawFixesMatchGolden(t, results, "a")
}

// assertRawFixesMatchGolden applies each file's SuggestedFixes as plain byte
// edits — no gofmt pass, unlike RunWithSuggestedFixes — and requires the
// golden byte for byte. Hosts such as golangci-lint may apply edits without
// formatting, so the edits themselves must carry gofmt's column alignment.
// Files whose golden equals the source must receive no edits at all.
func assertRawFixesMatchGolden(t *testing.T, results []*analysistest.Result, pkg string) {
	t.Helper()
	type edit struct {
		start, end int
		text       []byte
	}
	byFile := make(map[string][]edit)
	for _, r := range results {
		for _, d := range r.Diagnostics {
			for _, f := range d.SuggestedFixes {
				for _, e := range f.TextEdits {
					tf := r.Pass.Fset.File(e.Pos)
					byFile[tf.Name()] = append(byFile[tf.Name()], edit{tf.Offset(e.Pos), tf.Offset(e.End), e.NewText})
				}
			}
		}
	}

	paths, err := filepath.Glob(filepath.Join(analysistest.TestData(), "src", pkg, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range removeOtherArches(paths) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(path + ".golden")
		if err != nil {
			t.Fatal(err)
		}
		edits := byFile[path]
		// Apply back to front so earlier offsets stay valid.
		slices.SortFunc(edits, func(a, b edit) int { return b.start - a.start })
		got := slices.Clone(src)
		for i, e := range edits {
			if i > 0 && e.end > edits[i-1].start {
				t.Fatalf("%s: overlapping edits at %d and %d", path, e.start, edits[i-1].start)
			}
			got = slices.Concat(got[:e.start], e.text, got[e.end:])
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: raw SuggestedFixes differ from golden\n%s", filepath.Base(path),
				diff.Diff("golden", want, "fixed", got))
		}
	}
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

// TestFixLeavesWritingToDriver pins that -fix is no longer an alias of
// -apply: the driver applies the SuggestedFixes itself, so the analyzer must
// not touch the files (the driver would then abort on the size change), even
// when -apply is also set. The fixes must still be attached.
func TestFixLeavesWritingToDriver(t *testing.T) {
	skipUnless64Bit(t)
	tmpPkg, paths := copyTestPackage(t, "layoutpin")

	analyzer := NewTestAnalyzer()
	var fix bool
	analyzer.Flags.BoolVar(&fix, "fix", false, "host-registered -fix (test harness only)")
	_ = analyzer.Flags.Set("fix", "true")
	_ = analyzer.Flags.Set("apply", "true")
	results := analysistest.Run(t, analysistest.TestData(), analyzer, tmpPkg)

	for _, path := range paths {
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(analysistest.TestData(), "src", tmpPkg, filepath.Base(path)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s was written by the analyzer under -fix; that is the driver's job", filepath.Base(path))
		}
	}
	if countFixes(results) == 0 {
		t.Error("no SuggestedFixes under -fix; the driver would have nothing to apply")
	}
}

// countFixes counts the SuggestedFixes across all results.
func countFixes(results []*analysistest.Result) int {
	n := 0
	for _, r := range results {
		for _, d := range r.Diagnostics {
			n += len(d.SuggestedFixes)
		}
	}
	return n
}

// TestSuggestedFixesLayoutPins pins that pinned structs (positional literal,
// unsafe.Offsetof) are reported without a fix, while the unpinned controls get
// one whose raw edits reproduce the -apply goldens byte for byte.
func TestSuggestedFixesLayoutPins(t *testing.T) {
	skipUnless64Bit(t)
	results := analysistest.RunWithSuggestedFixes(t, analysistest.TestData(), NewTestAnalyzer(), "layoutpin")
	for _, r := range results {
		for _, d := range r.Diagnostics {
			pinned := strings.Contains(d.Message, "reorder skipped")
			if pinned && len(d.SuggestedFixes) != 0 {
				t.Errorf("pinned diagnostic carries a fix: %s", d.Message)
			}
			if !pinned && len(d.SuggestedFixes) != 1 {
				t.Errorf("unpinned diagnostic has %d fixes, want 1: %s", len(d.SuggestedFixes), d.Message)
			}
			for _, f := range d.SuggestedFixes {
				if f.Message != "Rearrange fields" {
					t.Errorf("fix message = %q, want upstream fieldalignment's %q", f.Message, "Rearrange fields")
				}
			}
		}
	}
	assertRawFixesMatchGolden(t, results, "layoutpin")
}

// analyzeWithApply loads pattern from the GOPATH-style testdata tree and runs
// the analyzer with -apply via checker.Analyze. Unlike analysistest.Run it doesn't
// enforce `// want` comments — needed when the base pass and its [pkg.test]
// variant must diverge on whether a shared file reports a diagnostic, which the
// per-root want model can't express. The -apply rewrites still happen as a side
// effect, as under analysistest.Run.
func analyzeWithApply(t *testing.T, pattern string) *checker.Graph {
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
	if err := analyzer.Flags.Set("apply", "true"); err != nil {
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
	g := analyzeWithApply(t, pattern)

	got, err := os.ReadFile(filepath.Join(tmpWorkDir, "lit.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != orig["lit.go"] {
		t.Errorf("lit.go was rewritten despite a positional literal in the test file:\n--- got:\n%s\n--- want (unchanged):\n%s", got, orig["lit.go"])
	}

	// Only the rewrite is deferred: base reports plain, variant adds the caveat. Both
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
		// The fix is deferred like -apply: under -fix the driver would apply a
		// base-pass fix the variant knows breaks the test build.
		if len(d.SuggestedFixes) != 0 {
			t.Errorf("base pass attached a fix despite deferring to the test variant: %q", d.Message)
		}
	}
	for _, d := range variant.Diagnostics {
		if len(d.SuggestedFixes) != 0 {
			t.Errorf("test variant attached a fix to a pinned struct: %q", d.Message)
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
// silent. Only the rewrite is deferred, so the base pass still reports it.
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
	g := analyzeWithApply(t, pattern)

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
	_ = analyzer.Flags.Set("apply", "true")

	results := analysistest.Run(t, testdata, analyzer, filepath.Join(filepath.Base(tmpDir), "litexcluded"))
	if countFixes(results) == 0 {
		t.Error("no SuggestedFix: a build-excluded test literal must not withhold it")
	}

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

// TestLayoutPins pins the three layout contracts betteralign must respect:
// structs.HostLayout structs and structs with a non-zero blank padding field
// are not reported at all, while structs with an unsafe.Offsetof use are
// reported with a "reorder skipped" caveat naming the offending position.
// Negative cases (zero-sized blank markers, named pads, a look-alike
// HostLayout, Sizeof and Alignof, a user method named Offsetof) keep the plain
// diagnostic.
func TestLayoutPins(t *testing.T) {
	skipUnless64Bit(t)
	testdata := analysistest.TestData()
	analyzer := NewTestAnalyzer()
	analysistest.Run(t, testdata, analyzer, "layoutpin")
}

// TestApplyLayoutPins pins the rewrite half of TestLayoutPins: under -apply the
// pinned structs keep their source order byte for byte, while the unpinned
// controls in the same files are still reordered, proving the rewrite ran.
func TestApplyLayoutPins(t *testing.T) {
	skipUnless64Bit(t)
	tmpPkg, paths := copyTestPackage(t, "layoutpin")

	analyzer := NewTestAnalyzer()
	_ = analyzer.Flags.Set("apply", "true")
	analysistest.Run(t, analysistest.TestData(), analyzer, tmpPkg)

	for _, path := range paths {
		got, err := os.ReadFile(filepath.Join(analysistest.TestData(), "src", tmpPkg, filepath.Base(path)))
		if err != nil {
			t.Fatal(err)
		}
		golden.Assert(t, string(got), filepath.Join("src", "layoutpin", filepath.Base(path)+".golden"))
	}
}

// skipUnless64Bit skips fixtures whose sizes assume an 8-byte word.
func skipUnless64Bit(t *testing.T) {
	t.Helper()
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skipf("fixture assumes 64-bit word size, GOARCH=%s", runtime.GOARCH)
	}
}

// copyTestPackage copies testdata/src/<name>/*.go into a fresh temp package
// under testdata/src (removed on cleanup) and returns its import path relative
// to testdata/src together with the copied source paths.
func copyTestPackage(t *testing.T, name string) (string, []string) {
	t.Helper()
	srcDir := filepath.Join("testdata", "src")
	tmpDir, err := os.MkdirTemp(srcDir, filepath.Base(name)+"-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	tmpPkg := filepath.Join(filepath.Base(tmpDir), name)
	if err := os.MkdirAll(filepath.Join(srcDir, tmpPkg), 0o750); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(srcDir, name, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(srcDir, tmpPkg, filepath.Base(path)), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return tmpPkg, paths
}

// runDiffMode runs the analyzer over a temp copy of layoutpin with stdout
// diffs captured, after configure has set the flags under test. It returns the
// captured output and fails if any source file changed on disk.
func runDiffMode(t *testing.T, configure func(*analysis.Analyzer)) (string, []string, string) {
	t.Helper()
	out, jsonOut, paths, tmpDir := runDiffModeJSON(t, configure)
	if jsonOut != "" {
		t.Errorf("diff went to the -json sink without -json:\n%s", jsonOut)
	}
	return out, paths, tmpDir
}

// runDiffModeJSON is runDiffMode that also returns what went to the -json
// diff sink (stderr in production).
func runDiffModeJSON(t *testing.T, configure func(*analysis.Analyzer)) (string, string, []string, string) {
	t.Helper()
	skipUnless64Bit(t)
	tmpPkg, paths := copyTestPackage(t, "layoutpin")

	var out, jsonOut bytes.Buffer
	saved, savedJSON := *betteralign.DiffOut, *betteralign.DiffJSONOut
	*betteralign.DiffOut, *betteralign.DiffJSONOut = &out, &jsonOut
	t.Cleanup(func() { *betteralign.DiffOut, *betteralign.DiffJSONOut = saved, savedJSON })

	analyzer := NewTestAnalyzer()
	configure(analyzer)
	analysistest.Run(t, analysistest.TestData(), analyzer, tmpPkg)

	tmpDir := filepath.Join(analysistest.TestData(), "src", tmpPkg)
	for _, path := range paths {
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(tmpDir, filepath.Base(path)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s was modified on disk; -diff must only preview", filepath.Base(path))
		}
	}
	return out.String(), jsonOut.String(), paths, tmpDir
}

// wantLayoutPinDiff builds the expected -diff output: one gofmt -d style diff
// per file whose -apply golden differs, in the sorted order run writes them.
// Files the rewrite leaves alone (dotimport.go) contribute nothing.
func wantLayoutPinDiff(t *testing.T, paths []string, tmpDir string) string {
	t.Helper()
	var want bytes.Buffer
	for _, path := range paths { // filepath.Glob returns sorted paths
		orig, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rewritten, err := os.ReadFile(path + ".golden")
		if err != nil {
			t.Fatal(err)
		}
		fn := filepath.Join(tmpDir, filepath.Base(path))
		want.Write(diff.Diff(fn+".orig", orig, fn, rewritten))
	}
	if want.Len() == 0 {
		t.Fatal("fixture produced no expected diff; the test would prove nothing")
	}
	return want.String()
}

// TestApplyDiff pins -apply -diff: the rewrite is printed as a unified diff
// matching the -apply golden output, and no file is written.
func TestApplyDiff(t *testing.T) {
	got, paths, tmpDir := runDiffMode(t, func(a *analysis.Analyzer) {
		var d bool
		a.Flags.BoolVar(&d, "diff", false, "host-registered -diff (test harness only)")
		_ = a.Flags.Set("apply", "true")
		_ = a.Flags.Set("diff", "true")
	})
	if want := wantLayoutPinDiff(t, paths, tmpDir); got != want {
		t.Errorf("diff output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestFixDiffGlobalFlags is the regression for `betteralign -fix -diff`: the
// driver registers both flags on the process-global set, and -fix used to
// rewrite files the user asked only to preview.
//
// With SuggestedFixes populated, the driver owns both: it prints the diff from
// the fixes and never writes. The analyzer must therefore stay silent and
// leave the files alone even with -apply also set (runDiffMode checks disk).
func TestFixDiffGlobalFlags(t *testing.T) {
	got, _, _ := runDiffMode(t, func(a *analysis.Analyzer) {
		_ = a.Flags.Set("apply", "true")
		*betteralign.FixGlobalFlag = true
		*betteralign.DiffGlobalFlag = true
		t.Cleanup(func() {
			*betteralign.FixGlobalFlag = false
			*betteralign.DiffGlobalFlag = false
		})
	})
	if got != "" {
		t.Errorf("analyzer printed a diff under -fix -diff; the driver prints it from the fixes:\n%s", got)
	}
}

// TestDiffWithoutApply pins that -diff alone stays report-only: it neither
// prints a diff nor writes files, matching the driver's "-diff needs -fix".
func TestDiffWithoutApply(t *testing.T) {
	got, _, _ := runDiffMode(t, func(a *analysis.Analyzer) {
		var d bool
		a.Flags.BoolVar(&d, "diff", false, "host-registered -diff (test harness only)")
		_ = a.Flags.Set("diff", "true")
	})
	if got != "" {
		t.Errorf("-diff without -apply printed output:\n%s", got)
	}
}

// TestApplyDiffJSON pins that under -json the diff goes to the stderr sink,
// leaving stdout to the driver's JSON, and is otherwise the same diff.
func TestApplyDiffJSON(t *testing.T) {
	out, jsonOut, paths, tmpDir := runDiffModeJSON(t, func(a *analysis.Analyzer) {
		var d, j bool
		a.Flags.BoolVar(&d, "diff", false, "host-registered -diff (test harness only)")
		a.Flags.BoolVar(&j, "json", false, "host-registered -json (test harness only)")
		_ = a.Flags.Set("apply", "true")
		_ = a.Flags.Set("diff", "true")
		_ = a.Flags.Set("json", "true")
	})
	if out != "" {
		t.Errorf("-json -diff wrote to stdout, corrupting the JSON stream:\n%s", out)
	}
	if want := wantLayoutPinDiff(t, paths, tmpDir); jsonOut != want {
		t.Errorf("diff output mismatch\n--- got ---\n%s\n--- want ---\n%s", jsonOut, want)
	}
}

// TestFixWithGeneratedFiles is the regression for -fix -generated_files: the
// driver refuses fixes in "Code generated ... DO NOT EDIT" files, so the pair
// silently rewrote nothing there (in v0.15.4, where -fix aliased -apply, it
// did). The analyzer must write the generated file itself and attach no fixes,
// or the driver's own writes would race it.
func TestFixWithGeneratedFiles(t *testing.T) {
	tmpPkg, paths := copyTestPackage(t, filepath.Join("generated", "b"))

	analyzer := NewTestAnalyzer()
	var fix bool
	analyzer.Flags.BoolVar(&fix, "fix", false, "host-registered -fix (test harness only)")
	_ = analyzer.Flags.Set("fix", "true")
	_ = analyzer.Flags.Set("generated_files", "true")
	results := analysistest.Run(t, analysistest.TestData(), analyzer, tmpPkg)

	if n := countFixes(results); n != 0 {
		t.Errorf("%d SuggestedFixes attached; the analyzer writes itself here, so the driver must get none", n)
	}
	got, err := os.ReadFile(filepath.Join(analysistest.TestData(), "src", tmpPkg, filepath.Base(paths[0])))
	if err != nil {
		t.Fatal(err)
	}
	// Optimal order puts the int32 first.
	if y, x := strings.Index(string(got), "y int32"), strings.Index(string(got), "x byte"); y < 0 || x < 0 || y > x {
		t.Errorf("generated file was not rewritten under -fix -generated_files:\n%s", got)
	}
}

// TestCrossPackageConversionFollowsPartner pins the fix for a regression the
// conversion pin introduced: pinning the local side of a conversion to a
// struct that another package's pass reorders broke the build (rewriting the
// standard library failed at net/http's HTTP2Config -> http2.Config, which
// v0.15.4 handled by reordering both). A same-run partner predicts the local
// side reorders too; local-to-local conversions stay pinned.
func TestCrossPackageConversionFollowsPartner(t *testing.T) {
	skipUnless64Bit(t)
	analysistest.Run(t, analysistest.TestData(), NewTestAnalyzer(), "xconv/user")
}
