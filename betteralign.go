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
// rewrite. The alignment math is largely unchanged from fieldalignment/maligned.
// The reorder reprints the whole file (go/ast can't print one node with its
// comments), then cuts each struct's gofmt'd body out of that reprint as a
// SuggestedFix TextEdit, so -fix goes through the standard driver; -apply
// writes the whole-file reprint itself.
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
	"go/build/constraint"
	"go/parser"
	"go/token"
	"go/types"
	"go/version"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/dkorunic/betteralign/internal/diff"
	dst "github.com/dkorunic/betteralign/internal/dstmin"
	"github.com/google/renameio/v2/maybe"
	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/types/typeutil"
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

A struct with a ` + "`structs.HostLayout`" + ` field declares that its layout must
follow the platform's C ABI, so it is neither reported nor rewritten. A struct
with explicit padding (a blank ` + "`_`" + ` field of non-zero size, such as a
cache-line pad) is skipped the same way, since its layout is deliberate.

A struct the package depends on in field order is reported but never
rewritten: one whose field offset is taken with ` + "`unsafe.Offsetof`" + `, one
whose field is passed to a 64-bit ` + "`sync/atomic`" + ` function (which needs it
8-byte aligned on 32-bit platforms), one converted to or from another struct
type, one identical to an unnamed struct
type it may be assigned to (including an alias ` + "`type A = struct{...}`" + `), one
converted to or from ` + "`unsafe.Pointer`" + ` (type punning), and one passed to
` + "`encoding/binary`" + `, ` + "`encoding/asn1`" + ` or ` + "`x/crypto/ssh`" + ` (whose wire format is field order),
and one taken or returned by a body-less function declaration, whose
assembly reads fields at fixed offsets. Reordering would break the build,
corrupt memory or silently change the encoding. The last four also cover
structs held by value inside them. A struct that declares itself ASN.1 — an
` + "`asn1:\"...\"`" + ` tag or a field of an ` + "`encoding/asn1`" + ` type, on it or on a
struct it holds by value — is skipped outright, since its encoder usually lives
in another package; so is one tagged for ` + "`x/crypto/ssh`" + ` (` + "`ssh`" + `, ` + "`sshtype`" + `). A conversion to a struct of another package in the same
module follows that package: both are reordered, identically.

Each rewritable diagnostic carries a suggested fix, so ` + "`-fix`" + ` (and
` + "`-fix -diff`" + ` to preview) rewrites through the standard analysis driver.
` + "`-apply`" + ` rewrites the files itself with atomic writes; under it, ` + "`-diff`" + `
prints a unified diff of each rewrite to stdout (stderr with ` + "`-json`" + `)
instead of updating the files. ` + "`-fix`" + ` takes precedence over ` + "`-apply`" + `.
The driver never applies fixes to files marked as generated, so with
` + "`-generated_files`" + `, ` + "`-fix`" + ` instead rewrites through the ` + "`-apply`" + ` path.
A file that imports ` + "`\"C\"`" + ` is analyzed only with ` + "`-generated_files`" + ` and is
never rewritten.

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
	ErrRewritePanic   = errors.New("internal error while rewriting, please report it")
	ErrApplyUnderVet  = errors.New("-apply is not supported under go vet (each package runs in its own process, " +
		"so rewrites race with other packages' layout predictions); use `go vet -fix` / `go fix -fixtool`, or run betteralign directly")
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

// run is the per-package analyzer entry point. Optimality comes from AST +
// type info alone, so clean files never read source bytes or build DST. A
// misaligned, rewritable struct is reordered in a lazily decorated DST; after
// the walk, one dstmin.Reprint per dirty file yields both the -apply bytes and
// each struct's SuggestedFix, and every diagnostic is then reported in walk
// order. A struct dstmin can't decorate is still reported, just without a fix.
//
// Returns a non-nil error only when exclude-path normalisation fails
// (wrapping ErrPreFilterFiles) or when asked to write under go vet
// (ErrApplyUnderVet); per-file decorate/format/write failures are logged and
// skipped. Safe across concurrent passes; flag slices are cloned because hosts
// like golangci-lint may share flag state.
func (cfg *analyzerConfig) run(pass *analysis.Pass) (any, error) {
	// -fix belongs to the driver, which applies the SuggestedFixes itself (and
	// handles its own -diff). Writing here too would change the file under the
	// driver, which then aborts on the size mismatch, so -fix wins over -apply.
	//
	// Except with -generated_files: the driver refuses every fix in a file
	// marked "Code generated ... DO NOT EDIT" — exactly the files that flag asks
	// for — so -fix would silently rewrite nothing there. Then -fix keeps its
	// v0.15.4 meaning, an alias of -apply: the analyzer writes (and handles
	// -diff) and attaches no fixes, leaving the driver nothing to collide with.
	// Not under go vet, which refuses analyzer writes (ErrApplyUnderVet) and
	// whose -fix never reads a pass's error: the run would exit 0 having done
	// nothing. There -fix stays the driver's, and the flag's no-op is said.
	fix := flagRequested(pass, "fix")
	selfWrite := fix && cfg.generatedFiles && !unitcheckerDriver()
	if fix && cfg.generatedFiles && unitcheckerDriver() {
		vetGeneratedNote.Do(func() {
			fmt.Fprintln(os.Stderr, "betteralign: -generated_files has no effect under go vet -fix "+
				"(the driver refuses fixes in generated files); run betteralign -fix or -apply directly")
		})
	}
	apply := (cfg.apply && !fix) || selfWrite
	// -diff previews an -apply rewrite; never write files under it. Under -json
	// the diff goes to stderr so stdout stays parseable JSON.
	showDiff := apply && flagRequested(pass, "diff")
	// Under go vet each package is its own process: a dependency's process
	// rewrites its file before an importer's predicts from it, the moved
	// struct no longer matches, the importer pins its side and the build
	// breaks. -diff writes nothing, so it stays allowed.
	if apply && !showDiff && unitcheckerDriver() {
		return nil, ErrApplyUnderVet
	}
	diffTo := diffOut
	if flagRequested(pass, "json") {
		diffTo = diffJSONOut
	}
	// Defer the rewrite — both -apply and SuggestedFixes — but never reporting,
	// to the test variant; see deferToTestVariant. Lazy, so clean packages skip
	// the test-file scan.
	var deferredSet, deferred bool
	rewriteDeferred := func() bool {
		if !deferredSet {
			deferred, deferredSet = deferToTestVariant(pass), true
		}
		return deferred
	}
	testFiles := cfg.testFiles
	generatedFiles := cfg.generatedFiles
	optInMode := cfg.optInMode
	// Defensive clones: golangci-lint and other hosts may run analyzers in
	// parallel while StringArrayFlag.Set could be appending concurrently.
	excludeDirs := slices.Clone(cfg.excludeDirs)
	excludeFiles := slices.Clone(cfg.excludeFiles)

	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	// Fall back to 64-bit defaults if the unsafe.Pointer lookup ever fails.
	var wordSize, maxAlign int64 = 8, 8
	if unsafePointerTyp != nil {
		wordSize = pass.TypesSizes.Sizeof(unsafePointerTyp)
		maxAlign = pass.TypesSizes.Alignof(unsafePointerTyp)
	}
	sizes := newGCSizes(wordSize, maxAlign)

	// Built lazily so clean packages skip the composite-literal/call walk.
	// declMoves is set below, before the walk.
	var layoutPins map[*types.Struct]layoutPin
	var declMoves func(*types.Struct) bool
	ensureLayoutPins := func() map[*types.Struct]layoutPin {
		if layoutPins == nil {
			layoutPins = collectLayoutPins(inspect, pass, sizes, declMoves)
		}
		return layoutPins
	}

	// Lazy: nil until the first misaligned struct.
	var dec *dst.Decorator

	// Diagnostics are reported after the walk, once each file's reprint has
	// produced the SuggestedFixes; st is the struct whose reorder is pending, or
	// nil when the diagnostic carries no fix.
	type pendingDiag struct {
		st   *ast.StructType
		diag analysis.Diagnostic
	}
	var diags []pendingDiag

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

	// Measure each named struct this run will reorder at its post-reorder
	// layout, so enclosing structs settle in one run (gcSizes.projected).
	// The prediction must come out the same in every pass — the two sides of a
	// cross-package conversion sort identical fields, and must sort them
	// identically — so it uses only the struct's own layout and what its
	// declaring file shows to anyone (declRewritable), never this package's
	// pins. -opt_in disables the projection: another package's opt-in marks
	// are invisible to the packages measuring it.
	skipFile := func(fn string) bool {
		return (!testFiles && hasSuffix(fn, testSuffixes)) ||
			(!generatedFiles && hasSuffix(fn, generatedSuffixes)) ||
			(wd != "" && isExcluded(wd, fn, excludeDirs, excludeFiles))
	}
	rewrites := make(map[*types.Struct]bool)
	rewritable := func(decl *types.Struct) bool {
		v, ok := rewrites[decl]
		if !ok {
			v = declRewritable(pass, decl, skipFile, generatedFiles)
			rewrites[decl] = v
		}
		return v
	}
	declMoves = rewritable
	if optInMode {
		// A conversion partner reads the mark from its source (declOptedIn).
		declMoves = func(decl *types.Struct) bool { return rewritable(decl) && declOptedIn(pass, decl) }
	} else {
		sizes.rewritten = rewritable
	}

	// Per-file state set on *ast.File visit; reused by child node visits.
	var (
		currentFn      string
		currentASTFile *ast.File
		currentSkip    bool
		// currentHeld: the file has a //line directive (lineDirective);
		// computed on its first rewrite.
		currentHeld, currentHeldSet bool
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
			currentHeldSet = false

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

		candidate, optsz, optptrs := rewriteCandidate(typ, sizes)
		if !candidate {
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
		}

		// AST-based so ignore works even for shapes dstmin can't decorate.
		if hasIgnoreCommentAST(pass.Fset, currentASTFile, s) {
			return
		}

		// A pinned layout is safe to report, unsafe to mutate.
		pin, pinned := ensureLayoutPins()[typ]
		if pinned {
			message += "; reorder skipped: " + fmt.Sprintf(pin.format, pin.where(pass.Fset))
		}
		diags = append(diags, pendingDiag{diag: analysis.Diagnostic{
			Pos:     s.Pos(),
			End:     s.Pos() + structKeywordLen,
			Message: message,
		}})
		if pinned || rewriteDeferred() {
			return
		}
		// Only with -generated_files: a cgo file is parsed from its output in
		// the build cache (named without .go), so a rewrite would change the
		// cache and leave the source as it was.
		if !strings.HasSuffix(currentFn, ".go") {
			return
		}
		if !currentHeldSet {
			currentHeld, currentHeldSet = lineDirective(pass.Fset, currentASTFile), true
		}
		if currentHeld {
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
			df, err := decorateFile(dec, currentASTFile)
			if err != nil {
				// Report-only runs still get the diagnostic; only a requested
				// rewrite (or an internal bug) warrants noise about the missing fix.
				if apply || errors.Is(err, ErrRewritePanic) {
					fmt.Fprintf(os.Stderr, "failed to decorate %s: %v\n", currentFn, err)
				}
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

		// Map type-info indexes (one per name) onto DST fields (one per
		// declaration): a multi-name field (A, B int) takes its first name's
		// slot and dummyField pads the rest, so the declaration moves as a unit.
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
		diags[len(diags)-1].st = s
	})

	// One reprint per dirty file feeds both the SuggestedFixes and -apply.
	// Sorted so partial-failure ordering is reproducible across runs.
	fns := make([]string, 0, len(dirtyFiles))
	for fn := range dirtyFiles {
		fns = append(fns, fn)
	}
	slices.Sort(fns)
	edits := make(map[*ast.StructType]dst.BodyEdit)
	for _, fn := range fns {
		df := dirtyFiles[fn]
		// Reprint already validates its output parses; no second parse needed.
		out, bodyEdits, err := reprint(df)
		if err != nil {
			if apply || errors.Is(err, ErrRewritePanic) {
				fmt.Fprintf(os.Stderr, "failed to print %s: %v\n", fn, err)
			}
			continue
		}
		for _, e := range bodyEdits {
			edits[e.Struct] = e
		}
		if !apply {
			continue
		}
		if showDiff {
			writeDiff(diffTo, fn, df.Source(), out)
			continue
		}
		// Before the write: another pass may be predicting from this file.
		storePristine(fn, df.Source())
		if err := applyToFile(fn, out); err != nil {
			fmt.Fprintf(os.Stderr, "error applying fixes to %v: %v\n", fn, err)
		}
	}

	for _, d := range diags {
		// No fixes when the analyzer writes the file itself: nothing needs
		// them, and a host applying them anyway (golangci-lint's --fix over a
		// plugin set to apply, invisible to flagRequested) would edit the
		// rewritten file at offsets an earlier rewrite shifted.
		if e, ok := edits[d.st]; ok && d.st != nil && !selfWrite && (!apply || showDiff) && validEdit(pass.Fset, e) {
			d.diag.SuggestedFixes = []analysis.SuggestedFix{{
				Message:   fixMessage,
				TextEdits: []analysis.TextEdit{{Pos: e.Pos, End: e.End, NewText: e.New}},
			}}
		}
		pass.Report(d.diag)
	}

	return nil, nil
}

// decorateFile and reprint wrap the dstmin entry points so a dstmin bug costs
// one file's fix, never the run. dstmin runs in every mode now (SuggestedFixes
// need it, not just -apply), so a panic there would otherwise turn a plain
// report into a crash. The panic surfaces as ErrRewritePanic, which run always
// logs (it is a bug to report); the file loses its fix, the diagnostic stays.
// Variables so tests can inject a panic.
var (
	decorateFile = func(dec *dst.Decorator, f *ast.File) (df *dst.File, err error) {
		defer recoverRewrite(&err)
		return dec.DecorateFile(f)
	}
	reprint = func(df *dst.File) (out []byte, edits []dst.BodyEdit, err error) {
		defer recoverRewrite(&err)
		return dst.Reprint(df)
	}
)

// recoverRewrite converts a panic in a deferred-from function into
// ErrRewritePanic on *err.
func recoverRewrite(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("%w: %v", ErrRewritePanic, r)
	}
}

// validEdit reports whether e satisfies the driver's fix validation: both
// ends in one file, in order. The x/tools checker panics on an invalid fix
// inside pass.Report, killing the whole run, so a malformed edit must cost
// only its own fix. Brace positions always pass today; this is the backstop.
func validEdit(fset *token.FileSet, e dst.BodyEdit) bool {
	tf := fset.File(e.Pos)
	return tf != nil && e.Pos.IsValid() && e.End.IsValid() && e.Pos <= e.End && fset.File(e.End) == tf
}

// rewriteCandidate reports whether str's layout alone makes it a reorder
// candidate — misaligned with a real size or ptrdata saving, and none of the
// layouts betteralign leaves alone — plus the optimal size and ptrdata. It is
// the single source of truth for both run's decision and collectLayoutPins'
// prediction of what another package's pass does with its struct, so the two
// can't drift. Directives, pins and file filters are applied on top by run.
func rewriteCandidate(str *types.Struct, sizes *gcSizes) (ok bool, optSize, optPtrdata int64) {
	// Compute optimality without DST and without allocating a permutation;
	// clean files pay no decoration cost and the hot path stays zero-alloc.
	optimal, optSize, optPtrdata := layoutMetrics(str, sizes)
	if optimal {
		return false, 0, 0
	}
	// Skip structs whose layout depends on a type parameter: sized from the
	// constraint underlying, their figures are per-instantiation guesses. On
	// the misaligned path only, so non-generic structs pay nothing.
	if layoutDependsOnTypeParam(str) {
		return false, 0, 0
	}
	// structs.HostLayout (a C-ABI contract) and explicit blank padding make the
	// layout deliberate: nothing to report, since the user can't act on it and
	// the "could be" figure would need breaking that contract.
	if hasHostLayout(str) || hasBlankPadding(str, sizes) || isASN1Struct(str) || hasSSHTag(str) {
		return false, 0, 0
	}
	size, ptrdata := sizes.Sizeof(str), sizes.ptrdata(str)
	if size == optSize && ptrdata == optPtrdata {
		return false, 0, 0
	}
	// The sort never makes either figure worse except on 32-bit, where an
	// 8-aligned atomic.Int64 (align64) sorts ahead of 4-aligned pointers and
	// can push them later. Size, the real memory, still decides: a smaller
	// struct is worth more pointer bytes, but a reorder that saves no memory
	// and raises them ("-4 bytes saved") is not offered.
	if optSize > size || optSize == size && optPtrdata > ptrdata {
		return false, 0, 0
	}
	return true, optSize, optPtrdata
}

// fixMessage titles the reorder SuggestedFix; it matches upstream
// fieldalignment's so hosts that key on it treat betteralign as a drop-in.
const fixMessage = "Rearrange fields"

// flagRequested reports whether a driver-owned boolean flag (-fix, -diff,
// -json) is set. Both flag sets matter: x/tools drivers register them on the
// process-global set (via analysisflags), while hosts and the test harness wire
// them onto Analyzer.Flags. Checking only Analyzer.Flags would miss -fix in the
// shipped binary, letting -apply write files the driver is about to fix.
func flagRequested(pass *analysis.Pass, name string) bool {
	if a := pass.Analyzer.Flags.Lookup(name); a != nil && a.Value.String() == "true" {
		return true
	}
	if a := flag.Lookup(name); a != nil && a.Value.String() == "true" {
		return true
	}
	return false
}

// diffOut receives -diff output, and diffJSONOut it under -json, where stdout
// carries the driver's JSON; variables so tests can capture them.
var (
	diffOut     io.Writer = os.Stdout
	diffJSONOut io.Writer = os.Stderr
	diffMu      sync.Mutex
)

// writeDiff prints the unified diff of one rewrite to w in gofmt -d form. The
// lock keeps whole-file diffs from interleaving across concurrent passes.
func writeDiff(w io.Writer, fn string, orig, rewritten []byte) {
	d := diff.Diff(fn+".orig", orig, fn, rewritten)
	if d == nil {
		return
	}
	diffMu.Lock()
	defer diffMu.Unlock()
	if _, err := w.Write(d); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write diff for %s: %v\n", fn, err)
	}
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

// layoutPin records why a struct's field order must not change: pos is the
// offending source position and format the diagnostic suffix, taking pos as %s.
// at replaces pos for a position outside the pass's FileSet (a file this
// build excludes, see otherBuildPositional).
type layoutPin struct {
	format string
	at     string
	pos    token.Pos
}

// where renders the pin's position for its diagnostic.
func (p layoutPin) where(fset *token.FileSet) string {
	if p.at != "" {
		return p.at
	}
	return fset.Position(p.pos).String()
}

const (
	pinPositional = "positional composite literal at %s would break, convert to keyed form first"
	pinOffsetof   = "unsafe.Offsetof at %s depends on field order"
	pinConversion = "conversion at %s needs identical field order"
	pinIdentical  = "identical unnamed struct type at %s needs the same field order"
	pinUnsafe     = "unsafe.Pointer conversion at %s depends on field layout"
	pinBinary     = "encoding/binary call at %s depends on field order"
	pinASN1       = "encoding/asn1 call at %s depends on field order"
	pinSSH        = "x/crypto/ssh call at %s depends on field order"
	pinAtomic     = "64-bit atomic access at %s needs its 8-byte alignment on 32-bit platforms"
	pinAssembly   = "assembly implementation at %s (body-less func declaration) reads fields by offset"
)

// isAtomic64Func reports whether fn is one of sync/atomic's 64-bit functions
// (AddInt64, LoadUint64, CompareAndSwapInt64, ...). Their operand must be
// 8-byte aligned on 386, arm and 32-bit mips; the typed atomic.Int64 and
// atomic.Uint64 are aligned by the compiler and need no pin.
func isAtomic64Func(fn *types.Func) bool {
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != "sync/atomic" {
		return false
	}
	if sig, ok := fn.Type().(*types.Signature); !ok || sig.Recv() != nil {
		return false
	}
	return strings.HasSuffix(fn.Name(), "Int64") || strings.HasSuffix(fn.Name(), "Uint64")
}

// orderCodecs are the functions that encode or decode struct fields in
// declaration order, by package path; the value is the pin message.
// encoding/asn1 also encodes slices of structs (SEQUENCE OF), so its pins
// follow slice fields too.
var orderCodecs = map[string]map[string]string{
	"encoding/binary": {
		"Read": pinBinary, "Write": pinBinary, "Size": pinBinary,
		"Append": pinBinary, "Encode": pinBinary, "Decode": pinBinary,
	},
	"encoding/asn1": {
		"Marshal": pinASN1, "MarshalWithParams": pinASN1,
		"Unmarshal": pinASN1, "UnmarshalWithParams": pinASN1,
	},
	"golang.org/x/crypto/ssh": {"Marshal": pinSSH, "Unmarshal": pinSSH},
}

// unsafeOffsetof is the unsafe.Offsetof builtin; go/types records every use of
// it (qualified or dot-imported) against this one object.
var unsafeOffsetof = types.Unsafe.Scope().Lookup("Offsetof")

// collectLayoutPins maps each *types.Struct whose field order the package
// relies on to the first such use, which the diagnostic surfaces as a jump
// target. These uses pin a struct:
//
//   - a positional composite literal (`T{1, 2, 3}`, not keyed): reordering
//     would re-map its elements to other fields.
//   - unsafe.Offsetof(x.f): pins x's struct and, for a promoted f, every
//     embedded struct on the path. Offsetof itself recomputes, but the code
//     around it (constant offset asserts, 64-bit atomic alignment checks,
//     offsets shared with assembly or C) usually assumes today's layout.
//   - a conversion between two distinct struct types (U(t), (*U)(&t)): Go
//     requires identical underlying types, field order included, so
//     reordering one side alone (the other ignored, excluded or pinned) breaks
//     the build. Both sides are pinned.
//   - &x.f passed to a 64-bit sync/atomic function (AddInt64, LoadUint64,
//     ...): 386, arm and 32-bit mips need f 8-byte aligned, which only the
//     first word of an allocation guarantees, so x's layout (and every
//     embedded struct on the path) is deliberate.
//   - an unnamed struct type identical to another struct type in the package
//     (a parameter, variable or alias `type A = struct{...}`): assignability
//     between them rests on identical field order. Both are pinned.
//   - an unsafe.Pointer conversion to or from *S: type punning reads S's
//     memory layout directly, so a reorder corrupts it silently.
//   - an argument of an orderCodecs function (encoding/binary, encoding/asn1,
//     x/crypto/ssh): the wire format is field declaration order, so a reorder
//     silently changes it.
//   - a parameter, result or receiver of a body-less func declaration: its
//     assembly implementation reads fields at fixed offsets, and nothing
//     (build, vet, asmdecl) notices when they move. One a //go:linkname
//     directive names is Go code elsewhere and pins nothing. Declarations in files this build excludes
//     (the usual arch-gated stub) are found syntactically, by type name.
//
// The last three also pin every struct held by value inside S (fields, arrays),
// since those bytes are part of the same layout.
//
// Per-file skip flags deliberately do not apply: an excluded file still
// compiles against the layout, so its use would break just the same. Elts[0]
// classifies the whole literal (Go forbids mixing keyed and positional).
// Returns a fresh map; one per pass.
func collectLayoutPins(inspect *inspector.Inspector, pass *analysis.Pass, sizes *gcSizes, declMoves func(*types.Struct) bool) map[*types.Struct]layoutPin {
	pins := make(map[*types.Struct]layoutPin)
	foreignMoves := func(st *types.Struct) bool { return foreignRewritten(pass, st, sizes, declMoves) }
	// atomicHeld: every struct an atomic pin placed, whichever pin came first.
	atomicHeld := make(map[*types.Struct]layoutPin)
	pin := func(st *types.Struct, p layoutPin) {
		if _, seen := pins[st]; !seen {
			pins[st] = p
		}
		if _, seen := atomicHeld[st]; !seen && p.format == pinAtomic {
			atomicHeld[st] = p
		}
	}

	// pinLayout pins st and every struct it holds by value: punning and binary
	// encoding see all of those bytes. With followSlices (ASN.1), structs reached
	// through slice fields are pinned as well.
	var pinLayout func(t types.Type, p layoutPin, followSlices bool, seen map[*types.Struct]bool)
	pinLayout = func(t types.Type, p layoutPin, followSlices bool, seen map[*types.Struct]bool) {
		var visited map[*types.Named]bool
		for !revisits(t, &visited) {
			if tp, ok := types.Unalias(t).(*types.TypeParam); ok {
				for _, st := range structsOf(tp) {
					pinLayout(st, p, followSlices, seen)
				}
				return
			}
			switch u := types.Unalias(t).Underlying().(type) {
			case *types.Array:
				t = u.Elem()
				continue
			case *types.Slice:
				if followSlices {
					t = u.Elem()
					continue
				}
			case *types.Struct:
				st := canonicalStructType(t)
				if st == nil || seen[st] {
					return
				}
				seen[st] = true
				pin(st, p)
				for f := range st.Fields() {
					pinLayout(f.Type(), p, followSlices, seen)
				}
			}
			return
		}
	}

	// pinFieldPath pins the struct of field selector sel and, for a promoted
	// field, every embedded struct on the path to it: each one's layout places
	// the field. The field's own type is not pinned.
	pinFieldPath := func(sel *ast.SelectorExpr, p layoutPin) {
		selection := pass.TypesInfo.Selections[sel]
		if selection == nil || selection.Kind() != types.FieldVal {
			return
		}
		t := selection.Recv()
		for _, idx := range selection.Index() {
			st := canonicalStructType(t)
			if st == nil || idx >= st.NumFields() {
				return
			}
			pin(st, p)
			t = st.Field(idx).Type()
		}
	}

	// elemOf walks t to the element a value arrives as: through pointers
	// and slices (codec arguments, assembly operands); pinLayout takes arrays.
	elemOf := func(t types.Type) types.Type {
		var visited map[*types.Named]bool
		for !revisits(t, &visited) {
			switch u := types.Unalias(t).Underlying().(type) {
			case *types.Pointer:
				t = u.Elem()
				continue
			case *types.Slice:
				t = u.Elem()
				continue
			}
			break
		}
		return t
	}

	// Unnamed struct type literals, checked for identity after the walk.
	var unnamed, all []*types.Struct
	allPos := make(map[*types.Struct]token.Pos)
	defined := make(map[*ast.StructType]bool)

	inspect.Preorder([]ast.Node{(*ast.CompositeLit)(nil), (*ast.CallExpr)(nil), (*ast.TypeSpec)(nil), (*ast.StructType)(nil), (*ast.FuncDecl)(nil)}, func(node ast.Node) {
		switch n := node.(type) {
		case *ast.FuncDecl:
			// Body-less: implemented in assembly, which reads its operands'
			// fields by offset — unless //go:linkname binds it to Go code of
			// this package (linkTarget).
			if n.Body != nil {
				return
			}
			for _, f := range pass.Files {
				if f.FileStart <= n.Pos() && n.Pos() < f.FileEnd && selfLinked(pass.Pkg, linkTarget(f, n)) {
					return
				}
			}
			fn, _ := pass.TypesInfo.Defs[n.Name].(*types.Func)
			if fn == nil {
				return
			}
			sig := fn.Signature()
			p := layoutPin{format: pinAssembly, pos: n.Pos()}
			vars := []*types.Var{sig.Recv()}
			for _, tup := range []*types.Tuple{sig.Params(), sig.Results()} {
				for v := range tup.Variables() {
					vars = append(vars, v)
				}
			}
			for _, v := range vars {
				if v != nil {
					pinLayout(elemOf(v.Type()), p, false, map[*types.Struct]bool{})
				}
			}
			return
		case *ast.TypeSpec:
			// A defined type's struct is named; an alias's stays unnamed.
			if st, ok := n.Type.(*ast.StructType); ok && !n.Assign.IsValid() {
				defined[st] = true
			}
			return
		case *ast.StructType:
			st, ok := pass.TypesInfo.TypeOf(n).(*types.Struct)
			if !ok || st.NumFields() == 0 {
				return
			}
			all = append(all, st)
			allPos[st] = n.Pos()
			if !defined[n] {
				unnamed = append(unnamed, st)
			}
			return
		}

		if call, ok := node.(*ast.CallExpr); ok {
			// Order codecs first and regardless of arity: binary.Size takes
			// one argument, most others two or three.
			fn, _ := typeutil.Callee(pass.TypesInfo, call).(*types.Func)
			var codec string
			if fn != nil && fn.Pkg() != nil {
				codec = orderCodecs[fn.Pkg().Path()][fn.Name()]
			}
			if codec != "" {
				for _, arg := range call.Args {
					t := pass.TypesInfo.TypeOf(arg)
					if t == nil {
						continue
					}
					// Values arrive by pointer, slice or array; walk to the element.
					pinLayout(elemOf(t), layoutPin{format: codec, pos: call.Pos()}, codec == pinASN1, map[*types.Struct]bool{})
				}
				return
			}
			// The 64-bit function API (AddInt64(&s.n, 1), ...) needs the word
			// 8-byte aligned on 32-bit platforms, which only the first word of
			// an allocation guarantees, so the layout around it is deliberate.
			if isAtomic64Func(fn) && len(call.Args) > 0 {
				// The word may arrive through a pointer conversion:
				// (*int64)(&s.d), (*int64)(unsafe.Pointer(&s.u)).
				arg := ast.Unparen(call.Args[0])
				for {
					conv, ok := arg.(*ast.CallExpr)
					if !ok || len(conv.Args) != 1 || !pass.TypesInfo.Types[conv.Fun].IsType() {
						break
					}
					arg = ast.Unparen(conv.Args[0])
				}
				if addr, ok := arg.(*ast.UnaryExpr); ok && addr.Op == token.AND {
					// &s.f, an element of an array field (&s.f[i]), which the
					// array's offset places, or a field of a struct held by
					// value (&s.t.f), which t's offset places. A slice's
					// elements or a pointer's target are their own allocation.
					x := ast.Unparen(addr.X)
					for {
						var inner ast.Expr
						switch e := x.(type) {
						case *ast.IndexExpr:
							inner = e.X
						case *ast.SelectorExpr:
							pinFieldPath(e, layoutPin{format: pinAtomic, pos: call.Pos()})
							inner = e.X
						}
						if inner == nil {
							break
						}
						t := pass.TypesInfo.TypeOf(inner)
						if t == nil {
							break
						}
						switch types.Unalias(t).Underlying().(type) {
						case *types.Array, *types.Struct:
							x = ast.Unparen(inner)
							continue
						}
						break
					}
				}
				return
			}
			if len(call.Args) != 1 {
				return
			}
			if tv, ok := pass.TypesInfo.Types[call.Fun]; ok && tv.IsType() {
				to, from := tv.Type, pass.TypesInfo.TypeOf(call.Args[0])
				if from == nil {
					return
				}
				switch {
				case isUnsafePointer(from):
					if p, ok := types.Unalias(to).Underlying().(*types.Pointer); ok {
						pinLayout(p.Elem(), layoutPin{format: pinUnsafe, pos: call.Pos()}, false, map[*types.Struct]bool{})
					}
				case isUnsafePointer(to):
					if p, ok := types.Unalias(from).Underlying().(*types.Pointer); ok {
						pinLayout(p.Elem(), layoutPin{format: pinUnsafe, pos: call.Pos()}, false, map[*types.Struct]bool{})
					}
				default:
					// A type parameter converts as each struct of its type set;
					// P(p) converts nothing (each term to itself).
					if types.Identical(to, from) {
						return
					}
					for _, a := range structsOf(to) {
						for _, b := range structsOf(from) {
							// A partner from another package is rewritten (or not)
							// by that package's own pass, which can't see this
							// conversion. If it will be reordered, the local side
							// must follow — identical fields sort to the identical
							// order — so pinning it would be what breaks the build
							// (net/http's HTTP2Config converts to
							// net/http/internal/http2.Config). Otherwise pin as usual.
							if a == b || foreignMoves(a) || foreignMoves(b) {
								continue
							}
							pin(a, layoutPin{format: pinConversion, pos: call.Pos()})
							pin(b, layoutPin{format: pinConversion, pos: call.Pos()})
						}
					}
				}
				return
			}
			if typeutil.Callee(pass.TypesInfo, call) != unsafeOffsetof {
				return
			}
			if sel, ok := ast.Unparen(call.Args[0]).(*ast.SelectorExpr); ok {
				pinFieldPath(sel, layoutPin{format: pinOffsetof, pos: call.Pos()})
			}
			return
		}

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
		for _, st := range structsOf(tv.Type) {
			pin(st, layoutPin{format: pinPositional, pos: lit.Pos()})
		}
	})

	// Unnamed struct types also arrive from imports, as the type of another
	// package's parameter, result, variable or field (func F(x struct{...})).
	// A local struct assigned to one must keep its order, so they join the
	// identity check, at the importing object's position. Uses is a map, so the
	// objects are sorted for a stable first pin.
	var foreign []types.Object
	for _, obj := range pass.TypesInfo.Uses {
		if obj.Pkg() != nil && obj.Pkg() != pass.Pkg {
			foreign = append(foreign, obj)
		}
	}
	slices.SortFunc(foreign, func(a, b types.Object) int { return comparePos(pass.Fset, a.Pos(), b.Pos()) })
	// A named type the package references is opened one level, its fields
	// or elements: a positional dep.Holder{h} or an element of dep.List{e}
	// assigns to them with no field reference of its own. Named types inside
	// it are not opened, which keeps the walk to what the package references
	// rather than its whole dependency graph. inNamed is part of the memo key,
	// so a type first seen inside another one is still opened when referenced.
	type walkKey struct {
		t       types.Type
		inNamed bool
	}
	walked := make(map[walkKey]bool)
	var addUnnamed func(t types.Type, pos token.Pos, inNamed bool)
	addUnnamed = func(t types.Type, pos token.Pos, inNamed bool) {
		t = types.Unalias(t)
		if _, ok := t.(*types.Named); ok && inNamed {
			return
		}
		if walked[walkKey{t, inNamed}] {
			return
		}
		walked[walkKey{t, inNamed}] = true
		switch t := t.(type) {
		case *types.Named:
			if st, ok := t.Underlying().(*types.Struct); ok {
				for f := range st.Fields() {
					addUnnamed(f.Type(), pos, true)
				}
			} else {
				addUnnamed(t.Underlying(), pos, true)
			}
		case *types.Struct:
			// Its fields need no walk: assigning to one goes through the field,
			// which has its own Uses entry, or spells the type in this package.
			if _, local := allPos[t]; !local && t.NumFields() > 0 {
				unnamed = append(unnamed, t)
				allPos[t] = pos
			}
		case *types.Pointer:
			addUnnamed(t.Elem(), pos, inNamed)
		case *types.Slice:
			addUnnamed(t.Elem(), pos, inNamed)
		case *types.Array:
			addUnnamed(t.Elem(), pos, inNamed)
		case *types.Chan:
			addUnnamed(t.Elem(), pos, inNamed)
		case *types.Map:
			addUnnamed(t.Key(), pos, inNamed)
			addUnnamed(t.Elem(), pos, inNamed)
		case *types.Signature:
			for v := range t.Params().Variables() {
				addUnnamed(v.Type(), pos, inNamed)
			}
			for v := range t.Results().Variables() {
				addUnnamed(v.Type(), pos, inNamed)
			}
		}
	}
	for _, obj := range foreign {
		addUnnamed(obj.Type(), obj.Pos(), false)
	}
	// Instances are new types, identical to what their declaration doesn't
	// spell: G[int]'s struct to struct{p *int}, F[int]'s parameter
	// struct{p *T} to a local struct{p *int}. Their structs join the check,
	// pinning (and pinned through) the generic declaration.
	origin := make(map[*types.Struct]*types.Struct)
	instIDs := slices.SortedFunc(maps.Keys(pass.TypesInfo.Instances), func(a, b *ast.Ident) int { return comparePos(pass.Fset, a.Pos(), b.Pos()) })
	for _, id := range instIDs {
		t := pass.TypesInfo.Instances[id].Type
		if st, ok := t.Underlying().(*types.Struct); ok && st.NumFields() > 0 {
			if o := canonicalStructType(t); o != nil && o != st {
				if _, known := allPos[st]; !known {
					origin[st] = o
					all = append(all, st)
					allPos[st] = id.Pos()
				}
			}
		}
		addUnnamed(t, id.Pos(), false)
	}
	canon := func(st *types.Struct) *types.Struct {
		if o, ok := origin[st]; ok {
			return o
		}
		return st
	}

	// Positional literals in the package's files this build excludes (another
	// GOOS/GOARCH, an unset tag) are invisible to type checking, yet a reorder
	// breaks that build or, with compatible field types, silently re-maps its
	// values. Found syntactically, by type name; after the typed pins, which
	// win.
	other := otherBuildPositional(pass)
	for _, path := range slices.Sorted(maps.Keys(other.lits)) {
		for _, st := range resolveLiteralPath(pass.Pkg, path, other.decls) {
			pin(st, layoutPin{format: pinPositional, at: other.lits[path]})
		}
	}
	// Likewise the operands of a body-less declaration there: assembly stubs
	// are usually arch-gated (f_arm64.go beside f_arm64.s), so the in-build
	// check above misses the main case.
	// The first stub not linknamed to this package's Go code names the pin.
	for _, name := range slices.Sorted(maps.Keys(other.asm)) {
		i := slices.IndexFunc(other.asm[name], func(s asmStub) bool { return !selfLinked(pass.Pkg, s.link) })
		if i < 0 {
			continue
		}
		p := layoutPin{format: pinAssembly, at: other.asm[name][i].at}
		seen := make(map[*types.Struct]bool)
		// A named slice or array (type HS []E) is no struct to resolve:
		// walk to its elements like an in-build operand.
		if tn, ok := pass.Pkg.Scope().Lookup(name).(*types.TypeName); ok {
			pinLayout(elemOf(tn.Type()), p, false, seen)
		}
		for _, st := range resolveLiteralPath(pass.Pkg, name, other.decls) {
			pinLayout(st, p, false, seen)
		}
	}

	// Identity: each unnamed struct type pins every distinct identical struct
	// type, and is pinned by it. Identical structs share their field names in
	// order, so bucketing by them confines types.Identical to real candidates;
	// a field-count bucket made generated code with thousands of unnamed
	// structs quadratic (6000 three-field structs: +0.5s; 60000: ~minutes).
	byNames := make(map[string][]*types.Struct)
	for _, st := range all {
		k := fieldNamesKey(st)
		byNames[k] = append(byNames[k], st)
	}
	for _, u := range unnamed {
		for _, other := range byNames[fieldNamesKey(u)] {
			if other == u || !types.Identical(u, other) {
				continue
			}
			// As for conversions: a side another package reorders (an alias
			// type A = struct{...} there) takes the local side with it.
			cu, co := canon(u), canon(other)
			if cu == co || foreignMoves(cu) || foreignMoves(co) {
				continue
			}
			pin(co, layoutPin{format: pinIdentical, pos: allPos[u]})
			pin(cu, layoutPin{format: pinIdentical, pos: allPos[other]})
		}
	}

	// A struct an atomic pin placed is 8-aligned only at offset 0 of its
	// allocation or at an 8-aligned offset in its holder, so every struct
	// holding it by value keeps its layout too (Outer{c Counter; p *int}
	// whose method does atomic.AddInt64(&c.n, 1)): to the fixed point, since
	// holders nest.
	for changed := len(atomicHeld) > 0; changed; {
		changed = false
		for _, st := range all {
			st = canon(st)
			if _, done := atomicHeld[st]; done {
				continue
			}
			if p, ok := holdsAtomic(st, atomicHeld); ok {
				pin(st, p)
				changed = true
			}
		}
	}

	return pins
}

// holdsAtomic reports whether st holds, by value (fields, arrays, inline
// struct literals), a struct of held, and that struct's pin. A named struct
// field is looked up, not opened: the caller iterates to the fixed point.
func holdsAtomic(st *types.Struct, held map[*types.Struct]layoutPin) (layoutPin, bool) {
	for f := range st.Fields() {
		t := types.Unalias(f.Type())
		var visited map[*types.Named]bool
		for {
			a, ok := t.Underlying().(*types.Array)
			if !ok || revisits(t, &visited) {
				break
			}
			t = types.Unalias(a.Elem())
		}
		switch t := t.(type) {
		case *types.Named:
			if p, ok := held[canonicalStructType(t)]; ok {
				return p, true
			}
		case *types.Struct:
			if p, ok := holdsAtomic(t, held); ok {
				return p, true
			}
		}
	}
	return layoutPin{}, false
}

// foreignRewritten predicts whether st, a struct literal, is declared in
// another package that the same run will reorder: declMoves (sameRun and
// nothing in its source keeping it in place: see declRewritable; under
// -opt_in, also the mark), a rewriteCandidate, and a shape dstmin accepts.
// The declaring package is the first field's, which also covers a defined
// type over another package's struct and an alias of a struct literal. That
// package's pins are invisible from here, as is whether it is actually among
// the analyzed packages, so the prediction can miss in either direction: a
// partner pinned in its own package (a positional literal, unsafe.Offsetof, a
// holder of an atomically accessed struct) stays while the local side moves,
// a compile error at the conversion — loud, unlike the 386 alignment panic or
// mis-assigned literal that reordering the partner would cause.
func foreignRewritten(pass *analysis.Pass, st *types.Struct, sizes *gcSizes, declMoves func(*types.Struct) bool) bool {
	if st == nil || st.NumFields() == 0 || st.Field(0).Pkg() == pass.Pkg || declMoves == nil || !declMoves(st) {
		return false
	}
	ok, _, _ := rewriteCandidate(st, sizes)
	return ok && declDecorates(pass, st)
}

// declDecorates reports whether dstmin would accept the struct literal decl
// for a reorder; a shape it refuses stays as declared, whatever its layout.
// A source that doesn't hold the declaration (declAt) gives no such
// evidence, so it counts as accepted.
func declDecorates(pass *analysis.Pass, decl *types.Struct) bool {
	if decl.NumFields() == 0 {
		return false
	}
	_, ds, ok := declAt(pass, decl)
	if !ok {
		return true
	}
	accepted, known := ds.decorated()[pass.Fset.PositionFor(decl.Field(0).Pos(), false).Line]
	return !known || accepted
}

// decoratedStructs maps the first-field line of each struct in src (the
// outermost, where one line starts several) to whether dstmin accepts it.
func decoratedStructs(fn string, src []byte) map[int]bool {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, fn, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	dec := dst.NewDecorator(fset)
	dec.DecorateFileSrc(f, src)
	byLine := make(map[int]bool)
	ast.Inspect(f, func(n ast.Node) bool {
		if st, ok := n.(*ast.StructType); ok && st.Fields != nil && len(st.Fields.List) > 0 {
			line := fset.PositionFor(st.Fields.List[0].Pos(), false).Line
			if _, seen := byLine[line]; !seen {
				_, byLine[line] = dec.Dst.Nodes[st]
			}
		}
		return true
	})
	return byLine
}

// declOptedIn reports whether the struct literal decl carries the -opt_in
// mark in its source, the only place another package's pass can see it. An
// unreadable file counts as unmarked.
func declOptedIn(pass *analysis.Pass, decl *types.Struct) bool {
	if decl.NumFields() == 0 {
		return false
	}
	d, _, ok := declAt(pass, decl)
	return ok && d.optedIn
}

// comparePos orders positions by file name, then offset: a FileSet's file
// bases follow go/packages' concurrent parse order, so comparing raw Pos
// values orders files differently from run to run.
func comparePos(fset *token.FileSet, a, b token.Pos) int {
	pa, pb := fset.PositionFor(a, false), fset.PositionFor(b, false)
	return cmp.Or(cmp.Compare(pa.Filename, pb.Filename), cmp.Compare(pa.Offset, pb.Offset))
}

// sameRun guesses whether obj's package is analyzed in the same run as pass:
// the same module, another module of the active go.work workspace (`./...`
// at its root analyzes them together), or — without module information — the
// same side of the standard-library boundary (std builds and GOPATH-mode
// trees such as analysistest's). A module only replaced to a local directory
// is not in the run; neither is anything in the module cache or GOROOT, whose
// files never sit in a used module's directory.
func sameRun(pass *analysis.Pass, obj types.Object) bool {
	pkg := obj.Pkg()
	if pass.Module == nil || pass.Module.Path == "" {
		return isStdPath(pass.Pkg.Path()) == isStdPath(pkg.Path())
	}
	m := pass.Module.Path
	underModule := pkg.Path() == m || strings.HasPrefix(pkg.Path(), m+"/")
	file := pass.Fset.Position(obj.Pos()).Filename
	var self string
	if len(pass.Files) > 0 {
		// The package clause's position, not the token.File's name: a cgo
		// file is parsed from the build cache but //line-mapped back.
		self = pass.Fset.Position(pass.Files[0].Package).Filename
	}
	if !filepath.IsAbs(file) || !filepath.IsAbs(self) {
		return underModule
	}
	// The import path alone can't tell a nested module, which `./...` stops at.
	root := moduleRoot(file)
	if underModule && root == moduleRoot(self) {
		return true
	}
	// The go command picks the go.work above its working directory, not the
	// package's: a workspace may use a module outside its own tree (../m2).
	wd, err := os.Getwd()
	if err != nil {
		return false
	}
	return slices.Contains(workspaceModules(wd), root)
}

// workspaceCache memoises workspaceModules by GOWORK and starting directory.
var workspaceCache sync.Map // map[[2]string][]string

// workspaceModules returns the directories of the modules used by the go.work
// governing a go command run in dir — GOWORK if set ("off" disables
// workspaces), else the nearest go.work above dir — or nil outside a
// workspace.
func workspaceModules(dir string) []string {
	key := [2]string{os.Getenv("GOWORK"), dir}
	if v, ok := workspaceCache.Load(key); ok {
		return v.([]string)
	}
	var dirs []string
	work := key[0]
	if work == "" {
		for d := dir; ; d = filepath.Dir(d) {
			if _, err := os.Stat(filepath.Join(d, "go.work")); err == nil {
				work = filepath.Join(d, "go.work")
				break
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	if work != "" && work != "off" {
		if data, err := os.ReadFile(work); err == nil {
			if wf, err := modfile.ParseWork(work, data, nil); err == nil {
				for _, u := range wf.Use {
					p := u.Path
					if !filepath.IsAbs(p) {
						p = filepath.Join(filepath.Dir(work), p)
					}
					dirs = append(dirs, filepath.Clean(p))
				}
			}
		}
	}
	workspaceCache.Store(key, dirs)
	return dirs
}

// moduleRoot returns the directory of the go.mod nearest above file, or "".
// Vendored code has none of its own and belongs to no used module: the walk
// stops at a vendor directory rather than reach the vendoring module's go.mod
// (go work vendor puts vendor/ in a workspace root that may be `use .`).
func moduleRoot(file string) string {
	for d := filepath.Dir(file); ; d = filepath.Dir(d) {
		if filepath.Base(d) == "vendor" {
			return ""
		}
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// otherBuildCache memoises scanOtherBuild by path, size and modification
// time (a long-running host sees an edited file afresh), built once: passes
// start concurrently, and p and p [p.test] share their ignored files.
var otherBuildCache sync.Map // map[declSourceKey]func() otherBuildScan

// otherBuildScan is one excluded file's package name, its first positional
// composite literal ("file:line:col") per literal type path, its body-less
// func declarations per operand type name (asm), and its non-generic type
// declarations (name to type expression).
type otherBuildScan struct {
	lits  map[string]string
	asm   map[string][]asmStub
	decls map[string]ast.Expr
	pkg   string
}

// asmStub is a body-less func declaration of an excluded file: its location
// and its linkTarget, judged by collectLayoutPins, which knows the package.
type asmStub struct {
	at, link string
}

// otherBuild is otherBuildScan merged over a package's excluded files.
type otherBuild struct {
	lits  map[string]string
	asm   map[string][]asmStub
	decls map[string][]ast.Expr
}

// otherBuildPositional maps each literal type path (see scanOtherBuild) to the
// first positional composite literal of it in pass's .go files of its own
// package that the build excluded (pass.IgnoredFiles: another GOOS/GOARCH, an
// unset tag, a tag the run set that a file negates), and each operand type
// name to the first body-less func declaration taking or returning it there.
// It also returns the type declarations of those files, every one per name:
// the builds that declare a name differently each need theirs.
func otherBuildPositional(pass *analysis.Pass) otherBuild {
	out := otherBuild{lits: make(map[string]string), asm: make(map[string][]asmStub), decls: make(map[string][]ast.Expr)}
	for _, path := range slices.Sorted(slices.Values(pass.IgnoredFiles)) {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		key := declSourceKey{path, fi.Size(), fi.ModTime().UnixNano()}
		scan, _ := otherBuildCache.LoadOrStore(key, sync.OnceValue(func() otherBuildScan {
			return scanOtherBuild(path)
		}))
		s := scan.(func() otherBuildScan)()
		if s.pkg != pass.Pkg.Name() {
			continue
		}
		keepFirst(out.lits, s.lits)
		for name, stubs := range s.asm {
			out.asm[name] = append(out.asm[name], stubs...)
		}
		for _, name := range slices.Sorted(maps.Keys(s.decls)) {
			out.decls[name] = append(out.decls[name], s.decls[name])
		}
	}
	return out
}

// keepFirst copies src's entries into dst, keeping any dst already has.
func keepFirst(dst, src map[string]string) {
	for k, v := range src {
		if _, seen := dst[k]; !seen {
			dst[k] = v
		}
	}
}

// scanOtherBuild records path's positional composite literals by the type
// they build. Only the syntax is available, so a type is a path: a local type
// name, then for each elided level ([]T{{...}}, M{"k": {...}} with M a named
// map) "/e" for an element or "/k" for a map key, resolved against the
// package's types by resolveLiteralPath. It also records, by local type name,
// the receiver, parameters and results of each body-less func declaration
// (implemented in assembly), stripped of *, [] and [N].
func scanOtherBuild(path string) otherBuildScan {
	src, err := os.ReadFile(path)
	if err != nil || neverBuilt(src) {
		return otherBuildScan{}
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution|parser.ParseComments)
	if err != nil {
		return otherBuildScan{}
	}
	found := make(map[string]string)
	// visit records lit, of type typ or, for an elided literal (typ nil), of
	// the type path implied, and descends into elided element literals.
	var visit func(lit *ast.CompositeLit, typ ast.Expr, implied string)
	visit = func(lit *ast.CompositeLit, typ ast.Expr, implied string) {
		var key, elem ast.Expr
		switch t := ast.Unparen(typ).(type) {
		case *ast.ArrayType:
			elem = t.Elt
		case *ast.MapType:
			key, elem = t.Key, t.Value
		case nil:
		default:
			implied = literalTypeName(typ)
		}
		if implied != "" && len(lit.Elts) > 0 {
			if _, keyed := lit.Elts[0].(*ast.KeyValueExpr); !keyed {
				if _, seen := found[implied]; !seen {
					found[implied] = fset.Position(lit.Pos()).String()
				}
			}
		}
		// child visits an elided literal of syntactic type t or, when the
		// container's type is only known by name, of implied+step.
		child := func(e, t ast.Expr, step string) {
			c, ok := ast.Unparen(e).(*ast.CompositeLit)
			if !ok || c.Type != nil {
				return
			}
			switch {
			case t != nil:
				visit(c, t, "")
			case implied != "":
				visit(c, nil, implied+step)
			}
		}
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				child(kv.Key, key, "/k")
				e = kv.Value
			}
			child(e, elem, "/e")
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.CompositeLit); ok && lit.Type != nil {
			visit(lit, lit.Type, "")
		}
		return true
	})
	decls := make(map[string]ast.Expr)
	asm := make(map[string][]asmStub)
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				if ts := spec.(*ast.TypeSpec); ts.TypeParams == nil {
					decls[ts.Name.Name] = ts.Type
				}
			}
		case *ast.FuncDecl:
			if d.Body != nil {
				continue
			}
			stub := asmStub{at: fset.Position(d.Pos()).String(), link: linkTarget(f, d)}
			for _, fl := range []*ast.FieldList{d.Recv, d.Type.Params, d.Type.Results} {
				if fl == nil {
					continue
				}
				for _, field := range fl.List {
					if name := asmOperandName(field.Type); name != "" {
						asm[name] = append(asm[name], stub)
					}
				}
			}
		}
	}
	return otherBuildScan{pkg: f.Name.Name, lits: found, asm: asm, decls: decls}
}

// linkTarget returns the target of a two-argument `//go:linkname fn target`
// directive in f naming fn, a body-less top-level function, or "". Only a
// target in fn's own package (selfLinked) is Go compiled against the real
// layout; a one-argument directive (assembly, or a push to a package keeping
// its own copy of the struct) and a pull from another package (plugin.doInit
// ↔ runtime.initTask) are layout contracts like assembly. A linkname cannot
// name a method, so a method never has one. The doc comment first; gofmt
// sometimes detaches a directive, so then every comment of f.
func linkTarget(f *ast.File, fn *ast.FuncDecl) string {
	if fn.Recv != nil {
		return ""
	}
	target := func(g *ast.CommentGroup) string {
		for _, c := range g.List {
			rest, ok := strings.CutPrefix(c.Text, "//go:linkname ")
			if args := strings.Fields(rest); ok && len(args) > 1 && args[0] == fn.Name.Name {
				return args[1]
			}
		}
		return ""
	}
	if fn.Doc != nil {
		if t := target(fn.Doc); t != "" {
			return t
		}
	}
	for _, g := range f.Comments {
		if t := target(g); t != "" {
			return t
		}
	}
	return ""
}

