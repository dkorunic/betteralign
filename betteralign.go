// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//
// Forked and modified by Dinko Korunic, 2022
//
// SPDX-FileCopyrightText: Copyright 2020 The Go Authors
// SPDX-FileCopyrightText: Copyright 2022 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

// Package betteralign defines an Analyzer that detects structs that would use less
// memory if their fields were sorted.

// Fork of fieldalignment (https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/fieldalignment)
// using a DST (Decorated Syntax Tree) so comments and spacing survive a
// rewrite. The alignment math is largely unchanged from fieldalignment/maligned;
// apply mode reprints the whole file because DST can't serialise a single node,
// and -fix acts as an alias for -apply since SuggestedFixes are unused.
//
// Related AST issue:
//   - https://github.com/golang/go/issues/20744
//
// Related Go/AST CL:
//   - https://go-review.googlesource.com/c/go/+/429639
//
// Original struct alignment projects:
//  - https://github.com/mdempsky/maligned
//  - https://github.com/orijtech/structslop

package betteralign

import (
	"bytes"
	"cmp"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"go/version"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	dst "github.com/dkorunic/betteralign/internal/dstmin"
	"github.com/google/renameio/v2/maybe"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

const Doc = `find structs that would use less memory if their fields were sorted

This analyzer find structs that can be rearranged to use less memory, and provides
a suggested edit with the most compact order.

Note that there are two different diagnostics reported. One checks struct size,
and the other reports "pointer bytes" used. Pointer bytes is how many bytes of the
object that the garbage collector has to potentially scan for pointers, for example:

	struct { uint32; string }

have 16 pointer bytes because the garbage collector has to scan up through the string's
inner pointer.

	struct { string; *uint32 }

has 24 pointer bytes because it has to scan further through the *uint32.

	struct { string; uint32 }

has 8 because the pointer bytes end immediately after the string pointer.

This "pointer bytes" figure is the type's ptrdata. On Go before 1.26 it was
also the number of bytes the collector scanned per value; under the Green Tea
GC (default in Go 1.26) marking is span-based rather than per-value, so a
smaller ptrdata lowers the ptrdata-based scan-work estimate the runtime uses
for GC pacing rather than the bytes physically scanned. betteralign frames the
pointer-bytes diagnostic to match the Go version of the analyzed package.

Be aware that the most compact order is not always the most efficient.
In rare cases it may cause two variables each updated by its own goroutine
to occupy the same CPU cache line, inducing a form of memory contention
known as "false sharing" that slows down both goroutines.

Unlike most analyzers, which report likely mistakes, the diagnostics
produced by betteralign very rarely indicate a significant problem,
so the analyzer is not included in typical suites such as vet or
gopls. Use this standalone command to run it on your code:

   $ go install github.com/dkorunic/betteralign/cmd/betteralign@latest
   $ betteralign [packages]

Only named struct types declared with the ` + "`type T struct { ... }`" + ` form
are analyzed. Anonymous structs (nested struct-typed fields, struct literals,
` + "`var x struct{...}`" + ` declarations, and similar unnamed forms) are
skipped. To enable analysis of one of those, lift it into a named type
declaration.

A generic struct whose layout depends on a type parameter — one with a type
parameter in a by-value field position (` + "`B T`" + `, ` + "`B [4]T`" + `, or a by-value
struct holding one) — is also skipped. Such a field is sized from the
constraint's underlying (` + "`T any`" + ` as ` + "`interface{}`" + `), so its size and
pointer-bytes figures would be per-instantiation guesses. Fields that hold a
type parameter only indirectly (` + "`*T`" + `, ` + "`[]T`" + `, ` + "`map[K]T`" + `) have a fixed
layout and are still analyzed.

If a struct is constructed somewhere in the package with a positional
composite literal (` + "`T{1, 2, 3}`" + ` rather than ` + "`T{a: 1, b: 2, c: 3}`" + `),
the reorder is reported but never applied: rewriting the field order would
re-map the literal's elements to different fields, breaking the build (or
worse, silently mis-assigning values when the new field types still happen
to accept the old element types). Convert the literal to keyed form and
rerun to enable the reorder.

This positional-literal guard only sees the package that declares the struct
and its in-package tests. A positional literal of an exported struct used from
another package (an ordinary importer, or an external ` + "`package p_test`" + `) is
invisible to the pass that rewrites the struct, so ` + "`-apply`" + ` may reorder it and
break — or silently mis-assign — that caller. Unexported structs are safe. When
running ` + "`-apply ./...`" + ` across a multi-package module, gate it with a
` + "`go build ./...`" + ` (and your tests) afterwards, or convert positional literals
to keyed form first. The in-package test guard also relies on the test files
being loaded, so a driver run with -test=false can still rewrite a struct
pinned by a literal in an in-package test.

Windows-line-ending (CRLF) files are reported but never rewritten under
` + "`-apply`" + `; normalise them to LF to enable the reorder.

`

const (
	ignoreStruct = "betteralign:ignore"
	optInStruct  = "betteralign:check"
)

// structKeywordLen sizes the diagnostic span over the `struct` keyword.
const structKeywordLen = token.Pos(len("struct"))

var (
	// default test and generated suffixes
	testSuffixes      = []string{"_test.go"}
	generatedSuffixes = []string{"_generated.go", "_gen.go", ".gen.go", ".pb.go", ".pb.gw.go"}

	// errors
	ErrStatFile       = errors.New("unable to stat the file")
	ErrNotRegularFile = errors.New("not a regular file, skipping")
	ErrWriteFile      = errors.New("unable to write to file")
	ErrEmptyBuffer    = errors.New("refusing to write empty buffer")
	ErrPreFilterFiles = errors.New("failed to pre-filter files")
)

// analyzerConfig holds per-analyzer flag state; one per InitAnalyzer call.
type analyzerConfig struct {
	excludeFiles   StringArrayFlag
	excludeDirs    StringArrayFlag
	apply          bool
	testFiles      bool
	generatedFiles bool
	optInMode      bool
}

// StringArrayFlag accumulates comma-separated values across repeated flag occurrences; backs -exclude_dirs / -exclude_files.
type StringArrayFlag []string

// String satisfies flag.Value with a human-readable rendering; not meant to
// round-trip through Set.
func (f *StringArrayFlag) String() string {
	return fmt.Sprintf("%v", *f)
}

// Set splits comma-separated values onto the accumulator, matching the original
// fieldalignment CLI (`a,b,c` is equivalent to three repeats). Empty entries
// are dropped to avoid spurious "" matches. Never errors. Not concurrency-safe
// — run clones the flag slices at start.
func (f *StringArrayFlag) Set(value string) error {
	for v := range strings.SplitSeq(value, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		*f = append(*f, v)
	}
	return nil
}

var Analyzer = &analysis.Analyzer{
	Name:     "betteralign",
	Doc:      Doc,
	Requires: []*analysis.Analyzer{inspect.Analyzer},
}

// InitAnalyzer attaches a fresh flag set and Run hook so each Analyzer owns
// independent flag state (the original fieldalignment used package-level flags
// that collided when several analyzers ran in one process, e.g. under
// golangci-lint). Exported for external drivers that build their own Analyzer.
//
// Idempotent: re-init is a no-op rather than a duplicate-flag os.Exit(2).
// Concurrency-safe across distinct Analyzers, not on the same one.
func InitAnalyzer(analyzer *analysis.Analyzer) {
	if analyzer.Flags.Lookup("apply") != nil {
		return
	}
	cfg := &analyzerConfig{}
	analyzer.Flags.BoolVar(&cfg.apply, "apply", false, "apply suggested fixes")
	analyzer.Flags.BoolVar(&cfg.testFiles, "test_files", false, "also check and fix test files")
	analyzer.Flags.BoolVar(&cfg.generatedFiles, "generated_files", false, "also check and fix generated files")
	analyzer.Flags.BoolVar(&cfg.optInMode, "opt_in", false, fmt.Sprintf("opt-in mode on per-struct basis with '%s' in comment", optInStruct))
	analyzer.Flags.Var(&cfg.excludeFiles, "exclude_files", "exclude files matching a pattern")
	analyzer.Flags.Var(&cfg.excludeDirs, "exclude_dirs", "exclude directories matching a pattern")
	analyzer.Run = cfg.run
}

// init wires the package-level Analyzer so direct importers of
// betteralign.Analyzer get the flag set without calling InitAnalyzer.
func init() {
	InitAnalyzer(Analyzer)
}

// run is the per-package analyzer entry point. Reporting is decoupled from
// rewriting: diagnostics come from AST + type info alone, so report-only mode
// never reads source bytes or builds DST. DST decoration is lazy and gated by
// -apply (and its legacy alias -fix); a struct dstmin can't decorate is still
// reported, just not rewritten. SuggestedFixes stay nil because DST can't
// serialise a single node without losing comments — the rewrite emits whole
// files (see package doc).
//
// Returns a non-nil error (wrapping ErrPreFilterFiles) only when exclude-path
// normalisation fails; per-file decorate/format/write failures are logged and
// skipped. Safe across concurrent passes; flag slices are cloned because hosts
// like golangci-lint may share flag state.
func (cfg *analyzerConfig) run(pass *analysis.Pass) (any, error) {
	// Hand the whole package to the [pkg.test] variant when it exists: the base
	// pass is blind to in-package test files, so it could rewrite a struct a
	// test's positional literal pins, and would emit a duplicate, caveat-less
	// diagnostic. The variant sees a superset of files, so nothing is lost.
	if deferToTestVariant(pass) {
		return nil, nil
	}

	apply := cfg.apply
	if fixRequested(pass) {
		apply = true
	}
	testFiles := cfg.testFiles
	generatedFiles := cfg.generatedFiles
	optInMode := cfg.optInMode
	// Defensive clones: golangci-lint and other hosts may run analyzers in
	// parallel while StringArrayFlag.Set could be appending concurrently.
	excludeDirs := slices.Clone(cfg.excludeDirs)
	excludeFiles := slices.Clone(cfg.excludeFiles)

	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	// Built lazily so clean packages skip the composite-literal walk.
	var positionalUsers map[*types.Struct]token.Pos
	ensurePositionalUsers := func() map[*types.Struct]token.Pos {
		if positionalUsers == nil {
			positionalUsers = collectPositionalUsers(inspect, pass)
		}
		return positionalUsers
	}

	// Lazy: nil until the first misaligned struct.
	var dec *dst.Decorator

	dirtyFiles := make(map[string]*dst.File)
	decoratedFiles := make(map[string]*dst.File)
	decorationFailed := make(map[string]struct{})
	// Populated from *ast.GenDecl visits, consulted from *ast.StructType visits.
	structOptIn := make(map[token.Pos]bool)

	var wd string
	if len(excludeDirs) > 0 || len(excludeFiles) > 0 {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrPreFilterFiles, err)
		}
		excludeDirs, err = normalizeExcludePaths(wd, excludeDirs)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrPreFilterFiles, err)
		}
		excludeFiles, err = normalizeExcludePaths(wd, excludeFiles)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrPreFilterFiles, err)
		}
	}

	// Fall back to 64-bit defaults if the unsafe.Pointer lookup ever fails.
	var wordSize, maxAlign int64 = 8, 8
	if unsafePointerTyp != nil {
		wordSize = pass.TypesSizes.Sizeof(unsafePointerTyp)
		maxAlign = pass.TypesSizes.Alignof(unsafePointerTyp)
	}
	sizes := newGCSizes(wordSize, maxAlign)

	// Per-file state set on *ast.File visit; reused by child node visits.
	var (
		currentFn      string
		currentASTFile *ast.File
		currentSkip    bool
	)

	inspect.Preorder(nodeFilter, func(node ast.Node) {
		if f, ok := node.(*ast.File); ok {
			// Defensive: File is nil only for a stray AST not in the FileSet;
			// skip rather than nil-deref .Name().
			tf := pass.Fset.File(f.Pos())
			if tf == nil {
				currentSkip = true
				return
			}
			currentFn = tf.Name()
			currentASTFile = f
			currentSkip = false

			if !testFiles && hasSuffix(currentFn, testSuffixes) {
				currentSkip = true
				return
			}
			if !generatedFiles && hasSuffix(currentFn, generatedSuffixes) {
				currentSkip = true
				return
			}
			if wd != "" && isExcluded(wd, currentFn, excludeDirs, excludeFiles) {
				currentSkip = true
				return
			}
			if !generatedFiles && hasGeneratedComment(f) {
				currentSkip = true
			}
			return
		}

		if currentSkip {
			return
		}

		if g, ok := node.(*ast.GenDecl); ok {
			if g.Tok == token.TYPE {
				groupOptedIn := declaredWithOptInComment(g)
				for _, spec := range g.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if st, ok := ts.Type.(*ast.StructType); ok {
						structOptIn[st.Pos()] = groupOptedIn || commentGroupHasOptIn(ts.Doc)
					}
				}
			}

			return
		}

		s, ok := node.(*ast.StructType)
		if !ok {
			return
		}

		// Only named type declarations carry an opt-in entry.
		optedIn, found := structOptIn[s.Pos()]
		if !found {
			return
		}

		// In opt-in mode, skip structs lacking the opt-in directive.
		if optInMode && !optedIn {
			return
		}

		tv, ok := pass.TypesInfo.Types[s]
		if !ok {
			return
		}
		typ, ok := tv.Type.(*types.Struct)
		if !ok {
			return
		}

		// Compute optimality without DST and without allocating a permutation;
		// clean files pay no decoration cost and the hot path stays zero-alloc.
		optimal, optsz, optptrs := layoutMetrics(typ, sizes)
		if optimal {
			return
		}

		// Skip structs whose layout depends on a type parameter: sized from the
		// constraint underlying, their figures are per-instantiation guesses.
		// On the misaligned path only, so non-generic structs pay nothing.
		if layoutDependsOnTypeParam(typ) {
			return
		}

		var message string
		if sz := sizes.Sizeof(typ); sz != optsz {
			message = fmt.Sprintf("%d bytes saved: struct of size %d could be %d", sz-optsz, sz, optsz)
		} else if ptrs := sizes.ptrdata(typ); ptrs != optptrs {
			// Frame the pointer-bytes win to the analyzed project's Go version:
			// under Green Tea it is a scan-work estimate, not bytes saved.
			if greenTeaGC(targetGoVersion(pass, currentASTFile)) {
				message = fmt.Sprintf("struct with %d pointer bytes could be %d (lowers GC scan-work estimate)", ptrs, optptrs)
			} else {
				message = fmt.Sprintf("%d bytes saved: struct with %d pointer bytes could be %d", ptrs-optptrs, ptrs, optptrs)
			}
		} else {
			return
		}

		// AST-based so ignore works even for shapes dstmin can't decorate.
		if hasIgnoreCommentAST(pass.Fset, currentASTFile, s) {
			return
		}

		// Positional literal pins layout: safe to report, unsafe to mutate.
		if litPos, blocked := ensurePositionalUsers()[typ]; blocked {
			pass.Report(analysis.Diagnostic{
				Pos: s.Pos(),
				End: s.Pos() + structKeywordLen,
				Message: fmt.Sprintf(
					"%s; reorder skipped: positional composite literal at %s would break, convert to keyed form first",
					message, pass.Fset.Position(litPos),
				),
				SuggestedFixes: nil,
			})
			return
		}

		pass.Report(analysis.Diagnostic{
			Pos:            s.Pos(),
			End:            s.Pos() + structKeywordLen,
			Message:        message,
			SuggestedFixes: nil,
		})

		// Rewrite path: only -apply runs it, and only it needs DST.
		if !apply {
			return
		}

		// Decorate this file once; never retry on failure. Checked before
		// optimalOrder so a file that already failed decoration doesn't pay the
		// sort + permutation alloc for its remaining misaligned structs.
		if _, failed := decorationFailed[currentFn]; failed {
			return
		}

		// Permutation needed only for the reorder; computed lazily per misaligned
		// struct so report-only runs never allocate it.
		indexes, _, _ := optimalOrder(typ, sizes)

		if dec == nil {
			dec = dst.NewDecorator(pass.Fset)
		}
		dFile, ok := decoratedFiles[currentFn]
		if !ok {
			df, err := dec.DecorateFile(currentASTFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to decorate %s: %v\n", currentFn, err)
				decorationFailed[currentFn] = struct{}{}
				return
			}
			decoratedFiles[currentFn] = df
			dFile = df
		}

		dNode, ok := dec.Dst.Nodes[s].(*dst.StructType)
		if !ok {
			// Shape dstmin can't rewrite; already reported, so skip.
			return
		}

		// Flatten multi-name fields (A, B int) into one slot per name.
		// TODO: preserve multi-named fields instead of flattening.
		flatPtr := fieldSlicePool.Get().(*[]*dst.Field)
		flat := (*flatPtr)[:0]
		defer func() {
			// Drop pointer refs so the pool can't pin DST nodes.
			clear(flat)
			*flatPtr = flat[:0]
			fieldSlicePool.Put(flatPtr)
		}()
		for _, fld := range dNode.Fields.List {
			flat = append(flat, fld)
			if len(fld.Names) == 0 {
				continue
			}
			for range fld.Names[1:] {
				flat = append(flat, dummyField)
			}
		}

		// Guard malformed type info: mismatched lengths would panic at flat[idx].
		if len(flat) != len(indexes) {
			return
		}
		reordered := make([]*dst.Field, 0, len(dNode.Fields.List))
		for _, idx := range indexes {
			fld := flat[idx]
			if fld == dummyField {
				continue
			}
			reordered = append(reordered, fld)
		}
		dNode.Fields.List = reordered
		dirtyFiles[currentFn] = dFile
	})

	if apply {
		// Sort so partial-failure ordering is reproducible across runs.
		fns := make([]string, 0, len(dirtyFiles))
		for fn := range dirtyFiles {
			fns = append(fns, fn)
		}
		slices.Sort(fns)
		// Reuse buffer across writes to amortize the backing byte slice.
		var buf bytes.Buffer
		for _, fn := range fns {
			buf.Reset()
			df := dirtyFiles[fn]
			// Fprint already validates its output parses; no second parse needed.
			if err := dst.Fprint(&buf, df); err != nil {
				fmt.Fprintf(os.Stderr, "failed to print %s: %v\n", fn, err)
				continue
			}
			if err := applyToFile(fn, buf.Bytes()); err != nil {
				fmt.Fprintf(os.Stderr, "error applying fixes to %v: %v\n", fn, err)
			}
		}
	}

	return nil, nil
}

