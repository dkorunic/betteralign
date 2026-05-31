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

// This is a fork of fieldalignment tool (https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/fieldalignment)
// patched to use DST (Decorated Syntax Tree) to support floating comments, field comments, preserve multiple line
// spacing etc.
// Vast majority of the alignment calculation code from fieldalignment (and maligned) has remained the same, except for
// using DST and handling suggested fixes. With DST we cannot print out a single node and all decorations easily, so in
// apply mode we are printing whole DST with alignment fixes into a file. Fix mode sadly doesn't do anything as we are
// not using SuggestedFixes for partial rewrite.
// To avoid DST panics due to node info reuse present in the original code, some logic from structslop
// (https://github.com/orijtech/structslop) was also borrowed.
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
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"math"
	"os"
	"path/filepath"
	"slices"
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

has 8 because it can stop immediately after the string pointer.

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

If a struct is constructed somewhere in the package with a positional
composite literal (` + "`T{1, 2, 3}`" + ` rather than ` + "`T{a: 1, b: 2, c: 3}`" + `),
the reorder is reported but never applied: rewriting the field order would
re-map the literal's elements to different fields, breaking the build (or
worse, silently mis-assigning values when the new field types still happen
to accept the old element types). Convert the literal to keyed form and
rerun to enable the reorder.

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

// String renders the accumulated entries through fmt's %v formatter. Exists
// to satisfy flag.Value (which mandates a String method even on flags that
// are pure accumulators); the format is intended for human-readable usage
// output, not for round-tripping through Set.
func (f *StringArrayFlag) String() string {
	return fmt.Sprintf("%v", *f)
}

// Set appends one or more values from a single -flag occurrence onto the
// accumulator. Comma-separated lists are split so `--exclude_dirs a,b,c`
// and three repetitions of the same flag are equivalent — the form was
// established by the original fieldalignment CLI and is preserved here for
// drop-in compatibility. Empty entries (from stray leading/trailing commas
// or `--exclude_dirs ,,`) are dropped silently so users don't get spurious
// "" matches. Never returns an error today; the signature is dictated by
// flag.Value, not by anticipated failures. Not safe for concurrent calls;
// run.cfg.excludeDirs/excludeFiles are clones at run-start to defend
// against host frameworks that mutate flag values in parallel.
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

// InitAnalyzer wires a fresh analyzerConfig into analyzer so each Analyzer
// instance owns independent flag state. Exists because the original
// fieldalignment used package-level flags, which conflicted when multiple
// analyzers ran in parallel (the analysistest suite in particular, and
// hosts like golangci-lint that run several analyzers in one process).
// The function is exported so external drivers can build their own
// Analyzer through `*analysis.Analyzer{Name:..., Doc:..., Requires:...}`
// and then call InitAnalyzer to attach the betteralign flag set without
// going through singlechecker.Main.
//
// Idempotent: re-init on an Analyzer whose FlagSet already carries the
// "apply" flag is a no-op rather than a panic. flag.FlagSet.BoolVar
// os.Exit(2)s on duplicate flag names, which would take down the host
// process; the lookup guard turns that into a benign re-entry. Not safe
// for concurrent calls on the SAME Analyzer; safe across distinct
// Analyzers (each owns its own FlagSet).
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

// init wires the package-level Analyzer through the shared InitAnalyzer
// path so direct importers of `betteralign.Analyzer` (the canonical
// integration shape) see the flag set without having to remember to call
// InitAnalyzer themselves. External drivers that construct their own
// Analyzer never hit this path.
func init() {
	InitAnalyzer(Analyzer)
}