// selfLinked reports whether a linkTarget names a symbol of package pkg.
func selfLinked(pkg *types.Package, target string) bool {
	return strings.HasPrefix(target, pkg.Path()+".")
}

// asmOperandName returns the local type name an assembly operand of type t
// reaches through pointers, slices, arrays and a variadic ..., or "".
func asmOperandName(t ast.Expr) string {
	for {
		switch tt := ast.Unparen(t).(type) {
		case *ast.StarExpr:
			t = tt.X
		case *ast.ArrayType:
			t = tt.Elt
		case *ast.Ellipsis:
			t = tt.Elt
		default:
			return literalTypeName(t)
		}
	}
}

// resolveLiteralPath resolves a scanOtherBuild type path to the structs its
// literal may build: through the type the name has in pkg's scope and through
// each declaration of it in the excluded files (decls), since those files'
// own types are not in the scope (type table []T; table{{...}}). A field's
// key is ignored: only array, slice and map literals elide their elements'
// types.
func resolveLiteralPath(pkg *types.Package, path string, decls map[string][]ast.Expr) []*types.Struct {
	return resolvePath(pkg, path, decls, make(map[string]bool))
}

// resolvePath is resolveLiteralPath's walk. seen holds the paths already
// followed: a name has one declaration per excluded file, so a cycle of them
// (type A B; type B A — no build compiles it, but each file only needs its
// own) branched at every step, exponential in the files. A path seen before
// adds nothing: its structs are already in the result.
func resolvePath(pkg *types.Package, path string, decls map[string][]ast.Expr, seen map[string]bool) []*types.Struct {
	if seen[path] {
		return nil
	}
	seen[path] = true
	head, steps, _ := strings.Cut(path, "/")
	name, targs := head, []string(nil)
	if i := strings.IndexByte(head, '['); i >= 0 && strings.HasSuffix(head, "]") {
		name, targs = head[:i], strings.Split(head[i+1:len(head)-1], ",")
	}
	var out []*types.Struct
	add := func(sts ...*types.Struct) {
		for _, st := range sts {
			if !slices.Contains(out, st) {
				out = append(out, st)
			}
		}
	}
	if tn, ok := pkg.Scope().Lookup(name).(*types.TypeName); ok {
		add(resolveTypedPath(pkg, tn.Type(), steps, targs, decls, seen)...)
	}
	for _, e := range decls[name] {
		add(resolveDeclPath(pkg, e, steps, decls, seen)...)
	}
	return out
}