// fixRequested reports whether the -fix alias of -apply is active. Both flag
// sets matter: x/tools drivers register -fix on the process-global set (via
// analysisflags), while hosts and the test harness wire it onto Analyzer.Flags.
// Checking only Analyzer.Flags makes -fix a silent no-op in the shipped binary,
// since the driver then applies the (always nil) SuggestedFixes instead of
// reporting diagnostics.
func fixRequested(pass *analysis.Pass) bool {
	if a := pass.Analyzer.Flags.Lookup("fix"); a != nil && a.Value.String() == "true" {
		return true
	}
	if a := flag.Lookup("fix"); a != nil && a.Value.String() == "true" {
		return true
	}
	return false
}

// unsafePointerTyp anchors word-size and pointer-alignment lookups via pass.TypesSizes.
// Wrapped to avoid an init-time panic if unsafe's shape ever changes; gcSizes falls back to 8.
var unsafePointerTyp = lookupUnsafePointer()

// nodeFilter is the visitor's node set; package-level so passes share one slice.
var nodeFilter = []ast.Node{
	(*ast.File)(nil),
	(*ast.StructType)(nil),
	(*ast.GenDecl)(nil),
}

// dummyField is a skip-position sentinel for the apply-mode reorder when one dst.Field
// declares multiple names (`A, B int`); pointer-identity, package-level to avoid allocation.
var dummyField = &dst.Field{}

