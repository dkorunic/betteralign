// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package betteralign_test

import (
	"bytes"
	"fmt"
	"go/build"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
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
	skipUnless64Bit(t)
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
	skipUnless64Bit(t)
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
	skipUnless64Bit(t)
	testdata := analysistest.TestData()
	analyzer := NewTestAnalyzer()
	analysistest.Run(t, testdata, analyzer, "generics")
}

func TestApply(t *testing.T) {
	skipUnless64Bit(t)
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

// TestApplyAttachesNoFixes pins that the analyzer attaches no SuggestedFixes
// when it writes the files itself: a host that sets -apply and then applies
// the fixes it collected (golangci-lint's fixer with a module plugin set to
// apply: true, run with --fix — not a Go flag, so flagRequested can't see it)
// applied them on top of the rewritten file at offsets an earlier struct's
// rewrite had shifted, corrupting it. Under -apply -diff nothing is written,
// so the fixes stay.
func TestApplyAttachesNoFixes(t *testing.T) {
	skipUnless64Bit(t)
	tmpPkg, _ := copyTestPackage(t, "layoutpin")
	analyzer := NewTestAnalyzer()
	_ = analyzer.Flags.Set("apply", "true")
	if n := countFixes(analysistest.Run(t, analysistest.TestData(), analyzer, tmpPkg)); n != 0 {
		t.Errorf("%d SuggestedFixes attached under -apply, which already wrote the files", n)
	}

	tmpPkg, _ = copyTestPackage(t, "layoutpin")
	var out bytes.Buffer
	saved := *betteralign.DiffOut
	*betteralign.DiffOut = &out
	t.Cleanup(func() { *betteralign.DiffOut = saved })
	analyzer = NewTestAnalyzer()
	var d bool
	analyzer.Flags.BoolVar(&d, "diff", false, "host-registered -diff (test harness only)")
	_ = analyzer.Flags.Set("apply", "true")
	_ = analyzer.Flags.Set("diff", "true")
	if countFixes(analysistest.Run(t, analysistest.TestData(), analyzer, tmpPkg)) == 0 {
		t.Error("no SuggestedFixes under -apply -diff, which writes nothing")
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

// analyzeTestdata loads pattern from the GOPATH-style testdata tree and runs
// the analyzer, with -apply if apply is set, via checker.Analyze. Unlike
// analysistest.Run it doesn't enforce `// want` comments — needed when the base
// pass and its [pkg.test] variant must diverge on whether a shared file reports
// a diagnostic, which the per-root want model can't express. The -apply
// rewrites still happen as a side effect, as under analysistest.Run.
func analyzeTestdata(t *testing.T, pattern string, apply bool) *checker.Graph {
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
	if err := analyzer.Flags.Set("apply", strconv.FormatBool(apply)); err != nil {
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
	analyzeTestdata(t, pattern, true)

	got, err := os.ReadFile(filepath.Join(tmpWorkDir, "lit.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != orig["lit.go"] {
		t.Errorf("lit.go was rewritten despite a positional literal in the test file:\n--- got:\n%s\n--- want (unchanged):\n%s", got, orig["lit.go"])
	}

	// Diagnostics and fixes from a report-only run: -apply attaches no fixes.
	g := analyzeTestdata(t, pattern, false)

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
	g := analyzeTestdata(t, pattern, true)

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
// a positional literal in a test file excluded from every build (//go:build
// ignore) is never compiled, so the struct must still be rewritten. The
// deferral must key on files go/packages actually loaded, not on a disk glob
// that ignores build constraints. (A file behind an ordinary tag or another
// GOOS does compile somewhere and pins; see TestOtherBuildPositional.)
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
	pkg := filepath.Join(filepath.Base(tmpDir), "litexcluded")
	// Report-only first: -apply writes itself and attaches no fixes.
	if countFixes(analysistest.Run(t, testdata, NewTestAnalyzer(), pkg)) == 0 {
		t.Error("no SuggestedFix: a build-excluded test literal must not withhold it")
	}
	analyzer := NewTestAnalyzer()
	_ = analyzer.Flags.Set("apply", "true")
	analysistest.Run(t, testdata, analyzer, pkg)

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
	skipUnless64Bit(t)
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
	skipUnless64Bit(t)
	t.Run("exclude b", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		_ = analyzer.Flags.Set("apply", "false")
		_ = analyzer.Flags.Set("exclude_files", "testdata/src/exclude/b/b/*.go")
		analysistest.Run(t, testdata, analyzer, "exclude/b/...")
	})
}

func TestFlagOptInMode(t *testing.T) {
	skipUnless64Bit(t)
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

// TestCgoFileNotRewritten pins that -generated_files reports a cgo file's
// struct without a fix: the file go/packages parses is cgo's output in the
// build cache, so a rewrite would change the cache and leave the source as is.
func TestCgoFileNotRewritten(t *testing.T) {
	skipUnlessCgo(t)
	analyzer := NewTestAnalyzer()
	_ = analyzer.Flags.Set("generated_files", "true")
	for _, r := range analysistest.Run(t, analysistest.TestData(), analyzer, "cgofile") {
		for _, d := range r.Diagnostics {
			if len(d.SuggestedFixes) != 0 {
				t.Errorf("fix attached to cgo output: %s", d.Message)
			}
		}
	}
}

// TestCgoPartnerPinsConversion pins the prediction for a conversion partner
// declared in a cgo file: no run rewrites it, so the local side must not move.
func TestCgoPartnerPinsConversion(t *testing.T) {
	skipUnlessCgo(t)
	analysistest.Run(t, analysistest.TestData(), NewTestAnalyzer(), "cgofile/user")
}

// TestRefusedPartnerPinsConversion pins the prediction for a conversion
// partner whose shape dstmin refuses: it stays as declared, so the local side
// must too.
func TestRefusedPartnerPinsConversion(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), NewTestAnalyzer(), "xrefuse/...")
}

// TestLineDirectivePartnerPinsConversion pins that a partner's declaration is
// judged from the file it is really in, not the one a //line directive names.
func TestLineDirectivePartnerPinsConversion(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), NewTestAnalyzer(), "xline/user")
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
	skipUnless64Bit(t)
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

// TestLinknameSelf pins the one //go:linkname exemption from the assembly pin:
// a pull whose target is in the package itself, Go compiled against the real
// layout, keeps its fix (matching the golden); a pull from another package
// stays pinned. Analyzed in place, since the target names the package path.
func TestLinknameSelf(t *testing.T) {
	skipUnless64Bit(t)
	results := analysistest.RunWithSuggestedFixes(t, analysistest.TestData(), NewTestAnalyzer(), "linkself")
	assertRawFixesMatchGolden(t, results, "linkself")
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

// skipUnlessCgo skips fixtures that need cmd/cgo and a C compiler.
func skipUnlessCgo(t *testing.T) {
	t.Helper()
	if !build.Default.CgoEnabled {
		t.Skip("cgo disabled")
	}
	cc, err := exec.Command("go", "env", "CC").Output()
	if err != nil {
		t.Skipf("go env CC: %v", err)
	}
	if _, err := exec.LookPath(strings.Fields(string(cc) + " cc")[0]); err != nil {
		t.Skipf("no C compiler: %v", err)
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
	skipUnless64Bit(t)
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
	skipUnless64Bit(t)
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
	skipUnless64Bit(t)
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
	skipUnless64Bit(t)
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

// TestOtherBuildPositional pins positional literals in files the current
// build excludes (another GOOS/GOARCH, an unset build tag): type checking
// never sees them, so a reorder broke that platform's build or, with
// compatible field types, silently re-mapped its values.
func TestOtherBuildPositional(t *testing.T) {
	skipUnless64Bit(t)
	analysistest.Run(t, analysistest.TestData(), NewTestAnalyzer(), "otherbuild")
}

// TestOtherBuildPositionalNegatedTag runs the otherbuild fixture with a tag
// set that negated.go excludes itself by: its literal is outside this build
// though go/build's defaults would include it, so the excluded files must be
// the load's own (pass.IgnoredFiles).
func TestOtherBuildPositionalNegatedTag(t *testing.T) {
	skipUnless64Bit(t)
	t.Setenv("GOFLAGS", "-tags=betteralign_set")
	analysistest.Run(t, analysistest.TestData(), NewTestAnalyzer(), "otherbuild")
}

// TestNested checks the diagnostics of structs holding another struct by value:
// an inner struct the run rewrites is measured at its new layout, one it
// leaves alone (ignored, single-line, generated) as declared.
func TestNested(t *testing.T) {
	skipUnless64Bit(t)
	analysistest.Run(t, analysistest.TestData(), NewTestAnalyzer(), "nested")
}

// TestApplyNested checks the -apply output for the nested fixture: inner
// structs reordered, outers left where the reordered inner already fits.
func TestApplyNested(t *testing.T) {
	skipUnless64Bit(t)
	tmpPkg, paths := copyTestPackage(t, "nested")

	analyzer := NewTestAnalyzer()
	_ = analyzer.Flags.Set("apply", "true")
	analysistest.Run(t, analysistest.TestData(), analyzer, tmpPkg)

	for _, path := range paths {
		got, err := os.ReadFile(filepath.Join(analysistest.TestData(), "src", tmpPkg, filepath.Base(path)))
		if err != nil {
			t.Fatal(err)
		}
		golden.Assert(t, string(got), filepath.Join("src", "nested", filepath.Base(path)+".golden"))
	}
}

// TestApplyIsFixedPoint pins one-run convergence: a second -apply over the
// first one's output changes nothing. An enclosing struct used to be laid out
// against its inner struct's old layout, which the same run then changed, so
// the next run moved it again (x/tools needed four runs) and a CI check
// failed right after -fix.
func TestApplyIsFixedPoint(t *testing.T) {
	skipUnless64Bit(t)
	for _, name := range []string{"a", "layoutpin", "nested"} {
		t.Run(name, func(t *testing.T) {
			tmpPkg, paths := copyTestPackage(t, name)
			dir := filepath.Join(analysistest.TestData(), "src", tmpPkg)
			apply := func() map[string]string {
				analyzer := NewTestAnalyzer()
				_ = analyzer.Flags.Set("apply", "true")
				// The want comments describe the pristine sources; once
				// rewritten they no longer match, so only load and type
				// errors matter here.
				rec := &errorRecorder{}
				analysistest.Run(rec, analysistest.TestData(), analyzer, tmpPkg)
				for _, msg := range rec.loadErrors() {
					t.Errorf("after -apply: %s", msg)
				}
				return readGoFiles(t, dir)
			}
			pristine := readGoFiles(t, dir)
			first := apply()
			if len(paths) > 0 && maps.Equal(pristine, first) {
				t.Fatalf("first -apply changed nothing in %s; the fixture no longer exercises a rewrite", name)
			}
			second := apply()
			for fn, src := range second {
				if src != first[fn] {
					t.Errorf("second -apply changed %s:\n%s", fn, diff.Diff("first", []byte(first[fn]), "second", []byte(src)))
				}
			}
		})
	}
}

// TestNestedConversionStaysInStep applies both sides of a cross-package
// conversion whose structs hold an inner struct the run reorders. Each pass
// must reach the same verdict on the pair, or the conversion stops
// compiling: the prediction behind the inner struct's measured layout uses
// only what both packages see alike.
func TestNestedConversionStaysInStep(t *testing.T) {
	skipUnless64Bit(t)
	tmpDir := applyBuilds(t, "xconv", nil)
	user, err := os.ReadFile(filepath.Join(tmpDir, "xconv", "user", "user.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(user), "type NT struct {\n\tF  dep.Four\n\tIn dep.Inner\n}") {
		t.Errorf("NT was reordered although dep.NU stays in place:\n%s", user)
	}
}

// applyBuilds copies fixture's dep and user packages to a temp tree, runs
// -apply over both with flags, and fails the test if the rewritten pair no
// longer type-checks (a conversion whose sides went out of step). It returns
// the temp tree's root.
func applyBuilds(t *testing.T, fixture string, flags map[string]string) string {
	t.Helper()
	srcDir := filepath.Join("testdata", "src")
	tmpDir, err := os.MkdirTemp(srcDir, filepath.Base(fixture)+"-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	root := filepath.Base(tmpDir)
	for _, pkg := range []string{"dep", "user"} {
		paths, err := filepath.Glob(filepath.Join(srcDir, fixture, pkg, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(tmpDir, fixture, pkg), 0o750); err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			src = bytes.ReplaceAll(src, []byte(`"`+fixture+`/`), []byte(`"`+root+`/`+fixture+`/`))
			if err := os.WriteFile(filepath.Join(tmpDir, fixture, pkg, filepath.Base(path)), src, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	reapplyBuilds(t, tmpDir, fixture, flags)
	return tmpDir
}

// reapplyBuilds runs -apply with flags over the dep and user packages
// applyBuilds copied to tmpDir, and fails the test if the result no longer
// type-checks.
func reapplyBuilds(t *testing.T, tmpDir, fixture string, flags map[string]string) {
	t.Helper()
	root := filepath.Base(tmpDir)
	pkgs := []string{root + "/" + fixture + "/dep", root + "/" + fixture + "/user"}

	analyzer := NewTestAnalyzer()
	for k, v := range flags {
		_ = analyzer.Flags.Set(k, v)
	}
	_ = analyzer.Flags.Set("apply", "true")
	analysistest.Run(&errorRecorder{}, analysistest.TestData(), analyzer, pkgs...)

	// Reload the rewritten pair: a mismatched order is a type error.
	rec := &errorRecorder{}
	analysistest.Run(rec, analysistest.TestData(), NewTestAnalyzer(), pkgs...)
	for _, msg := range rec.loadErrors() {
		t.Errorf("after -apply: %s", msg)
	}
}

// TestApplyAfterEditInSameProcess runs -apply twice in one process with an
// edit to dep in between, as an embedding host would: the second run must
// predict dep.Ignored — no longer ignored — from the edited file, not from
// the bytes the first run kept as dep.go's pre-rewrite text, or dep's pass
// reorders it while user.ToIgnored stays pinned and the conversion breaks.
func TestApplyAfterEditInSameProcess(t *testing.T) {
	skipUnless64Bit(t)
	tmpDir := applyBuilds(t, "xconv", nil)
	depFile := filepath.Join(tmpDir, "xconv", "dep", "dep.go")
	src, err := os.ReadFile(depFile)
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(src, []byte("type Ignored struct { // betteralign:ignore"), []byte("type Ignored struct {"), 1)
	if bytes.Equal(edited, src) {
		t.Fatal("dep.Ignored's ignore directive not found")
	}
	if err := os.WriteFile(depFile, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	reapplyBuilds(t, tmpDir, "xconv", nil)
	user := readGoFiles(t, filepath.Join(tmpDir, "xconv", "user"))["user.go"]
	if !strings.Contains(user, "\tI2 int64\n\tI1 bool\n\tI3 bool\n") {
		t.Errorf("ToIgnored did not follow dep.Ignored:\n%s", user)
	}
}

// TestConversionPartnerPrediction pins the cross-package prediction against
// regressions that broke the build after one -apply: a partner aliased to a
// struct literal is rewritten like a defined type (so the local side
// follows); an ignore directive whose comment holds a brace, or above a
// block comment, keeps the partner (so the local side stays); under -opt_in a
// marked but ignored partner stays too.
func TestConversionPartnerPrediction(t *testing.T) {
	skipUnless64Bit(t)
	for _, fixture := range []string{"xalias", "xignore"} {
		t.Run(fixture, func(t *testing.T) {
			analysistest.Run(t, analysistest.TestData(), NewTestAnalyzer(), fixture+"/...")
			tmpDir := applyBuilds(t, fixture, nil)
			if fixture == "xalias" {
				got := readGoFiles(t, filepath.Join(tmpDir, fixture, "user"))["user.go"]
				if !strings.Contains(got, "\tP *int\n\tA byte\n\tB byte\n}") {
					t.Errorf("U was not reordered with dep.N:\n%s", got)
				}
			}
		})
	}
	t.Run("opt_in", func(t *testing.T) {
		applyBuilds(t, filepath.Join("optin", "xconv"), map[string]string{"opt_in": "true"})
	})
}

// TestLineDirectiveWithoutColumn is the end-to-end regression for BUG-45: a
// struct after a //line directive naming an absolute file but no column has
// column 0 ("unknown"), and measuring an enclosing struct read its
// declaration with that column and panicked, killing the run.
func TestLineDirectiveWithoutColumn(t *testing.T) {
	skipUnless64Bit(t)
	root := t.TempDir()
	dir := filepath.Join(root, "src", "linedir")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "a.go")
	src := "package linedir\n\n//line " + file + ":4\ntype inner struct {\n\tp *int\n\ta bool\n\tq *int\n}\n\n" +
		"type outer struct {\n\tf  [4]*int\n\tin inner\n}\n"
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &errorRecorder{}
	for _, r := range analysistest.Run(rec, root, NewTestAnalyzer(), "linedir") {
		// A file with a //line directive is never rewritten (lineDirective):
		// another package can't locate its declarations to predict that.
		for _, d := range r.Diagnostics {
			if len(d.SuggestedFixes) != 0 {
				t.Errorf("fix attached in a //line file: %s", d.Message)
			}
		}
	}
	if errs := rec.loadErrors(); len(errs) > 0 {
		t.Errorf("load errors: %v", errs)
	}
	if !slices.ContainsFunc(rec.msgs, func(m string) bool { return strings.Contains(m, "struct with 24 pointer bytes could be 16") }) {
		t.Errorf("inner not reported; got %q", rec.msgs)
	}
}

// errorRecorder is an analysistest.Testing that collects messages instead of
// failing, for runs whose want comments are knowingly stale.
type errorRecorder struct{ msgs []string }

func (r *errorRecorder) Errorf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

// loadErrors returns the messages that are not want-comment mismatches: load
// and type-check failures.
func (r *errorRecorder) loadErrors() []string {
	var out []string
	for _, m := range r.msgs {
		if strings.Contains(m, "unexpected diagnostic") || strings.Contains(m, "no diagnostic was reported") || strings.Contains(m, "does not match pattern") {
			continue
		}
		out = append(out, m)
	}
	return out
}

// readGoFiles returns dir's .go files by base name.
func readGoFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(p)] = string(src)
	}
	return out
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

// TestExportDataPartnerPinsConversion runs the real driver on xconv's user
// package alone, so dep — not analyzed — is loaded from export data
// (analysistest loads every dependency from source, so only the binary shows
// this). dep's pass never runs and never reorders dep.U, so user.T, converted
// to it, must stay pinned: predicting from dep.go's text on disk (the export
// data's positions name it) reordered T alone and broke the build.
func TestExportDataPartnerPinsConversion(t *testing.T) {
	skipUnless64Bit(t)
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin, mod := xconvModule(t)
	cmd := exec.Command(bin, "-apply", "./user")
	cmd.Dir = mod
	out, _ := cmd.CombinedOutput() // exits non-zero when it reports
	if !regexp.MustCompile(`user\.go:9:\d+: .*reorder skipped: conversion at`).Match(out) {
		t.Errorf("T not pinned by its conversion to dep.U:\n%s", out)
	}
	build := exec.Command("go", "build", "./...")
	build.Dir = mod
	if out, err := build.CombinedOutput(); err != nil {
		t.Errorf("rewritten module no longer builds: %v\n%s", err, out)
	}
}

// xconvModule builds the betteralign binary and copies testdata/src/xconv
// into a module, both under t.TempDir().
func xconvModule(t *testing.T) (bin, mod string) {
	t.Helper()
	tmp := t.TempDir()
	bin = filepath.Join(tmp, "betteralign")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/betteralign").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin, copyXconv(t, filepath.Join(tmp, "xconv"))
}

// copyXconv copies testdata/src/xconv into a module at mod.
func copyXconv(t *testing.T, mod string) string {
	t.Helper()
	for _, pkg := range []string{"dep", "user"} {
		if err := os.MkdirAll(filepath.Join(mod, pkg), 0o750); err != nil {
			t.Fatal(err)
		}
		for name, src := range readGoFiles(t, filepath.Join("testdata", "src", "xconv", pkg)) {
			if err := os.WriteFile(filepath.Join(mod, pkg, name), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module xconv\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return mod
}

// TestVetToolSameModulePartnerFollows runs betteralign as go vet's vettool,
// where each package is analyzed in its own process and every import comes
// from export data (gcimporter's fake token.File). Treating that as "not
// analyzed" pinned user.T while dep's own vet process reordered dep.U, and
// the fixed module no longer built.
func TestVetToolSameModulePartnerFollows(t *testing.T) {
	skipUnless64Bit(t)
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin, mod := xconvModule(t)
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GOFLAGS=") })
	env = append(env, "GOWORK=off")

	// -apply is refused: dep's process rewrote dep.go before user's process
	// predicted from it, so user pinned its side and the build broke.
	pristine := map[string]string{}
	for _, pkg := range []string{"dep", "user"} {
		for name, src := range readGoFiles(t, filepath.Join(mod, pkg)) {
			pristine[filepath.Join(pkg, name)] = src
		}
	}
	apply := exec.Command("go", "vet", "-vettool="+bin, "-apply", "./...")
	apply.Dir, apply.Env = mod, env
	var stderr bytes.Buffer
	apply.Stderr = &stderr
	if err := apply.Run(); err == nil {
		t.Errorf("go vet -apply exited 0; want an error\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "-apply is not supported under go vet") {
		t.Errorf("go vet -apply stderr lacks the refusal:\n%s", stderr.String())
	}
	for f, src := range pristine {
		if got, err := os.ReadFile(filepath.Join(mod, f)); err != nil || string(got) != src {
			t.Errorf("go vet -apply changed %s (err %v)", f, err)
		}
	}

	vet := exec.Command("go", "vet", "-vettool="+bin, "-fix", "./...")
	vet.Dir, vet.Env = mod, env
	out, _ := vet.CombinedOutput() // exits non-zero when it reports
	build := exec.Command("go", "build", "./...")
	build.Dir, build.Env = mod, env
	if bout, err := build.CombinedOutput(); err != nil {
		t.Fatalf("module no longer builds after go vet -fix: %v\n%s\nvet output:\n%s", err, bout, out)
	}
	reordered := regexp.MustCompile(`type (T|U) struct \{[^\n]*\n\tB int64\n\tA bool\n\tC bool\n`)
	for _, f := range []string{"user/user.go", "dep/dep.go"} {
		src, err := os.ReadFile(filepath.Join(mod, f))
		if err != nil {
			t.Fatal(err)
		}
		if !reordered.Match(src) {
			t.Errorf("%s: struct not reordered in step:\n%s\nvet output:\n%s", f, src, out)
		}
	}

	// -fix -generated_files used to take the -apply path, which go vet
	// refuses with an error unitchecker never reads under -fix: no output,
	// exit 0, nothing written. It must act as plain -fix (the driver skips
	// the generated file) and say that the flag had no effect.
	gmod := copyXconv(t, filepath.Join(t.TempDir(), "xconv"))
	const gen = "// Code generated by hand. DO NOT EDIT.\n\npackage dep\n\ntype Gen struct {\n\tA bool\n\tB int64\n\tC bool\n}\n"
	if err := os.WriteFile(filepath.Join(gmod, "dep", "gen.go"), []byte(gen), 0o644); err != nil {
		t.Fatal(err)
	}
	gvet := exec.Command("go", "vet", "-vettool="+bin, "-fix", "-generated_files", "./...")
	gvet.Dir, gvet.Env = gmod, env
	stderr.Reset()
	gvet.Stderr = &stderr
	if err := gvet.Run(); err != nil {
		t.Errorf("go vet -fix -generated_files: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "-generated_files has no effect under go vet -fix") {
		t.Errorf("go vet -fix -generated_files stderr lacks the note:\n%s", stderr.String())
	}
	if got, err := os.ReadFile(filepath.Join(gmod, "dep", "gen.go")); err != nil || string(got) != gen {
		t.Errorf("go vet -fix -generated_files changed the generated file (err %v):\n%s", err, got)
	}
	for _, f := range []string{"user/user.go", "dep/dep.go"} {
		src, err := os.ReadFile(filepath.Join(gmod, f))
		if err != nil {
			t.Fatal(err)
		}
		if !reordered.Match(src) {
			t.Errorf("%s: not fixed under go vet -fix -generated_files:\n%s\nstderr:\n%s", f, src, stderr.String())
		}
	}
}