// resolveTypedPath follows steps from t, a type of pkg's scope written with
// type arguments targs (names), to the structs at its end. A step that lands
// on one of t's type parameters continues from the argument's name.
func resolveTypedPath(pkg *types.Package, t types.Type, steps string, targs []string, decls map[string][]ast.Expr, seen map[string]bool) []*types.Struct {
	var tparams *types.TypeParamList
	if a, ok := t.(*types.Alias); ok {
		tparams = a.TypeParams() // type GA[T any] = []T
	} else if n, ok := types.Unalias(t).(*types.Named); ok {
		tparams = n.TypeParams()
	}
	for {
		if tp, ok := t.(*types.TypeParam); ok {
			for i := range tparams.Len() {
				if tparams.At(i) == tp && i < len(targs) {
					next := targs[i]
					if steps != "" {
						next += "/" + steps
					}
					return resolvePath(pkg, next, decls, seen)
				}
			}
			return nil
		}
		var step string
		step, steps, _ = strings.Cut(steps, "/")
		if step == "" {
			if st := canonicalStructType(t); st != nil {
				return []*types.Struct{st}
			}
			return nil
		}
		u := t.Underlying()
		if p, ok := u.(*types.Pointer); ok {
			u = p.Elem().Underlying()
		}
		switch u := u.(type) {
		case *types.Slice:
			t = u.Elem()
		case *types.Array:
			t = u.Elem()
		case *types.Map:
			if t = u.Elem(); step == "k" {
				t = u.Key()
			}
		default:
			return nil
		}
		// A pointer element is elided too ([]*T{{...}}).
		if p, ok := types.Unalias(t).(*types.Pointer); ok && steps == "" {
			t = p.Elem()
		}
	}
}