// fieldElem is optimalOrder's per-field sort record; lifted out so a pool can recycle it.
type fieldElem struct {
	alignof int64
	sizeof  int64
	ptrdata int64
	index   int
}

// elemsPool recycles fieldElem scratch buffers for optimalOrder.
// Holds *[]T to avoid boxing slice headers via any (SA6002).
var elemsPool = sync.Pool{
	New: func() any { s := []fieldElem(nil); return &s },
}

// fieldSlicePool recycles the apply-mode flatten buffer; reordered is freshly allocated.
var fieldSlicePool = sync.Pool{
	New: func() any { s := []*dst.Field(nil); return &s },
}

// collectPositionalUsers maps each *types.Struct pinned by a positional
// composite literal (`T{1, 2, 3}`, not keyed) to its first such literal's
// position. Reordering those structs would re-map literal elements to other
// fields, so the diagnostic path consults this set and surfaces the position as
// a jump target.
//
// Per-file skip flags deliberately do not apply: an excluded file still
// compiles against the layout, so its positional literal would break just the
// same. Elts[0] classifies the whole literal (Go forbids mixing keyed and
// positional). Returns a fresh map; one per pass.
func collectPositionalUsers(inspect *inspector.Inspector, pass *analysis.Pass) map[*types.Struct]token.Pos {
	users := make(map[*types.Struct]token.Pos)

	inspect.Preorder([]ast.Node{(*ast.CompositeLit)(nil)}, func(node ast.Node) {
		lit := node.(*ast.CompositeLit)
		if len(lit.Elts) == 0 {
			return
		}
		if _, keyed := lit.Elts[0].(*ast.KeyValueExpr); keyed {
			return
		}
		tv, ok := pass.TypesInfo.Types[lit]
		if !ok || tv.Type == nil {
			return
		}
		st := canonicalStructType(tv.Type)
		if st == nil {
			return
		}
		if _, seen := users[st]; !seen {
			users[st] = lit.Pos()
		}
	})

	return users
}