// run is the analyzer entry point that go/analysis invokes once per
// package. It rides the *inspector.Inspector that inspect.Analyzer
// already built for this pass, so the traversal cost is shared across
// any other analyzers participating in the same suite (vet, gopls, etc).
// Reporting is decoupled from rewriting: diagnostics (including the
// betteralign:ignore opt-out and the positional-literal check) come purely
// from AST + type info, so report-only mode never reads source bytes from
// disk and never builds DST trees, and a clean codebase does only AST work.
// DST decoration is built lazily and only under -apply, since only the
// rewrite needs it; a misaligned struct dstmin cannot decorate is therefore
// still reported, just not rewritten.
//
// The diagnostic-only path runs even when -apply is unset, so editors and
// linters get the report; -apply (and its legacy alias -fix) gates the
// rewrite step. SuggestedFixes are intentionally left nil — DST cannot
// serialise a single node without losing its comment decorations, so the
// rewrite has to emit the whole file (see the package doc for the
// trade-off discussion). When -apply does run, each dirty file is
// re-parsed after Fprint to verify the bytes form valid Go; an
// unparseable result skips the write rather than corrupting the source.
//
// Returns (nil, nil) on success. Non-nil error wraps ErrPreFilterFiles
// when -exclude_dirs / -exclude_files normalisation fails; per-file
// failures (decoration, formatting, write) are logged to stderr and the
// pass continues with the rest, since the caller (singlechecker, host
// driver) treats an analyzer-level error as fatal for the whole pass.
//
// pass is provided by the framework and must not be retained beyond the
// call. Concurrency: go/analysis invokes one run per pass and may invoke
// multiple passes concurrently; each Pass owns its own context, so this
// function is safe to call concurrently against distinct passes. The
// excludeDirs/excludeFiles slices are cloned defensively because hosts
// like golangci-lint may share flag state across passes.
func (cfg *analyzerConfig) run(pass *analysis.Pass) (any, error) {
	apply := cfg.apply
	if a := pass.Analyzer.Flags.Lookup("fix"); a != nil && a.Value.String() == "true" {
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
			currentFn = pass.Fset.File(f.Pos()).Name()
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

		// Compute optimality without DST; clean files pay no decoration cost.
		indexes, optsz, optptrs := optimalOrder(typ, sizes)
		// Identity permutation means the original layout is already optimal.
		if isIdentityOrder(indexes) {
			return
		}
		var message string
		if sz := sizes.Sizeof(typ); sz != optsz {
			message = fmt.Sprintf("%d bytes saved: struct of size %d could be %d", sz-optsz, sz, optsz)
		} else if ptrs := sizes.ptrdata(typ); ptrs != optptrs {
			message = fmt.Sprintf("%d bytes saved: struct with %d pointer bytes could be %d", ptrs-optptrs, ptrs, optptrs)
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

		// Decorate this file once; never retry on failure.
		if _, failed := decorationFailed[currentFn]; failed {
			return
		}
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

// collectPositionalUsers indexes every *types.Struct whose layout is pinned
// by a positional composite literal (`T{1, 2, 3}` rather than the keyed
// form). Reordering such a struct would silently re-map literal elements to
// different fields, so the diagnostic path consults this set before
// rewriting. The recorded position is the first offending literal; the
// diagnostic surfaces it so the user gets a jump target straight to the
// code blocking the reorder.
//
// Per-file skip flags (-exclude_files, -exclude_dirs, -test_files,
// -generated_files) deliberately do NOT apply here: excluded source still
// compiles against the struct's layout, so a reorder of the analysed
// declaration would break the excluded literal just as it would break an
// analysed one. Treating excluded files as evidence of "this struct is
// pinned" is the safe-by-default behaviour.
//
// Go's grammar forbids mixing keyed and positional elements in the same
// literal, so inspecting Elts[0] classifies the whole literal — the same
// shortcut go vet's "composites" pass uses. Walk cost is O(literals);
// canonicalStructType keys multiple syntactic forms (named, generic
// instantiation, pointer-elided, alias chain) onto the same struct so
// the map stays small.
//
// Returns a fresh map; safe to call concurrently against distinct passes,
// not safe to share the returned map across concurrent mutations.
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

// canonicalStructType collapses every syntactic form of a struct reference
// onto the single *types.Struct the alignment visitor sees when it lands on
// the corresponding *ast.StructType, so collectPositionalUsers and the main
// visitor key against identical pointers. Returns nil when t does not
// denote a struct (the caller's "not interesting" signal).
//
// Four forms collapse together:
//
//   - plain named structs (`T{...}`)                → T's struct body
//   - chained type definitions (`type S T`)          → T's struct body (shared)
//   - generic instantiations (`Box[int]{...}`)       → the generic origin's body
//   - pointer-typed elided literals (`[]*T{{...}}`)  → T's struct body
//
// Type aliases (`type A = T`) are stripped via types.Unalias first, so the
// chain looks through `type A = []*B` correctly. Anonymous struct literals
// land in the *types.Struct case and key themselves; they cannot match any
// analyser candidate (the visitor only reports named structs) but storing
// them is harmless and avoids a special case for "skip anonymous".
//
// The *types.Pointer arm is load-bearing for elided composite literals
// inside slice/array/map types whose element type is a pointer. In
// `[]*T{{1,2,3}}` the type-checker records the inner literal as `*T`, not
// `T`; without the unwrap step canonicalStructType would return nil and
// the analyser would happily reorder T's fields, breaking the build the
// next time someone compiled the file. Pure function; no allocations.
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

// lookupUnsafePointer resolves unsafe.Pointer's canonical types.Type for
// pass.TypesSizes lookups so the analyzer probes the target architecture
// rather than hard-coding 64-bit sizes. Returns nil only if a future Go
// release ever removes `unsafe.Pointer` from the unsafe package's scope,
// which has been stable for the entire history of the language but the
// guard costs nothing and lets run fall back to 8-byte defaults instead
// of crashing. Memoised at init in unsafePointerTyp; pure on every call
// since types.Unsafe is a process-wide singleton.
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

// optimalOrder is the core layout algorithm: it sorts fields by the
// alignment-first / GC-ptrdata-minimising comparator and computes the
// resulting struct's size and ptrdata in the same pass. Computing
// size/ptrdata here, while we already hold the sorted fieldElem slice,
// saves a follow-up `sizes.Sizeof(rebuiltStruct)` / `sizes.ptrdata(...)`
// pair that would re-walk the same fields — the rebuild would also need
// to allocate a fresh *types.Struct, which go/types does not support
// directly.
//
// Sort order, in priority:
//
//  1. zero-sized fields first (they collapse alignment requirements);
//  2. higher alignment first (fills natural-alignment slots);
//  3. pointer-bearing fields before pointer-free (front-loads GC scan);
//  4. fewer trailing non-pointer bytes among pointerful fields (shrinks
//     ptrdata to the smallest GC scan prefix);
//  5. larger size first as the final tiebreaker.
//
// The sort is stable, so betteralign reports identity permutations when
// the original order is already optimal under the comparator — that's
// what isIdentityOrder checks for in the caller. fieldElem scratch
// slices are recycled through elemsPool so the backing array survives
// across optimalOrder calls in the same pass; the returned indexes
// slice is freshly allocated because callers retain it past return.
//
// Pure with respect to str and sizes (the sizes caches are populated but
// the result for any given type is stable). Safe to call concurrently
// only if each goroutine has its own *gcSizes — the cache maps are not
// concurrency-safe.
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

	for i := range nf {
		field := str.Field(i)
		ft := field.Type()
		elems[i] = fieldElem{
			alignof: sizes.Alignof(ft),
			sizeof:  sizes.Sizeof(ft),
			ptrdata: sizes.ptrdata(ft),
			index:   i,
		}
	}

	slices.SortStableFunc(elems, func(a, b fieldElem) int {
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
	})

	indexes = make([]int, nf)
	var offset int64
	maxAlign := int64(1)
	for i, e := range elems {
		indexes[i] = e.index
		if e.alignof > maxAlign {
			maxAlign = e.alignof
		}
		offset = align(offset, e.alignof)
		if e.ptrdata != 0 {
			optPtrdata = addSize(offset, e.ptrdata)
		}
		sz := e.sizeof
		// Trailing zero-size field on a non-empty struct gets one padding byte.
		if i == nf-1 && sz == 0 && offset != 0 {
			sz = 1
		}
		offset = addSize(offset, sz)
	}
	optSize = align(offset, maxAlign)
	return indexes, optSize, optPtrdata
}

// isIdentityOrder is the "the existing layout is already optimal" guard.
// Because optimalOrder uses a stable sort, the output preserves the
// original order whenever the comparator can't distinguish two fields,
// and matches it across the whole slice when the original is optimal.
// The analyzer uses this single comparison instead of recomputing Sizeof
// and ptrdata against str to skip diagnostic emission entirely when no
// improvement is available. O(N) over the index slice; never errors.
func isIdentityOrder(indexes []int) bool {
	for i, idx := range indexes {
		if idx != i {
			return false
		}
	}
	return true
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

// newGCSizes constructs a *gcSizes pre-seeded with empty memoisation
// caches. Mandatory for any caller that runs Alignof/Sizeof/ptrdata in
// bulk (the analyzer pass) so the per-pass cost of recurring sub-types
// (very common in generated protobuf or grpc code) stays linear in unique
// types rather than reachable nodes.
//
// Tests may also build a *gcSizes via a plain struct literal with nil
// caches; the Alignof/Sizeof/ptrdata helpers detect nil and degrade to
// uncached computation, accepting the perf cost in exchange for the
// ability to assert on cache-free behaviour. Not safe for concurrent use
// — the cache maps are plain `map[types.Type]int64`. Allocate one per
// goroutine if parallelising.
func newGCSizes(wordSize, maxAlign int64) *gcSizes {
	return &gcSizes{
		WordSize:   wordSize,
		MaxAlign:   maxAlign,
		sizeCache:  make(map[types.Type]int64),
		alignCache: make(map[types.Type]int64),
		ptrCache:   make(map[types.Type]int64),
	}
}

// Alignof reports T's alignment in bytes as the Go GC observes it,
// memoising the answer so a single pass over recurring sub-types stays
// cheap. The crucial bit is the sentinel: BUG-29 found that go/types
// occasionally yields invalid self-referential types (multi-declaration
// corners where the user's source is malformed but the type-checker
// produces a value anyway), which would otherwise drive Alignof into
// unbounded recursion. Writing a sentinel of 1 BEFORE the recursion
// means the re-entry finds a cached value and returns immediately;
// the spec also guarantees Alignof ≥ 1, so the sentinel doubles as a
// safe approximation for any type the algorithm can't unwind.
//
// Returns a positive int64. Pure with respect to T; mutates s.alignCache
// (so not safe for concurrent calls on a shared *gcSizes — see newGCSizes).
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

// alignofUncached is the actual algorithm Alignof memoises around. Split
// out so the cache-write half stays trivial and so tests can exercise the
// computation directly via a struct-literal *gcSizes with nil caches.
//
// Per the Go spec: arrays align to their element, structs to the max
// alignment among fields (≥ 1), and every other type aligns to its size
// clamped to [1, MaxAlign]. Recurses through Alignof (the memoised entry
// point) so sub-type lookups still hit the cache.
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

// Sizeof reports T's size in bytes as the Go GC observes it: internal
// padding included, but the trailing-zero-sized-field padding rule
// applied only at the enclosing struct's layout, not by the field's own
// type. Memoisation in sizeCache amortises recurring sub-types across a
// pass.
//
// The sentinel-write-before-recurse pattern (mirroring Alignof) terminates
// invalid self-referential types from BUG-29 — a malformed go/types
// product would otherwise drive Sizeof into infinite recursion. Writing
// 0 before descending means the re-entry finds the cached 0, collapsing
// the recursive arm to "self is empty," which is finite even if not
// strictly correct (the source is invalid anyway, so the analyzer just
// emits no diagnostic for it).
//
// Result is saturated to math.MaxInt64 via addSize/mulSize on overflow,
// not negative. Not safe for concurrent calls on a shared *gcSizes.
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

// sizeofUncached is the size algorithm Sizeof memoises around. Split out
// so the recursive arms still hit the cache via Sizeof's wrapper while
// tests can exercise the algorithm directly through a nil-cache *gcSizes.
//
// Basic kinds are table-lookups (basicSizes). String and interface types
// are word pairs (`(ptr, len)` and `(itab, data)` respectively). Slices
// are word triples (`(ptr, len, cap)`). Arrays multiply (with mulSize's
// overflow saturation). Structs walk field-by-field, applying per-field
// alignment via `align(o, a)` and the runtime's trailing-zero-field
// padding rule: a zero-sized last field on a non-empty struct gets one
// padding byte so the field's address never aliases the next allocation
// (a guarantee &empty != &next). Unknown kinds fall through to a single
// word as a conservative placeholder.
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

// align is the offset-rounding primitive used throughout the layout
// algorithm. Caller passes the running offset and the next field's
// alignment; result is the offset where that field actually starts.
// Saturation at math.MaxInt64 (rather than wrap or panic) keeps the
// algorithm well-behaved on adversarial inputs from fuzz harnesses —
// callers can compare the saturated result against MaxInt64 to detect
// overflow if they care. Precondition: a > 0 (Alignof always returns ≥ 1,
// so the value path satisfies this; the function does not guard against
// a == 0 because a fuzz-hardened caller never passes one).
func align(x, a int64) int64 {
	if x > math.MaxInt64-(a-1) {
		return math.MaxInt64
	}
	y := x + a - 1
	return y - y%a
}

// mulSize is the array-size primitive: arr-len times element-size with
// overflow saturation at math.MaxInt64. BUG-30 captured the original bug
// — raw int64 multiply silently wraps negative on adversarial array
// lengths from the fuzz harness, which then poisons every downstream
// invariant ("size ≥ 0"). The non-positive-input collapse to 0 covers
// both the legitimate case (Array of length 0) and the adversarial case
// (negative length from an invalid AST), both of which should report a
// zero-size array, not a wrapped value.
func mulSize(n, size int64) int64 {
	if n <= 0 || size <= 0 {
		return 0
	}
	if n > math.MaxInt64/size {
		return math.MaxInt64
	}
	return n * size
}

// addSize is the offset-accumulator primitive: pairs with align() and
// mulSize() to keep the layout walk overflow-saturated end-to-end.
// Negative inputs collapse to 0 (defensive — neither legitimate field
// sizes nor offsets can be negative, but a saturated upstream result
// can be MaxInt64, and the corresponding sum is also MaxInt64, never
// negative).
func addSize(a, b int64) int64 {
	if a < 0 || b < 0 {
		return 0
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// ptrdata reports how many bytes from the head of T the GC must scan for
// pointers. A zero result means T is pointer-free (the GC can skip it
// entirely); a positive result is the offset just past the last
// pointer-bearing word, which is what the GC tracks for objects with
// trailing pointer-free regions. This number is what the
// alignment-first / ptrdata-minimising sort comparator in optimalOrder
// optimises against — fewer ptrdata bytes means a smaller GC scan
// prefix, which the runtime walks faster.
//
// Memoisation mirrors Sizeof and Alignof for the same reason: recurring
// sub-types are common in real codebases. The pre-recursion sentinel of
// 0 saves the BUG-29 self-referential-struct case from infinite
// recursion by treating the re-entry as "self is pointer-free." Not
// strictly correct, but the source is invalid anyway and the analyzer
// emits no diagnostic for it. Not safe for concurrent calls on a shared
// *gcSizes.
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

// ptrdataUncached is the ptrdata algorithm ptrdata memoises around. Each
// kind is handled per the GC's actual scan rules: strings and
// unsafe.Pointer hold a single pointer word at offset 0; channels, maps,
// pointers, function values, and slices are entirely pointer (one word
// of ptrdata, even slices since the GC only tracks the head pointer);
// interfaces are two pointer words (`(itab, data)`); arrays propagate
// their element's pointer span scaled to the array's last pointer-bearing
// element; structs walk fields tracking the offset of the last
// pointer-bearing field.
//
// The default arm (one-word fallback) catches type kinds the switch
// doesn't enumerate — most importantly *types.TypeParam in generic code,
// whose Underlying() returns itself rather than its constraint, so the
// switch can never see a concrete kind. Returning the word size there
// keeps the analyzer conservative (over-reports ptrdata, never
// under-reports) instead of panicking on an unfamiliar kind.
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

	// Conservative one-word fallback for unenumerated kinds (e.g. uninstantiated TypeParam).
	return s.WordSize
}

// hasSuffix is a multi-suffix early-out check used by run for the
// test-file and generated-file gate. Kept as its own helper (rather than
// inlined twice) so future suffix-set tweaks land in one place. O(N) in
// the number of suffixes; typical N is 1–5.
func hasSuffix(fn string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(fn, s) {
			return true
		}
	}
	return false
}

// hasGeneratedComment is the secondary detector for generated files —
// suffix-based detection via hasSuffix catches the common naming
// conventions, this function catches files where the generator didn't
// follow a naming convention but did emit the canonical header
// (protoc-gen-go, mockgen, sqlc, etc.). The two paths together are what
// the -generated_files flag gates on.
//
// file.Comments is sorted by source position, so the loop scans
// top-down and stops as soon as it reaches the package keyword — the Go
// specification requires the header to appear before `package`, so any
// later comment cannot be the header and need not be scanned. Inside a
// candidate group, individual // lines are checked against
// reGeneratedBy (a DFA generated by the `rec` tool for
// `^//\s*Code generated by .* DO NOT EDIT\.$`); the DFA avoids the
// regexp engine's per-call setup cost which matters when run is sweeping
// thousands of files.
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

// normalizeExcludePaths brings exclude-flag entries into a form isExcluded
// can hand directly to filepath.Match. The mixed-form problem motivating
// the normalisation: users supply `-exclude_dirs vendor` (clean and
// relative) and also `-exclude_dirs /home/me/proj/internal` (absolute)
// in the same invocation; without normalisation the absolute entry can
// never match isExcluded's wd-relative input, and the literal `vendor/`
// would not match `vendor` because filepath.Match doesn't strip trailing
// separators.
//
// The common case — every entry is already relative and canonical —
// returns the input slice unchanged with no allocation. Callers must
// therefore treat the result as read-only; mutating it could mutate the
// caller's input. The non-common path allocates a fresh slice and leaves
// the input untouched.
//
// Returns ErrPreFilterFiles-wrapped errors via the caller for two
// observable failures: filepath.Rel fails when an absolute path can't be
// expressed relative to wd (cross-volume on Windows is the realistic
// trigger); a "..-prefixed" relative result is rejected outright because
// filepath.Match has no way to match a path that escapes the working
// tree, and silently never-matching would be more confusing than failing
// loudly. Errors include both the offending path and the wd so the user
// can diagnose without inspecting the wrapped error.
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

// isExcluded is the per-file exclude check the analyzer applies before
// any AST work. Both flag sets use filepath.Match semantics so users can
// supply glob patterns; the dir-set is checked against every ancestor of
// fn's directory so a literal "vendor" excludes the whole vendor/
// subtree, and `a/*/c` excludes any file under `a/<anything>/c/`.
//
// On filepath.Rel or filepath.Match errors (malformed pattern, path
// escapes wd) the function logs to stderr and returns true. The
// fail-safe direction is "treat as excluded" rather than "treat as
// included": the alternative would have the analyzer happily emit
// diagnostics for paths it could not classify, which would surprise
// users into thinking their exclude pattern is wrong.
//
// O((ancestors × dirs) + files) per call. For betteralign's typical
// inputs (a handful of dir patterns, a handful of file patterns, a few
// dozen ancestors) this is negligible compared to the AST walk it
// gates. Pure with respect to fs state — the only I/O is the stderr log
// on error.
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
				match, err := filepath.Match(filepath.Clean(excludeDir), ancestor)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%v %s: %v\n", ErrPreFilterFiles, fn, err)
					return true
				}
				if match {
					return true
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

// hasIgnoreComment is the per-struct opt-out check. Lets users carve a
// single struct out of analysis without reaching for -exclude_files or
// -opt_in, which gate at file scope and would force splitting code into
// multiple files. The directive's scope is the struct body: it must
// appear among the comments attached to the opening brace, where DST
// gathers consecutive comment lines into a single decoration group.
//
// Only `//`-style line comments count (commentHasDirective rejects
// `/* betteralign:ignore */`), and the directive must be followed by a
// whitespace/end-of-string boundary so substring matches like
// `betteralign:ignored` don't trigger. Pure predicate.
func hasIgnoreComment(node *dst.FieldList) bool {
	for _, opening := range node.Decs.Opening.All() {
		if commentHasDirective(opening, ignoreStruct) {
			return true
		}
	}

	return false
}

// hasIgnoreCommentAST is the DST-independent twin of hasIgnoreComment, used so
// the betteralign:ignore directive is honored even for structs dstmin cannot
// decorate (single-line forms, etc.) where no DST node exists. It mirrors the
// decoration's Opening routing: a group on the opening-brace line, inside the
// body, is the directive's canonical home (`type S struct { // betteralign:ignore`).
// Both paths share commentHasDirective, so the recognition rules can't drift;
// only the comment-selection differs. file.Comments is position-sorted, so the
// scan stops once it reaches the closing brace. Pure predicate.
func hasIgnoreCommentAST(fset *token.FileSet, file *ast.File, s *ast.StructType) bool {
	open := s.Fields.Opening
	openLine := fset.Position(open).Line
	for _, cg := range file.Comments {
		if cg.Pos() >= s.Fields.Closing {
			break
		}
		if cg.Pos() <= open || fset.Position(cg.Pos()).Line != openLine {
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

// commentGroupHasOptIn is the per-CommentGroup half of the -opt_in
// detection used by run when -opt_in mode is active. nil cg is treated
// as "no comments → no opt-in" so callers can hand in an
// *ast.CommentGroup that may be nil (the common case for type specs
// without a doc comment) without nil-checking themselves. See
// commentHasDirective for the directive recognition rules.
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

// commentHasDirective is the central directive-recogniser for
// betteralign:ignore and betteralign:check. Centralising the parsing
// keeps the two directives consistent and the rules in one place:
//
//   - Must be a `//`-style line comment. Block comments are rejected
//     because gofmt's handling of trailing block comments inside structs
//     is fragile (see BUG-42) and we never want a `/* betteralign:ignore */`
//     to silently flip behaviour based on placement.
//   - Leading whitespace after `//` is tolerated (`// betteralign:ignore`
//     is the canonical form, but `//betteralign:ignore` also works).
//   - Trailing word boundary required: `betteralign:ignored` does NOT
//     trigger `betteralign:ignore`. The boundary is space, tab, or end
//     of comment.
//
// Pure function, no allocations beyond the strings package's interior
// slicing.
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

// declaredWithOptInComment looks for the opt-in directive on the GenDecl
// itself rather than on any contained TypeSpec. This handles the
// parenthesised `type ( ... )` form, where users typically attach a
// single doc comment to the whole group and expect it to apply to every
// type inside. Without this path, an opt-in comment on `type (\n  S
// struct{...}\n)` would never match because go/ast attaches the doc to
// the GenDecl, not to the inner TypeSpec.
func declaredWithOptInComment(decl *ast.GenDecl) bool {
	return commentGroupHasOptIn(decl.Doc)
}

// applyToFile is the source-file writer for -apply mode. Three layers of
// guarding stand between buf and the user's disk:
//
//  1. ErrEmptyBuffer for buf == nil/empty. The realistic trigger is a
//     future DST regression where Fprint returns successfully but emits
//     nothing; refusing to write a zero-length file means the worst
//     outcome of such a regression is "no rewrite happens", not "user's
//     source file is now empty."
//  2. ErrNotRegularFile via os.Lstat. Lstat (not Stat) is mandatory:
//     os.Stat follows symlinks, so a symlink whose target is regular
//     would pass an IsRegular check and the subsequent rename would
//     replace the symlink itself with the rewritten file — which
//     clobbers the user's symlink intent silently. A TOCTOU window
//     remains between Lstat and renameio's temp-rename, but the common
//     "symlink already lives at this path" case is now refused outright.
//  3. The actual write goes through renameio's maybe.WriteFile, which
//     does temp-write + atomic rename on POSIX (and best-effort on
//     Windows) so a partial write or crash never leaves the user with
//     half a Go file.
//
// Errors from Lstat and WriteFile are wrapped with ErrStatFile and
// ErrWriteFile so callers can classify failures via errors.Is without
// parsing strings. Permission bits are preserved by passing st.Mode() to
// WriteFile.
//
// Not safe for concurrent calls on the same fn; the analyzer serialises
// per-file writes in run's apply phase.
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