// resolveDeclPath follows steps through e, a type expression of an excluded
// file, continuing by name once it reaches one.
func resolveDeclPath(pkg *types.Package, e ast.Expr, steps string, decls map[string][]ast.Expr, seen map[string]bool) []*types.Struct {
	for {
		e = ast.Unparen(e)
		if star, ok := e.(*ast.StarExpr); ok {
			e = ast.Unparen(star.X)
		}
		var step string
		step, rest, _ := strings.Cut(steps, "/")
		switch t := e.(type) {
		case *ast.ArrayType:
			if step == "" {
				return nil
			}
			e, steps = t.Elt, rest
			continue
		case *ast.MapType:
			if step == "" {
				return nil
			}
			e, steps = t.Value, rest
			if step == "k" {
				e = t.Key
			}
			continue
		}
		name := literalTypeName(e)
		if name == "" {
			return nil
		}
		if steps != "" {
			name += "/" + steps
		}
		return resolvePath(pkg, name, decls, seen)
	}
}

// neverBuilt reports whether src opts out of every build with the
// conventional `//go:build ignore`: unlike a GOOS or an unset tag, no build
// ever compiles it, so its literals can't break.
func neverBuilt(src []byte) bool {
	for line := range strings.Lines(string(src)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			return false
		}
		if constraint.IsGoBuild(line) {
			expr, err := constraint.Parse(line)
			return err == nil && expr.String() == "ignore"
		}
	}
	return false
}