// deferToTestVariant reports whether this pass should hand the whole package to
// the in-package test variant, skipping both reporting and -apply.
//
// The base pass can't see in-package *_test.go files, so a positional literal
// there is invisible: it would rewrite the pinned struct (breaking the test)
// and duplicate the variant's diagnostic without its caveat. The [p.test]
// variant sees those tests plus the same non-test files, so deferring loses
// nothing.
//
// Keys on loaded files, not a disk glob, so a build-excluded test — which never
// compiles and can't break — doesn't block the fix. External p_test packages
// load separately and never rewrite p (the documented cross-package limitation).
func deferToTestVariant(pass *analysis.Pass) bool {
	dir := ""
	seen := make(map[string]struct{}, len(pass.Files))
	for _, f := range pass.Files {
		tf := pass.Fset.File(f.Pos())
		if tf == nil {
			continue
		}
		seen[tf.Name()] = struct{}{}
		if dir == "" {
			dir = filepath.Dir(tf.Name())
		}
	}
	if dir == "" {
		return false
	}

	for _, e := range testFilesByDir(pass.Fset)[dir] {
		if _, ok := seen[e.name]; ok {
			continue
		}
		// pkgName tells in-package tests from external p_test ones.
		if e.pkgName == pass.Pkg.Name() {
			return true
		}
	}
	return false
}

// testFileEntry is a loaded *_test.go and its package clause, letting
// deferToTestVariant skip the external-p_test ones without re-parsing per pass.
type testFileEntry struct {
	name    string
	pkgName string
}

// testVariantCache memoises the per-FileSet *_test.go scan (keyed by
// *token.FileSet, value map[dir][]testFileEntry): deferToTestVariant runs on
// every pass over the shared FileSet, so the walk must not repeat per package.
// Entries live for the process lifetime, which suits the CLI and golangci-lint.
var testVariantCache sync.Map // map[*token.FileSet]map[string][]testFileEntry

// testFilesByDir groups fset's *_test.go files by directory, building the map
// once. The FileSet is fully populated before analysis, so the scan is a safe
// concurrent read; a racing double-build stores one deterministic result
// (LoadOrStore, as in readSourceOnce). Unparsable files are dropped.
func testFilesByDir(fset *token.FileSet) map[string][]testFileEntry {
	if v, ok := testVariantCache.Load(fset); ok {
		return v.(map[string][]testFileEntry)
	}
	byDir := make(map[string][]testFileEntry)
	fset.Iterate(func(tf *token.File) bool {
		name := tf.Name()
		if !strings.HasSuffix(name, "_test.go") {
			return true
		}
		pf, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.PackageClauseOnly)
		if err != nil || pf.Name == nil {
			return true
		}
		dir := filepath.Dir(name)
		byDir[dir] = append(byDir[dir], testFileEntry{name: name, pkgName: pf.Name.Name})
		return true
	})
	actual, _ := testVariantCache.LoadOrStore(fset, byDir)
	return actual.(map[string][]testFileEntry)
}

// canonicalStructType collapses every syntactic form of a struct reference onto
// the single *types.Struct the visitor sees at the *ast.StructType, so both key
// against identical pointers. Returns nil for non-structs.
//
// Collapses named structs, chained type defs, generic instantiations, and
// pointer-elided literals; aliases are stripped via types.Unalias. The
// *types.Pointer arm is load-bearing: in `[]*T{{...}}` the type-checker records
// the inner literal as *T, and without the unwrap the analyzer would reorder T
// and break the build.
func canonicalStructType(t types.Type) *types.Struct {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	switch tt := t.(type) {
	case *types.Named:
		st, _ := tt.Origin().Underlying().(*types.Struct)
		return st
	case *types.Struct:
		return tt
	}
	return nil
}

// lookupUnsafePointer resolves unsafe.Pointer's types.Type so size lookups
// probe the target architecture instead of hard-coding 64-bit. Returns nil only
// if unsafe.Pointer ever disappears from the package scope; run then falls back
// to 8-byte defaults. Memoised in unsafePointerTyp.
func lookupUnsafePointer() types.Type {
	obj := types.Unsafe.Scope().Lookup("Pointer")
	if obj == nil {
		return nil
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return nil
	}
	return tn.Type()
}

// targetGoVersion reports the analyzed source's effective Go version, a file's
// //go:build go1.x constraint taking precedence over the module's go.mod. Empty
// when unknown (e.g. GOPATH-mode packages), which callers treat as pre-1.26.
func targetGoVersion(pass *analysis.Pass, file *ast.File) string {
	if file != nil && file.GoVersion != "" {
		return file.GoVersion
	}
	if pass.Pkg != nil {
		return pass.Pkg.GoVersion()
	}
	return ""
}

// greenTeaGC reports whether the analyzed package targets Go 1.26+, where the
// Green Tea GC's span-based marking makes a smaller ptrdata a scan-work pacing
// win rather than fewer bytes scanned. Invalid/unknown versions count as
// pre-1.26 — hence the IsValid guard, since Compare reads invalid input as 0.
func greenTeaGC(v string) bool {
	return version.IsValid(v) && version.Compare(v, "go1.26") >= 0
}

// compareFieldElem is the alignment-first / GC-ptrdata-minimising comparator
// shared by layoutMetrics and optimalOrder. The stable sort keeps equal-key
// fields in source order, so the identity permutation means "already optimal".
//
// Sort order, in priority:
//
//  1. zero-sized fields first (they collapse alignment requirements);
//  2. higher alignment first (fills natural-alignment slots);
//  3. pointer-bearing fields before pointer-free (front-loads pointers,
//     shrinking ptrdata);
//  4. fewer trailing non-pointer bytes among pointerful fields (minimises
//     ptrdata, the GC's scan-work pacing estimate);
//  5. larger size first as the final tiebreaker.
func compareFieldElem(a, b fieldElem) int {
	// Zero-sized fields before non-zero-sized.
	if (a.sizeof == 0) != (b.sizeof == 0) {
		if a.sizeof == 0 {
			return -1
		}
		return 1
	}
	// Higher alignment first.
	if c := cmp.Compare(b.alignof, a.alignof); c != 0 {
		return c
	}
	// Pointerful fields before pointer-free.
	if (a.ptrdata == 0) != (b.ptrdata == 0) {
		if a.ptrdata != 0 {
			return -1
		}
		return 1
	}
	// Among pointerful fields, fewer trailing non-pointer bytes first.
	if a.ptrdata != 0 {
		if c := cmp.Compare(a.sizeof-a.ptrdata, b.sizeof-b.ptrdata); c != 0 {
			return c
		}
	}
	// Larger size first (final tiebreaker).
	return cmp.Compare(b.sizeof, a.sizeof)
}

// fillElems records one fieldElem per struct field in source order from the
// memoised sizes. len(elems) must equal str.NumFields().
func fillElems(str *types.Struct, elems []fieldElem, sizes *gcSizes) {
	for i := range elems {
		ft := str.Field(i).Type()
		elems[i] = fieldElem{
			alignof: sizes.Alignof(ft),
			sizeof:  sizes.Sizeof(ft),
			ptrdata: sizes.ptrdata(ft),
			index:   i,
		}
	}
}

// elemsIdentity reports whether sorted elems are still in source order, i.e.
// the original layout is already optimal (the stable sort guarantees this iff
// no reordering improves it).
func elemsIdentity(elems []fieldElem) bool {
	for i := range elems {
		if elems[i].index != i {
			return false
		}
	}
	return true
}

// measureLayout computes the size and ptrdata that result from laying fields
// out in elems order. nf is len(elems); a trailing zero-sized field on a
// non-empty struct gets one padding byte so its address can't alias the next
// allocation. All arithmetic is overflow-saturated via align/addSize.
func measureLayout(elems []fieldElem) (optSize, optPtrdata int64) {
	nf := len(elems)
	var offset int64
	maxAlign := int64(1)
	for i, e := range elems {
		if e.alignof > maxAlign {
			maxAlign = e.alignof
		}
		offset = align(offset, e.alignof)
		if e.ptrdata != 0 {
			optPtrdata = addSize(offset, e.ptrdata)
		}
		sz := e.sizeof
		if i == nf-1 && sz == 0 && offset != 0 {
			sz = 1
		}
		offset = addSize(offset, sz)
	}
	optSize = align(offset, maxAlign)
	return optSize, optPtrdata
}

// layoutMetrics is the report-mode hot path: it reports whether str is already
// optimally laid out and, if not, the optimal size and ptrdata — without
// allocating a permutation (that's only needed by -apply; see optimalOrder).
// Report-only runs dominate, so keeping the alloc off this path makes the
// already-optimal case zero-allocation (see BenchmarkDecisionPath_Optimal_FastPath).
// Optimal returns (true, 0, 0) and skips the layout walk. One *gcSizes per
// goroutine — caches are not concurrency-safe.
func layoutMetrics(str *types.Struct, sizes *gcSizes) (optimal bool, optSize, optPtrdata int64) {
	nf := str.NumFields()

	elemsPtr := elemsPool.Get().(*[]fieldElem)
	elems := *elemsPtr
	if cap(elems) < nf {
		elems = make([]fieldElem, nf)
	} else {
		elems = elems[:nf]
	}
	defer func() {
		*elemsPtr = elems[:0]
		elemsPool.Put(elemsPtr)
	}()

	fillElems(str, elems, sizes)
	slices.SortStableFunc(elems, compareFieldElem)
	if elemsIdentity(elems) {
		return true, 0, 0
	}
	optSize, optPtrdata = measureLayout(elems)
	return false, optSize, optPtrdata
}