// literalTypeName returns the local type name a composite literal of type t
// (or of *t, elided in []*T{{...}}) builds, or "" for any other type. An
// instance keeps its type arguments when each is a plain name (List[T,U]),
// so an element of a type parameter resolves to the argument.
func literalTypeName(t ast.Expr) string {
	var indices []ast.Expr
	switch tt := ast.Unparen(t).(type) {
	case *ast.Ident:
		return tt.Name
	case *ast.StarExpr:
		return literalTypeName(tt.X)
	case *ast.IndexExpr:
		t, indices = tt.X, []ast.Expr{tt.Index}
	case *ast.IndexListExpr:
		t, indices = tt.X, tt.Indices
	default:
		return ""
	}
	name := literalTypeName(t)
	if name == "" || strings.Contains(name, "[") {
		return name
	}
	args := make([]string, len(indices))
	for i, idx := range indices {
		args[i] = literalTypeName(idx)
		if args[i] == "" || strings.Contains(args[i], "[") {
			return name
		}
	}
	return name + "[" + strings.Join(args, ",") + "]"
}

// isStdPath reports whether path looks like a standard-library import path:
// no dot in its first element.
func isStdPath(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// fieldNamesKey joins st's field names in order, a necessary condition for
// types.Identical between structs, used to bucket identity candidates.
func fieldNamesKey(st *types.Struct) string {
	var b strings.Builder
	for f := range st.Fields() {
		b.WriteString(f.Name())
		b.WriteByte(0)
	}
	return b.String()
}

// isUnsafePointer reports whether t is unsafe.Pointer (through aliases).
func isUnsafePointer(t types.Type) bool {
	b, ok := types.Unalias(t).(*types.Basic)
	return ok && b.Kind() == types.UnsafePointer
}

// hasBlankPadding reports whether str has a blank field of non-zero size. A
// `_` field can't be read or written, so it exists only to shape the layout
// (cache-line pads against false sharing, alignment pads); sorting would move
// it and undo that. Zero-sized blank markers such as `_ [0]func()` or a noCopy
// guard carry no padding and don't count.
func hasBlankPadding(str *types.Struct, sizes *gcSizes) bool {
	for f := range str.Fields() {
		if f.Name() == "_" && sizes.Sizeof(f.Type()) > 0 {
			return true
		}
	}
	return false
}

// isASN1Struct reports whether str declares itself an ASN.1 SEQUENCE: a field
// tagged `asn1:"..."` or of an encoding/asn1 type (RawValue, RawContent,
// ObjectIdentifier, BitString, ...). Field order is then the wire format, and
// the encoder often lives in another package (crypto/x509 encodes
// crypto/x509/pkix types) where its call site can't pin this struct — so the
// struct is left alone in its own package, like structs.HostLayout.
func isASN1Struct(str *types.Struct) bool {
	var visited map[*types.Named]bool
	return asn1Struct(str, &visited)
}

// asn1Struct is isASN1Struct's walk: str's own fields (an encoding/asn1
// type counts directly or as an array or slice element), then the structs it
// holds by value — fields, arrays and slice elements (SEQUENCE OF), not
// pointers — since a holder of an ASN.1 struct is encoded with it and usually
// reaches the codec through an interface{} (x/crypto/pkcs12's encryptedData),
// where no call site pins it. visited, shared by the whole walk, stops on
// recursive types; the first hit ends it.
func asn1Struct(str *types.Struct, visited *map[*types.Named]bool) bool {
	for i := range str.NumFields() {
		if _, ok := reflect.StructTag(str.Tag(i)).Lookup("asn1"); ok {
			return true
		}
		for t := types.Unalias(str.Field(i).Type()); !revisits(t, visited); {
			if isASN1Type(t) {
				return true
			}
			switch u := t.Underlying().(type) {
			case *types.Array:
				t = types.Unalias(u.Elem())
				continue
			case *types.Slice:
				t = types.Unalias(u.Elem())
				continue
			case *types.Struct:
				if asn1Struct(u, visited) {
					return true
				}
			}
			break
		}
	}
	return false
}

// isASN1Type reports whether t is an encoding/asn1 named type or a pointer
// to one: as a field, an array or a slice element, it marks ASN.1.
func isASN1Type(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	named, ok := t.(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "encoding/asn1"
}

// hasSSHTag reports whether a field of str is tagged for x/crypto/ssh's
// encoder (`ssh:"rest"`, `sshtype:"90"`). As with ASN.1, field order is then
// the wire format, and the value often reaches ssh.Marshal through an
// interface or from another package, where its call site can't pin it.
func hasSSHTag(str *types.Struct) bool {
	for i := range str.NumFields() {
		tag := reflect.StructTag(str.Tag(i))
		if _, ok := tag.Lookup("ssh"); ok {
			return true
		}
		if _, ok := tag.Lookup("sshtype"); ok {
			return true
		}
	}
	return false
}

// hasHostLayout reports whether str has a direct structs.HostLayout field,
// which declares its layout fixed to the platform C ABI. Per the structs
// package, the marker applies only to the struct directly containing it, so
// nested structs aren't searched.
func hasHostLayout(str *types.Struct) bool {
	for f := range str.Fields() {
		named, ok := types.Unalias(f.Type()).(*types.Named)
		if !ok {
			continue
		}
		obj := named.Obj()
		if obj.Name() == "HostLayout" && obj.Pkg().Path() == "structs" {
			return true
		}
	}
	return false
}

// deferToTestVariant reports whether this pass should skip -apply and leave the
// rewrite to the in-package test variant.
//
// The base pass can't see in-package *_test.go files, so a positional literal
// there is invisible; it must not rewrite a struct such a literal pins. The
// [p.test] variant sees those tests and owns the rewrite. Only -apply is
// deferred, not reporting: the base pass still reports, so healthy non-test code
// stays covered even when the driver skips the variant as ill-typed. The
// trade-off is a possible duplicate diagnostic (base plain, variant caveated)
// for a struct pinned by a test literal.
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
// *token.FileSet, value func() map[dir][]testFileEntry from sync.OnceValue):
// deferToTestVariant runs on every pass over the shared FileSet, so the walk
// must not repeat per package. Entries live for the process lifetime, which
// suits the CLI and golangci-lint.
var testVariantCache sync.Map // map[*token.FileSet]func() map[string][]testFileEntry

// testFilesByDir groups fset's *_test.go files by directory, building the map
// exactly once per FileSet. The once matters: passes start concurrently, and a
// plain check-then-store let every one of them miss together and re-parse every
// test file (85% of a report-only stdlib run). The FileSet is fully populated
// before analysis, so the scan is a safe read. Unparsable files are dropped.
func testFilesByDir(fset *token.FileSet) map[string][]testFileEntry {
	scan, _ := testVariantCache.LoadOrStore(fset, sync.OnceValue(func() map[string][]testFileEntry {
		return scanTestFilesFn(fset)
	}))
	return scan.(func() map[string][]testFileEntry)()
}

// scanTestFilesFn indirects scanTestFiles so tests can count builds.
var scanTestFilesFn = scanTestFiles

// scanTestFiles is testFilesByDir's uncached body.
func scanTestFiles(fset *token.FileSet) map[string][]testFileEntry {
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
	return byDir
}

// revisits reports whether t is a defined type the walk already passed,
// recording it otherwise. A defined type can be its own element (type S []S,
// type P *P), so a loop unwrapping pointers or slices needs it to terminate;
// every such cycle runs through a defined type. Instances are keyed by their
// origin, so the map is allocated only when a defined type is seen.
func revisits(t types.Type, visited *map[*types.Named]bool) bool {
	n, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	n = n.Origin()
	if (*visited)[n] {
		return true
	}
	if *visited == nil {
		*visited = make(map[*types.Named]bool)
	}
	(*visited)[n] = true
	return false
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

// structsOf returns the struct canonicalStructType collapses t onto or, for a
// type parameter, every struct of its type set (P{...} and P(x) build or
// convert each of them: func Mk[P interface{ S }]() P { return P{1, 2} }).
func structsOf(t types.Type) []*types.Struct {
	tp, ok := types.Unalias(t).(*types.TypeParam)
	if !ok {
		if st := canonicalStructType(t); st != nil {
			return []*types.Struct{st}
		}
		return nil
	}
	var out []*types.Struct
	seen := make(map[*types.Interface]bool)
	var walk func(iface *types.Interface)
	walk = func(iface *types.Interface) {
		if seen[iface] {
			return
		}
		seen[iface] = true
		for e := range iface.EmbeddedTypes() {
			terms := []types.Type{e}
			if u, ok := e.(*types.Union); ok {
				terms = terms[:0]
				for i := range u.Len() {
					terms = append(terms, u.Term(i).Type())
				}
			}
			for _, term := range terms {
				if in, ok := term.Underlying().(*types.Interface); ok {
					walk(in)
				} else if st := canonicalStructType(term); st != nil && !slices.Contains(out, st) {
					out = append(out, st)
				}
			}
		}
	}
	if iface, ok := tp.Constraint().Underlying().(*types.Interface); ok {
		walk(iface)
	}
	return out
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
// allocating a permutation (that's only needed to rewrite; see optimalOrder).
// Already-optimal structs dominate, so keeping the alloc off this path makes the
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
// and ptrdata. It is the rewrite path, run only for a misaligned rewritable
// struct; the optimality check uses layoutMetrics and skips this allocation. indexes is always a full permutation of
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
		for field := range u.Fields() {
			if typeParamInLayout(field.Type(), seen) {
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
	// rewritten, when set, reports whether the run will reorder the struct
	// literal decl declares (a defined type's or a type alias's); such a
	// struct is then measured at its post-reorder size and ptrdata (see
	// projected). nil measures every type as declared.
	rewritten func(decl *types.Struct) bool
	WordSize  int64
	MaxAlign  int64
}

// projected returns the size and ptrdata struct type T will have once this
// run reorders it, when s.rewritten predicts that it will. An enclosing struct
// is then laid out against the inner struct's final layout, not its current
// one, so one run reaches the fixed point: measured as declared, the outer
// order was chosen for an inner layout the same run changed, and the next run
// moved it again (gorilla/mux's Router around routeConf; x/tools took four
// runs to settle). T is a defined type or a type alias of a struct literal
// (both declarations are rewritten); the decision is taken on the generic
// declaration, the figures on T itself. Alignment needs no projection: a
// reorder keeps the max.
func (s *gcSizes) projected(T types.Type) (size, ptrdata int64, ok bool) {
	if s.rewritten == nil {
		return 0, 0, false
	}
	var st, decl *types.Struct
	switch t := types.Unalias(T).(type) {
	case *types.Named:
		// Also reached through an alias of a defined type (type A = X).
		st, _ = t.Underlying().(*types.Struct)
		decl, _ = t.Origin().Underlying().(*types.Struct)
	case *types.Struct:
		// Only through an alias of a struct literal (type A = struct{...}),
		// whose declaration is rewritten; a literal written in place is not.
		if a, ok := T.(*types.Alias); ok {
			st = t
			decl, _ = types.Unalias(a.Origin()).(*types.Struct)
		}
	}
	if st == nil || decl == nil || !s.rewritten(decl) {
		return 0, 0, false
	}
	if decl != st {
		// An instance: its declaration must be a candidate (not laid out by
		// a type parameter) before the instance's own figures count.
		if ok, _, _ := rewriteCandidate(decl, s); !ok {
			return 0, 0, false
		}
	}
	ok, size, ptrdata = rewriteCandidate(st, s)
	return size, ptrdata, ok
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

// isAlign64 reports whether t is the compiler's align64 marker: a field of it
// makes the enclosing struct 8-byte aligned (cmd/compile's types.CalcSize
// recognises it only in sync/atomic and the runtime's atomic package).
func isAlign64(t types.Type) bool {
	n, ok := types.Unalias(t).(*types.Named)
	if !ok || n.Obj().Name() != "align64" || n.Obj().Pkg() == nil {
		return false
	}
	switch n.Obj().Pkg().Path() {
	case "sync/atomic", "internal/runtime/atomic", "runtime/internal/atomic":
		return true
	}
	return false
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
			if isAlign64(t.Field(i).Type()) {
				// gc forces 8-byte alignment past MaxAlign for atomic.Int64
				// and friends on 32-bit targets.
				if max < 8 {
					max = 8
				}
				continue
			}
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
	if size, _, ok := s.projected(T); ok {
		return size
	}
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
	if _, ptrdata, ok := s.projected(T); ok {
		return ptrdata
	}
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
// Whatever ast.IsGenerated accepts (`// Code generated from x.proto. DO NOT
// EDIT.`, no "by") counts too: the driver refuses fixes there, so -fix would
// silently skip a file -apply rewrites.
func hasGeneratedComment(file *ast.File) bool {
	if ast.IsGenerated(file) {
		return true
	}
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

// declRewritable reports whether the struct literal decl is declared where its
// own package's pass would rewrite it, judged only from facts every pass sees
// alike: that package is in the run, its declaring file passes the run's file
// filters (skipFile, the generated-code header), and the declaration is
// multi-line without a betteralign:ignore. Pins and deferral are deliberately
// not consulted — only the declaring package can see them. Layout is the
// caller's part (rewriteCandidate).
func declRewritable(pass *analysis.Pass, decl *types.Struct, skipFile func(string) bool, generatedFiles bool) bool {
	if decl.NumFields() == 0 {
		return false
	}
	// The first field locates the literal and its package, also for a type
	// defined over another (type X otherpkg.Y) or an alias of one.
	first := decl.Field(0)
	if first.Pkg() == nil || first.Pkg() != pass.Pkg && !sameRun(pass, first) {
		return false
	}
	// Unadjusted: the text is read from the file really holding the
	// declaration, not the one a //line directive names. Not .go means cgo's
	// output in the build cache, which run never rewrites; not absolute, a
	// trimmed path ($GOROOT/...) from export data.
	pos := pass.Fset.PositionFor(first.Pos(), false)
	fn := pos.Filename
	if !filepath.IsAbs(fn) || !strings.HasSuffix(fn, ".go") || pos.Line < 1 || skipFile(fn) {
		return false
	}
	d, src, ok := declAt(pass, decl)
	// A cgo file is never rewritten, -generated_files or not (see run).
	if !ok || src.cgo || !generatedFiles && src.generated {
		return false
	}
	return d.reorderable
}

// declAt finds the struct literal decl in the source its type information
// describes, so declRewritable, declDecorates and declOptedIn judge the same
// text. Candidates, in order: the file on disk, then its pristine bytes
// (pristineSource). One is accepted when it declares a struct whose first
// field has decl's first field's name on that field's unadjusted line, and —
// except under a vettool — has the size of the token.File holding decl.
//
// Disk first: after an edit in the same process the disk is current (the
// pristine entry is stale) and matches; after another pass's concurrent
// rewrite the moved struct no longer matches the disk, so the pristine bytes,
// stored before the write, are used; a struct the rewrite left in place
// matches both and both say the same. Pristine is loaded after the disk is
// read, so a read that raced the rewrite still finds it.
//
// The size rejects export data under the drivers that parse every analyzed
// package from source (singlechecker, golangci-lint): gcimporter's fake
// token.File (64 KiB, one-byte lines) means the package is not analyzed, so
// its pass never rewrites anything. Under go vet (unitcheckerDriver) every
// import is export data and the partner is analyzed by its own process
// whenever it is in the vet pattern, so the text alone decides; vetting a
// subset of a module leaves partners outside the pattern unrewritten while
// the local side follows them, as in v0.15.4.
// ponytail: size only; a real file of exactly gcimporter's fake size (64 KiB)
// passes, and is then judged by its text.
func declAt(pass *analysis.Pass, decl *types.Struct) (declInfo, *declSource, bool) {
	if decl.NumFields() == 0 {
		return declInfo{}, nil, false
	}
	first := decl.Field(0)
	tf := pass.Fset.File(first.Pos())
	if tf == nil {
		return declInfo{}, nil, false
	}
	pos := pass.Fset.PositionFor(first.Pos(), false)
	accept := func(ds *declSource) (declInfo, bool) {
		if ds == nil || !unitcheckerDriver() && int64(tf.Size()) != ds.size {
			return declInfo{}, false
		}
		return ds.at(pos.Line, first.Name())
	}
	if ds := readDeclSource(pos.Filename); ds != nil {
		if d, ok := accept(ds); ok {
			return d, ds, true
		}
	}
	if ds := pristineDeclSource(pos.Filename); ds != nil {
		if d, ok := accept(ds); ok {
			return d, ds, true
		}
	}
	return declInfo{}, nil, false
}

// vetGeneratedNote prints run's -fix -generated_files note once per process.
var vetGeneratedNote sync.Once

// unitcheckerDriver reports whether this process is a vettool (go vet
// -vettool, which runs one process per package). Computed once; a variable
// so tests can override it.
var unitcheckerDriver = sync.OnceValue(func() bool {
	return flag.Parsed() && vettoolArgs(flag.Args())
})

// vettoolArgs mirrors the x/tools singlechecker/multichecker hand-off to
// unitchecker: go vet runs `vettool <analyzer flags> <path>.cfg`.
func vettoolArgs(args []string) bool {
	return len(args) == 1 && strings.HasSuffix(args[0], ".cfg")
}

// declSource is a declaring file as declRewritable reads it.
type declSource struct {
	// decorated is built on first use: only conversion partners need it.
	decorated func() map[int]bool
	// decls maps the unadjusted first-field line of each struct declared by
	// a type spec (the first, where one line starts several) to what run
	// would do with it.
	decls map[int]declInfo
	// size is the length of the bytes parsed.
	size      int64
	generated bool
	cgo       bool
}

// declInfo is one struct declaration as run sees it.
type declInfo struct {
	// field is the first field's first name, "" for an embedded field.
	field string
	// reorderable: run may rewrite it — the body does not start on the
	// opening-brace line (dstmin refuses single-line structs), no
	// betteralign:ignore (hasIgnoreCommentAST) and no //line directive in
	// the file (lineDirective).
	reorderable bool
	optedIn     bool
}

// declSourceCache memoises readDeclSource by path, size and modification
// time, so a long-running host sees an edited file afresh.
var declSourceCache sync.Map // map[declSourceKey]*declSource

type declSourceKey struct {
	name    string
	size    int64
	modTime int64
}

// pristineSource holds, per path, a file's bytes from before -apply rewrote
// it. Type information — this pass's and the export data's — describes those
// bytes, while passes run concurrently: a pass predicting a partner declared
// in a file another pass already rewrote would otherwise read the reordered
// struct (another first field, shifted lines) and mispredict. Keyed by path,
// not parse: golangci-lint parses each package variant itself, so the
// importer's token.File is not the writer's. declAt decides when an entry
// applies.
var pristineSource sync.Map // map[string]*pristineEntry

// pristineEntry is a file's pre-rewrite bytes, parsed on first use.
type pristineEntry struct {
	ds  func() *declSource
	src []byte
}

// storePristine records src as fn's pre-rewrite text before -apply writes
// fn. The latest rewrite wins, except that an entry with the same bytes is
// kept (p and p [p.test] may both rewrite from one parse).
func storePristine(fn string, src []byte) {
	if v, ok := pristineSource.Load(fn); ok && bytes.Equal(v.(*pristineEntry).src, src) {
		return
	}
	pristineSource.Store(fn, &pristineEntry{
		src: src,
		ds:  sync.OnceValue(func() *declSource { return newDeclSource(fn, src) }),
	})
}

// pristineDeclSource returns fn's pristine bytes parsed, or nil.
func pristineDeclSource(fn string) *declSource {
	if v, ok := pristineSource.Load(fn); ok {
		return v.(*pristineEntry).ds()
	}
	return nil
}

// readDeclSource loads and parses fn as it is on disk; an unreadable file
// yields nil.
func readDeclSource(fn string) *declSource {
	fi, err := os.Stat(fn)
	if err != nil {
		return nil
	}
	key := declSourceKey{fn, fi.Size(), fi.ModTime().UnixNano()}
	if v, ok := declSourceCache.Load(key); ok {
		return v.(*declSource)
	}
	src, err := os.ReadFile(fn)
	if err != nil {
		return nil
	}
	v, _ := declSourceCache.LoadOrStore(key, newDeclSource(fn, src))
	return v.(*declSource)
}

// newDeclSource judges src's declarations with the predicates run applies to
// its own AST, so the prediction and the rewrite can't drift apart.
func newDeclSource(fn string, src []byte) *declSource {
	ds := &declSource{decls: make(map[int]declInfo), size: int64(len(src))}
	ds.decorated = sync.OnceValue(func() map[int]bool { return decoratedStructs(fn, src) })
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, fn, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return ds
	}
	ds.generated = hasGeneratedComment(f)
	ds.cgo = slices.ContainsFunc(f.Imports, func(s *ast.ImportSpec) bool { return s.Path.Value == `"C"` })
	fileHeld := lineDirective(fset, f)
	ast.Inspect(f, func(n ast.Node) bool {
		g, ok := n.(*ast.GenDecl)
		if !ok || g.Tok != token.TYPE {
			return true
		}
		for _, spec := range g.Specs {
			ts := spec.(*ast.TypeSpec)
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil || len(st.Fields.List) == 0 {
				continue
			}
			first := st.Fields.List[0]
			line := fset.PositionFor(first.Pos(), false).Line
			if _, seen := ds.decls[line]; seen {
				continue
			}
			var name string
			if len(first.Names) > 0 {
				name = first.Names[0].Name
			}
			ds.decls[line] = declInfo{
				field: name,
				reorderable: !fileHeld && line != fset.PositionFor(st.Fields.Opening, false).Line &&
					!hasIgnoreCommentAST(fset, f, st),
				optedIn: declaredWithOptInComment(g) || commentGroupHasOptIn(ts.Doc),
			}
		}
		return true
	})
	return ds
}

// at returns the declaration whose first field, named field, starts on line;
// ok is false when the file declares no struct there, or one whose first
// field has another name (a stale or //line-shifted position).
func (ds *declSource) at(line int, field string) (declInfo, bool) {
	d, ok := ds.decls[line]
	if !ok || d.field != "" && d.field != field {
		return declInfo{}, false
	}
	return d, true
}

// lineDirective reports whether f has a line directive, as go/scanner reads
// them: /*line anywhere, //line only at the start of a line (an indented
// "//line file:line" in a doc comment is text). run rewrites no struct in
// such a file: moving lines would break the mapping to the source the
// directive names, and another package sees the file's declarations only at
// those mapped positions (export data keeps no others), so it couldn't tell
// whether they move.
func lineDirective(fset *token.FileSet, f *ast.File) bool {
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			text, block := strings.CutPrefix(c.Text, "/*line ")
			if block {
				text = strings.TrimSuffix(text, "*/")
			} else if rest, ok := strings.CutPrefix(c.Text, "//line "); ok && fset.PositionFor(c.Slash, false).Column == 1 {
				text = rest
			} else {
				continue
			}
			// go/scanner ignores one not ending in :line (or :line:col).
			i := strings.LastIndexByte(text, ':')
			if i >= 0 && i < len(text)-1 && strings.Trim(text[i+1:], "0123456789") == "" {
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
// loud, not silent. So does a malformed glob, which isExcluded would treat as
// excluding every file: a run reporting nothing would pass a CI gate.
func normalizeExcludePaths(wd string, paths []string) ([]string, error) {
	out := paths
	if slices.ContainsFunc(paths, func(p string) bool { return filepath.IsAbs(p) || filepath.Clean(p) != p }) {
		out = make([]string, len(paths))
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
	}
	// Validate what isExcluded matches: cleaning can break a valid pattern
	// (foo\/ becomes foo\, a trailing escape).
	for i, p := range out {
		if _, err := filepath.Match(p, ""); err != nil {
			return nil, fmt.Errorf("exclude pattern %q: %w", paths[i], err)
		}
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