// optimalOrder returns the compacting field permutation plus the resulting size
// and ptrdata. It is the -apply rewrite path; report-only runs use layoutMetrics
// and skip this allocation. indexes is always a full permutation of
// [0, NumFields) — including identity when already optimal — because the unit
// and fuzz tests assert its contents (notably stable-sort order on equal keys).
// One *gcSizes per goroutine.
func optimalOrder(str *types.Struct, sizes *gcSizes) (indexes []int, optSize, optPtrdata int64) {
	nf := str.NumFields()

	elemsPtr := elemsPool.Get().(*[]fieldElem)
	elems := *elemsPtr
	if cap(elems) < nf {
		elems = make([]fieldElem, nf)
	} else {
		elems = elems[:nf]
	}
	defer func() {
		*elemsPtr = elems[:0]
		elemsPool.Put(elemsPtr)
	}()

	fillElems(str, elems, sizes)
	slices.SortStableFunc(elems, compareFieldElem)

	indexes = make([]int, nf)
	for i := range elems {
		indexes[i] = elems[i].index
	}
	optSize, optPtrdata = measureLayout(elems)
	return indexes, optSize, optPtrdata
}

// layoutDependsOnTypeParam reports whether str's layout would differ across
// instantiations, i.e. a type parameter sits in a by-value field position. Such
// a field is sized from the constraint underlying (T any → interface{}), so its
// figures are per-instantiation guesses and run() suppresses the diagnostic.
// A type parameter behind a pointer/slice/map/chan/func/interface has a fixed
// layout and does not count.
func layoutDependsOnTypeParam(str *types.Struct) bool {
	return typeParamInLayout(str, make(map[types.Type]struct{}))
}

// typeParamInLayout is layoutDependsOnTypeParam's recursion. It matches a
// *types.TypeParam before calling Underlying() — which for a type parameter is
// its constraint's interface, not the parameter — and recurses only into arrays
// and structs (by-value holders). seen breaks cycles in recursive types.
func typeParamInLayout(t types.Type, seen map[types.Type]struct{}) bool {
	if t == nil {
		return false
	}
	if _, ok := types.Unalias(t).(*types.TypeParam); ok {
		return true
	}
	if _, ok := seen[t]; ok {
		return false
	}
	seen[t] = struct{}{}
	switch u := t.Underlying().(type) {
	case *types.Array:
		return typeParamInLayout(u.Elem(), seen)
	case *types.Struct:
		for i := range u.NumFields() {
			if typeParamInLayout(u.Field(i).Type(), seen) {
				return true
			}
		}
	}
	return false
}

// Code below based on go/types.StdSizes.

// gcSizes computes sizes, alignments, and ptrdata matching the Go GC's layout.
// Caches memoise per-type results to avoid recomputing recurring sub-types;
// nil caches are permitted so tests can construct via struct literal.
type gcSizes struct {
	sizeCache  map[types.Type]int64
	alignCache map[types.Type]int64
	ptrCache   map[types.Type]int64
	WordSize   int64
	MaxAlign   int64
}

// newGCSizes returns a *gcSizes with empty memoisation caches — required for
// bulk use (the analyzer pass) so recurring sub-types stay linear in unique
// types. Tests may instead use a nil-cache struct literal (the helpers degrade
// to uncached). Not concurrency-safe; one per goroutine.
func newGCSizes(wordSize, maxAlign int64) *gcSizes {
	return &gcSizes{
		WordSize:   wordSize,
		MaxAlign:   maxAlign,
		sizeCache:  make(map[types.Type]int64),
		alignCache: make(map[types.Type]int64),
		ptrCache:   make(map[types.Type]int64),
	}
}

// Alignof reports T's GC alignment in bytes, memoised. The sentinel write of 1
// before recursing terminates invalid self-referential types (BUG-29): the
// re-entry hits the cached 1, which is also a safe lower bound since the spec
// guarantees Alignof ≥ 1. Mutates s.alignCache; not concurrency-safe.
func (s *gcSizes) Alignof(T types.Type) int64 {
	if v, ok := s.alignCache[T]; ok {
		return v
	}
	if s.alignCache != nil {
		s.alignCache[T] = 1
	}
	v := s.alignofUncached(T)
	if s.alignCache != nil {
		s.alignCache[T] = v
	}
	return v
}

// alignofUncached is the algorithm behind Alignof, split out for nil-cache
// testing. Per spec: arrays align to their element, structs to the max field
// alignment, everything else to its size clamped to [1, MaxAlign].
func (s *gcSizes) alignofUncached(T types.Type) int64 {
	switch t := T.Underlying().(type) {
	case *types.Array:
		// spec: Alignof(array) == Alignof(element), ≥ 1.
		return s.Alignof(t.Elem())
	case *types.Struct:
		// spec: Alignof(struct) == max(Alignof(fields)), ≥ 1.
		max := int64(1)
		for i, nf := 0, t.NumFields(); i < nf; i++ {
			if a := s.Alignof(t.Field(i).Type()); a > max {
				max = a
			}
		}
		return max
	}
	a := s.Sizeof(T) // may be 0
	// gc aligns complex{64,128} to their float half, not their full size
	// (Alignof(complex64) is 4, not 8); mirrors go/types.StdSizes. Upstream
	// fieldalignment omits this and over-aligns, misreporting sizes.
	if b, ok := T.Underlying().(*types.Basic); ok && b.Info()&types.IsComplex != 0 {
		a /= 2
	}
	// spec: Alignof(any) ≥ 1.
	if a < 1 {
		return 1
	}
	if a > s.MaxAlign {
		return s.MaxAlign
	}
	return a
}

var basicSizes = [...]byte{
	types.Bool:       1,
	types.Int8:       1,
	types.Int16:      2,
	types.Int32:      4,
	types.Int64:      8,
	types.Uint8:      1,
	types.Uint16:     2,
	types.Uint32:     4,
	types.Uint64:     8,
	types.Float32:    4,
	types.Float64:    8,
	types.Complex64:  8,
	types.Complex128: 16,
}

// Sizeof reports T's size in bytes as the GC lays it out, memoised. The
// sentinel write of 0 before recursing terminates invalid self-referential
// types (BUG-29) by treating the re-entry as empty. Overflow saturates to
// math.MaxInt64 via addSize/mulSize. Not concurrency-safe.
func (s *gcSizes) Sizeof(T types.Type) int64 {
	if v, ok := s.sizeCache[T]; ok {
		return v
	}
	if s.sizeCache != nil {
		s.sizeCache[T] = 0
	}
	v := s.sizeofUncached(T)
	if s.sizeCache != nil {
		s.sizeCache[T] = v
	}
	return v
}

// sizeofUncached is the algorithm behind Sizeof (split for nil-cache testing).
// Basic kinds via basicSizes; string/interface are 2 words, slices 3, arrays
// multiply (saturating). Structs walk fields applying alignment, with the
// runtime's trailing-zero-field rule: a zero-sized last field on a non-empty
// struct gets one padding byte so its address can't alias the next allocation.
// Unknown kinds fall back to one word.
func (s *gcSizes) sizeofUncached(T types.Type) int64 {
	switch t := T.Underlying().(type) {
	case *types.Basic:
		k := t.Kind()
		if int(k) < len(basicSizes) {
			if sz := basicSizes[k]; sz > 0 {
				return int64(sz)
			}
		}
		if k == types.String {
			return s.WordSize * 2
		}
	case *types.Array:
		return mulSize(t.Len(), s.Sizeof(t.Elem()))
	case *types.Slice:
		return s.WordSize * 3
	case *types.Struct:
		nf := t.NumFields()
		if nf == 0 {
			return 0
		}

		var o int64
		max := int64(1)
		for i := range nf {
			ft := t.Field(i).Type()
			a, sz := s.Alignof(ft), s.Sizeof(ft)
			if a > max {
				max = a
			}
			if i == nf-1 && sz == 0 && o != 0 {
				sz = 1
			}
			o = addSize(align(o, a), sz)
		}
		return align(o, max)
	case *types.Interface:
		return s.WordSize * 2
	}
	return s.WordSize // catch-all
}

// align rounds offset x up to alignment a, saturating at math.MaxInt64 instead
// of wrapping or panicking on fuzz inputs. Precondition: a > 0 (Alignof always
// returns ≥ 1).
func align(x, a int64) int64 {
	if x > math.MaxInt64-(a-1) {
		return math.MaxInt64
	}
	y := x + a - 1
	return y - y%a
}

// mulSize multiplies array length by element size, saturating at math.MaxInt64
// (BUG-30: a raw multiply wrapped negative on adversarial lengths, poisoning
// the "size ≥ 0" invariant). Non-positive inputs collapse to 0.
func mulSize(n, size int64) int64 {
	if n <= 0 || size <= 0 {
		return 0
	}
	if n > math.MaxInt64/size {
		return math.MaxInt64
	}
	return n * size
}

// addSize accumulates offsets, saturating at math.MaxInt64; pairs with align
// and mulSize to keep the layout walk overflow-safe end to end. Negative inputs
// collapse to 0.
func addSize(a, b int64) int64 {
	if a < 0 || b < 0 {
		return 0
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// ptrdata reports how many bytes from T's head the GC must scan for pointers;
// 0 means pointer-free. This is the value compareFieldElem minimises. Memoised;
// the sentinel write of 0 before recursing terminates BUG-29 self-referential
// types (treated as pointer-free). Not concurrency-safe.
func (s *gcSizes) ptrdata(T types.Type) int64 {
	if v, ok := s.ptrCache[T]; ok {
		return v
	}
	if s.ptrCache != nil {
		s.ptrCache[T] = 0
	}
	v := s.ptrdataUncached(T)
	if s.ptrCache != nil {
		s.ptrCache[T] = v
	}
	return v
}

// ptrdataUncached is the algorithm behind ptrdata. strings/unsafe.Pointer are
// one pointer word; chan/map/pointer/func/slice one word; interfaces two;
// arrays scale to the last pointer-bearing element; structs track the offset
// past the last pointer field. The default arm is a conservative one-word
// fallback: effectively unreachable, since Underlying() resolves named types,
// aliases, and type parameters first (a type parameter's Underlying() is its
// constraint's *types.Interface, caught above), but it over-reports rather than
// panicking on any future kind.
func (s *gcSizes) ptrdataUncached(T types.Type) int64 {
	switch t := T.Underlying().(type) {
	case *types.Basic:
		switch t.Kind() {
		case types.String, types.UnsafePointer:
			return s.WordSize
		}
		return 0
	case *types.Chan, *types.Map, *types.Pointer, *types.Signature, *types.Slice:
		return s.WordSize
	case *types.Interface:
		return 2 * s.WordSize
	case *types.Array:
		n := t.Len()
		if n <= 0 {
			return 0
		}
		a := s.ptrdata(t.Elem())
		if a == 0 {
			return 0
		}
		z := s.Sizeof(t.Elem())
		return addSize(mulSize(n-1, z), a)
	case *types.Struct:
		nf := t.NumFields()
		if nf == 0 {
			return 0
		}

		var o, p int64
		for i := range nf {
			ft := t.Field(i).Type()
			a, sz := s.Alignof(ft), s.Sizeof(ft)
			fp := s.ptrdata(ft)
			o = align(o, a)
			if fp != 0 {
				p = addSize(o, fp)
			}
			o = addSize(o, sz)
		}
		return p
	}

	// Conservative one-word fallback; see the doc comment on unreachability.
	return s.WordSize
}

// hasSuffix reports whether fn ends in any of suffixes; backs the test- and
// generated-file gates.
func hasSuffix(fn string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(fn, s) {
			return true
		}
	}
	return false
}

// hasGeneratedComment detects the canonical `Code generated by ... DO NOT
// EDIT.` header for generators that don't follow a naming convention (the
// suffix gate via hasSuffix catches the rest). Comments are position-sorted, so
// the scan stops at the package keyword (the header must precede it). Matching
// uses reGeneratedBy, a DFA, to skip the regexp engine's per-call setup.
func hasGeneratedComment(file *ast.File) bool {
	for _, cg := range file.Comments {
		if cg.Pos() > file.Package {
			return false
		}
		for _, l := range cg.List {
			if reGeneratedBy(l.Text) {
				return true
			}
		}
	}
	return false
}

// normalizeExcludePaths canonicalises exclude-flag entries for filepath.Match:
// absolute paths are made wd-relative (so they can match isExcluded's relative
// input) and all entries are cleaned. The all-canonical common case returns the
// input unchanged — treat the result as read-only.
//
// Returns an error (wrapped as ErrPreFilterFiles by the caller) when an
// absolute path can't be made relative to wd (cross-volume on Windows) or
// escapes wd with a `..` prefix, which filepath.Match can never match — fail
// loud, not silent.
func normalizeExcludePaths(wd string, paths []string) ([]string, error) {
	needsNorm := slices.ContainsFunc(paths, func(p string) bool {
		return filepath.IsAbs(p) || filepath.Clean(p) != p
	})
	if !needsNorm {
		return paths, nil
	}
	out := make([]string, len(paths))
	for i, p := range paths {
		if !filepath.IsAbs(p) {
			out[i] = filepath.Clean(p)
			continue
		}
		rel, err := filepath.Rel(wd, p)
		if err != nil {
			return nil, fmt.Errorf("exclude path %q is not resolvable relative to working directory %q (cross-volume on Windows, or requires runtime cwd): %w", p, wd, err)
		}
		// A "../"-prefixed rel never matches real paths under filepath.Match; fail loud not silent.
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("exclude path %q escapes working directory %q (resolved to %q); supply a path inside the working tree or use a relative pattern", p, wd, rel)
		}
		out[i] = rel
	}
	return out, nil
}

// isExcluded reports whether fn is excluded by the dir or file glob sets
// (filepath.Match). Each ancestor of fn's wd-relative path is tested by full
// path and, for separator-free patterns, by basename — so a bare name
// ("vendor", "internal") excludes that directory at any depth while a
// slash-bearing pattern ("a/internal") matches only that exact relative path. A
// Rel/Match error logs and returns true (fail toward excluded).
// O((ancestors × dirs) + files).
func isExcluded(wd, fn string, excludeDirs, excludeFiles []string) bool {
	relfn, err := filepath.Rel(wd, fn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v %s: %v\n", ErrPreFilterFiles, fn, err)
		return true
	}
	if len(excludeDirs) > 0 {
		ancestor := filepath.Dir(relfn)
		for {
			for _, excludeDir := range excludeDirs {
				// filepath.Match doesn't clean; covers callers that skip normalizeExcludePaths.
				pattern := filepath.Clean(excludeDir)
				match, err := filepath.Match(pattern, ancestor)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%v %s: %v\n", ErrPreFilterFiles, fn, err)
					return true
				}
				if match {
					return true
				}
				// Bare names also match by basename, excluding that dir at any depth.
				if !strings.ContainsRune(pattern, filepath.Separator) {
					match, err := filepath.Match(pattern, filepath.Base(ancestor))
					if err != nil {
						fmt.Fprintf(os.Stderr, "%v %s: %v\n", ErrPreFilterFiles, fn, err)
						return true
					}
					if match {
						return true
					}
				}
			}
			parent := filepath.Dir(ancestor)
			if parent == ancestor {
				break
			}
			ancestor = parent
		}
	}
	for _, excludeFile := range excludeFiles {
		match, err := filepath.Match(excludeFile, relfn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v %s: %v\n", ErrPreFilterFiles, fn, err)
			return true
		}
		if match {
			return true
		}
	}
	return false
}

// hasIgnoreCommentAST reports the betteralign:ignore opt-out. Working from the
// AST rather than DST honors the directive even for structs dstmin can't
// decorate. Only a //-line comment on the opening-brace line counts;
// recognition is shared with the opt-in check via commentHasDirective so the
// two can't drift.
func hasIgnoreCommentAST(fset *token.FileSet, file *ast.File, s *ast.StructType) bool {
	open := s.Fields.Opening
	closing := s.Fields.Closing
	openLine := fset.Position(open).Line

	// Binary-search to the first group past the opening brace rather than
	// rescanning from the top — otherwise the analyzer is O(misaligned structs ×
	// comments) (see BenchmarkHasIgnoreCommentAST_Scaling). Pos() is the raw byte
	// offset, monotonic even under //line directives. Mirrors dstmin.commentRun.
	comments := file.Comments
	lo := sort.Search(len(comments), func(i int) bool { return comments[i].Pos() > open })
	for _, cg := range comments[lo:] {
		if cg.Pos() >= closing {
			break
		}
		if fset.Position(cg.Pos()).Line != openLine {
			continue
		}
		for _, c := range cg.List {
			if commentHasDirective(c.Text, ignoreStruct) {
				return true
			}
		}
	}

	return false
}

// commentGroupHasOptIn reports the -opt_in directive within cg; nil cg means no
// opt-in (callers pass possibly-nil type-spec docs).
func commentGroupHasOptIn(cg *ast.CommentGroup) bool {
	if cg == nil {
		return false
	}
	for _, dc := range cg.List {
		if commentHasDirective(dc.Text, optInStruct) {
			return true
		}
	}
	return false
}

// commentHasDirective is the shared recogniser for betteralign:ignore and
// betteralign:check, keeping both consistent:
//
//   - //-line comments only; block comments are rejected (gofmt mishandles
//     trailing block comments inside structs — BUG-42).
//   - leading whitespace after // is tolerated.
//   - trailing word boundary required (`betteralign:ignored` does not match).
func commentHasDirective(comment, directive string) bool {
	body, ok := strings.CutPrefix(comment, "//")
	if !ok {
		return false
	}
	body = strings.TrimLeft(body, " \t")
	rest, ok := strings.CutPrefix(body, directive)
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	c := rest[0]
	return c == ' ' || c == '\t'
}

// declaredWithOptInComment finds the opt-in directive on the GenDecl doc,
// covering the parenthesised `type ( ... )` form where go/ast attaches the doc
// to the group rather than each TypeSpec.
func declaredWithOptInComment(decl *ast.GenDecl) bool {
	return commentGroupHasOptIn(decl.Doc)
}

// applyToFile atomically writes buf to fn under -apply, behind three guards:
//
//  1. ErrEmptyBuffer on empty buf — never truncate a source file to zero if a
//     future Fprint regression emits nothing.
//  2. ErrNotRegularFile via os.Lstat (not Stat): Stat follows symlinks, so the
//     rename would replace the symlink's target; Lstat refuses the symlink
//     itself. A TOCTOU window remains but the common case is closed.
//  3. renameio's maybe.WriteFile does temp-write + atomic rename, so a crash
//     never leaves half a file.
//
// Wraps ErrStatFile/ErrWriteFile for errors.Is and preserves permission bits.
// Not concurrency-safe on the same fn; run serialises writes.
func applyToFile(fn string, buf []byte) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}

	st, err := os.Lstat(fn)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStatFile, err)
	}

	if !st.Mode().IsRegular() {
		return ErrNotRegularFile
	}

	if err := maybe.WriteFile(fn, buf, st.Mode()); err != nil {
		return fmt.Errorf("%w: %w", ErrWriteFile, err)
	}

	return nil
}
