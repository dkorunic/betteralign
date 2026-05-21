// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//
// Forked and modified by Dinko Korunic, 2022-2025

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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/google/renameio/v2/maybe"
	"github.com/sirkon/dst"
	"github.com/sirkon/dst/decorator"
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

// StringArrayFlag is a flag.Value that accumulates comma-separated string
// arguments across one or more occurrences of the same flag. It backs the
// -exclude_dirs and -exclude_files options.
type StringArrayFlag []string

// String returns the accumulated values rendered with the default Go
// formatter, satisfying the flag.Value interface.
func (f *StringArrayFlag) String() string {
	return fmt.Sprintf("%v", *f)
}

// Set splits value on commas, trims surrounding whitespace from each entry,
// and appends every non-empty result to the flag. Empty entries (e.g. from a
// stray trailing comma) are dropped silently.
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

// InitAnalyzer wires a fresh analyzerConfig into analyzer: it allocates a new
// config, binds the command-line flags to that config, and sets analyzer.Run
// to the config's run method. Each analyzer constructed through InitAnalyzer
// gets independent flag state, so concurrent test analyzers no longer share
// mutable globals. The function is also exposed so external drivers can wire
// up the same flag set without going through singlechecker.Main.
//
// Calling InitAnalyzer on an analyzer that is already initialised (i.e. its
// flag set already carries the "apply" entry) is a no-op rather than a
// panic. flag.FlagSet.BoolVar fatals on duplicate names; the guard lets
// defensive callers re-init without taking down the host process.
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

func init() {
	InitAnalyzer(Analyzer)
}

// run is the analyzer's entry point invoked by the go/analysis framework. It
// hooks into the *inspector.Inspector built by inspect.Analyzer, visiting
// *ast.File, *ast.GenDecl and *ast.StructType nodes in source order. For
// every named struct type whose fields are not laid out optimally it emits a
// diagnostic; when -apply (or its -fix alias) is set, the decorated DST of
// each affected file is reprinted and atomically written back to disk.
// SuggestedFixes are deliberately left empty because DST cannot serialise a
// single node without losing its comment attachments (see the file-level
// package documentation for the full rationale).
func (cfg *analyzerConfig) run(pass *analysis.Pass) (any, error) {
	apply := cfg.apply
	if a := pass.Analyzer.Flags.Lookup("fix"); a != nil && a.Value.String() == "true" {
		apply = true
	}
	testFiles := cfg.testFiles
	generatedFiles := cfg.generatedFiles
	optInMode := cfg.optInMode
	excludeDirs := cfg.excludeDirs
	excludeFiles := cfg.excludeFiles

	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	positionalUsers := collectPositionalUsers(inspect, pass)

	// Lazy: nil until the first misaligned struct.
	var dec *decorator.Decorator

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
		var message string
		if sz := sizes.Sizeof(typ); sz != optsz {
			message = fmt.Sprintf("%d bytes saved: struct of size %d could be %d", sz-optsz, sz, optsz)
		} else if ptrs := sizes.ptrdata(typ); ptrs != optptrs {
			message = fmt.Sprintf("%d bytes saved: struct with %d pointer bytes could be %d", ptrs-optptrs, ptrs, optptrs)
		} else {
			return
		}

		// Decorate this file once; never retry on failure.
		if _, failed := decorationFailed[currentFn]; failed {
			return
		}
		if dec == nil {
			dec = decorator.NewDecorator(pass.Fset)
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
			return
		}

		// Skip if // betteralign:ignore appears inside the struct body.
		if hasIgnoreComment(dNode.Fields) {
			return
		}

		// Positional literal pins layout: reporting is safe, mutation is not.
		if litPos, blocked := positionalUsers[typ]; blocked {
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

		// Skip DST mutation when only emitting diagnostics.
		if !apply {
			return
		}

		// Flatten multi-name fields (A, B int) into one slot per name.
		// TODO: preserve multi-named fields instead of flattening.
		flat := fieldSlicePool.Get().([]*dst.Field)[:0]
		defer func() {
			// Drop pointer refs so the pool can't pin DST nodes.
			clear(flat)
			fieldSlicePool.Put(flat[:0])
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
			if err := decorator.Fprint(&buf, df); err != nil {
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

// unsafePointerTyp is the canonical types.Type for unsafe.Pointer, used to
// derive the architecture's word size and pointer alignment from
// pass.TypesSizes. The lookup is wrapped to avoid a panic at package init in
// the unlikely event that the standard library's unsafe package shape ever
// changes — gcSizes still works with reasonable defaults of 8 bytes if so.
var unsafePointerTyp = lookupUnsafePointer()

// nodeFilter is the constant set of AST nodes the visitor cares about; declared
// once at package level so each pass shares the same slice instead of
// allocating a fresh one.
var nodeFilter = []ast.Node{
	(*ast.File)(nil),
	(*ast.StructType)(nil),
	(*ast.GenDecl)(nil),
}

// dummyField is a placeholder used by the apply-mode flatten/reorder logic to
// keep flat positions aligned with optimalOrder indexes when a single dst.Field
// declares multiple names (e.g. `A, B int`). Pointer equality with this
// sentinel identifies skip-positions during the reorder pass. Reusing a single
// package-level value avoids per-struct allocation.
var dummyField = &dst.Field{}

// fieldElem is optimalOrder's per-field sort record; lifted out so a pool can recycle it.
type fieldElem struct {
	alignof int64
	sizeof  int64
	ptrdata int64
	index   int
}

// elemsPool recycles fieldElem scratch buffers for optimalOrder.
var elemsPool = sync.Pool{
	New: func() any { return []fieldElem(nil) },
}

// fieldSlicePool recycles the apply-mode flatten buffer; reordered is freshly allocated.
var fieldSlicePool = sync.Pool{
	New: func() any { return []*dst.Field(nil) },
}

// collectPositionalUsers returns a set of *types.Struct values whose types
// are constructed somewhere in the package via a positional composite literal
// (e.g. `T{1, 2, 3}` rather than `T{a: 1, b: 2, c: 3}`). The map's value is
// the position of the first such literal — surfaced in the diagnostic so the
// user can jump straight to the source that prevents the reorder.
//
// The walk deliberately ignores betteralign's per-file skip flags
// (-exclude_files, -exclude_dirs, -test_files, -generated_files): excluded
// source still depends on the struct layout at compile time, so reordering an
// analysed declaration would break the excluded source just as readily.
//
// Go's grammar forbids mixing keyed and positional elements in the same
// literal, so inspecting the first element is sufficient to classify the
// whole literal (this is the same shortcut go vet's "composites" pass uses).
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

// canonicalStructType returns the *types.Struct that the main alignment
// visitor will see when it lands on the corresponding *ast.StructType, or nil
// when t does not denote a struct type. The function resolves four forms to
// a single canonical pointer the visitor can equate against:
//
//   - plain named structs (`T{...}`)               → T's struct body
//   - chained type definitions (`type S T`)        → T's struct body (shared)
//   - generic instantiations (`Box[int]{...}`)     → the generic origin's body
//   - pointer-typed elided literals (`[]*T{{...}}`) → T's struct body
//
// Aliases (`type A = T`) are unwrapped via types.Unalias first. Anonymous
// struct literals fall through the *types.Struct case and key themselves;
// they never match a candidate (the analyzer only reports named structs) but
// keeping them in the map is harmless and avoids special-casing.
//
// The pointer arm matters for elided composite literals inside slice, array
// or map types whose element type is a pointer: in `[]*T{{1,2,3}}` the
// type-checker records the inner literal with type `*T`, not `T`, so without
// unwrapping the layout-pinning detection would miss the literal and the
// analyzer would happily reorder T's fields, breaking the build.
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

// lookupUnsafePointer returns the canonical types.Type for unsafe.Pointer, or
// nil if the standard library's unsafe package no longer exposes Pointer as a
// type name. The result is cached at package init in unsafePointerTyp; run
// falls back to architecture-default sizes (8 bytes) when this returns nil.
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

// optimalOrder returns the optimal field permutation as a list of original
// indexes, along with the size and ptrdata of the struct laid out in that
// order. Computing size and ptrdata directly from the sorted elements avoids
// rebuilding a *types.Struct just to call Sizeof / ptrdata on it.
//
// The fieldElem scratch slice is recycled through elemsPool so that
// successive optimalOrder calls amortise the backing-array allocation across
// the pass; the returned indexes slice is freshly allocated because it
// outlives this function.
func optimalOrder(str *types.Struct, sizes *gcSizes) (indexes []int, optSize, optPtrdata int64) {
	nf := str.NumFields()

	elems := elemsPool.Get().([]fieldElem)
	if cap(elems) < nf {
		elems = make([]fieldElem, nf)
	} else {
		elems = elems[:nf]
	}
	defer func() {
		elemsPool.Put(elems[:0])
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
			optPtrdata = offset + e.ptrdata
		}
		sz := e.sizeof
		// Trailing zero-size field on a non-empty struct gets one padding byte.
		if i == nf-1 && sz == 0 && offset != 0 {
			sz = 1
		}
		offset += sz
	}
	optSize = align(offset, maxAlign)
	return indexes, optSize, optPtrdata
}

// Code below based on go/types.StdSizes.

// gcSizes computes sizes, alignments and ptrdata in the same way as the Go
// runtime's garbage collector. The three cache maps memoise per-type results
// within a single analysis pass — they avoid recomputing the layout of types
// that recur across many fields (a major cost in protobuf-style generated
// code where the same sub-message type appears repeatedly). The caches are
// lazy-initialised so callers can still construct a *gcSizes with a struct
// literal in tests.
type gcSizes struct {
	sizeCache  map[types.Type]int64
	alignCache map[types.Type]int64
	ptrCache   map[types.Type]int64
	WordSize   int64
	MaxAlign   int64
}

// newGCSizes returns a *gcSizes seeded with the target architecture's word
// size and maximum alignment, with empty memoisation caches ready for use.
// Tests may also construct a *gcSizes via a struct literal; in that case the
// caches are nil and the Alignof/Sizeof/ptrdata helpers degrade gracefully to
// uncached computation.
func newGCSizes(wordSize, maxAlign int64) *gcSizes {
	return &gcSizes{
		WordSize:   wordSize,
		MaxAlign:   maxAlign,
		sizeCache:  make(map[types.Type]int64),
		alignCache: make(map[types.Type]int64),
		ptrCache:   make(map[types.Type]int64),
	}
}

// Alignof returns T's alignment in bytes as the Go GC observes it. Results
// are memoised in alignCache so recurring sub-types (common in generated
// protobuf code) are paid for once per pass.
func (s *gcSizes) Alignof(T types.Type) int64 {
	if v, ok := s.alignCache[T]; ok {
		return v
	}
	v := s.alignofUncached(T)
	if s.alignCache != nil {
		s.alignCache[T] = v
	}
	return v
}

// alignofUncached computes T's alignment from its underlying type without
// consulting the cache. Arrays defer to their element alignment, structs to
// the max of their fields, and any other type to its size clamped to
// [1, MaxAlign].
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

// Sizeof returns T's size in bytes as the Go GC observes it (including
// internal padding but not the trailing-zero-field padding rule, which is
// applied by the enclosing struct's layout). Results are memoised in
// sizeCache.
func (s *gcSizes) Sizeof(T types.Type) int64 {
	if v, ok := s.sizeCache[T]; ok {
		return v
	}
	v := s.sizeofUncached(T)
	if s.sizeCache != nil {
		s.sizeCache[T] = v
	}
	return v
}

// sizeofUncached computes T's size from its underlying type without
// consulting the cache. Basic kinds come from basicSizes, strings and
// interfaces from word-size multiples, slices from 3*WordSize, and structs
// from a field-by-field walk that respects per-field alignment plus a single
// padding byte after a trailing zero-sized field on a non-empty struct (a
// runtime quirk that prevents zero-size values aliasing the next allocation).
func (s *gcSizes) sizeofUncached(T types.Type) int64 {
	switch t := T.Underlying().(type) {
	case *types.Basic:
		k := t.Kind()
		if int(k) < len(basicSizes) {
			if s := basicSizes[k]; s > 0 {
				return int64(s)
			}
		}
		if k == types.String {
			return s.WordSize * 2
		}
	case *types.Array:
		return t.Len() * s.Sizeof(t.Elem())
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
			o = align(o, a) + sz
		}
		return align(o, max)
	case *types.Interface:
		return s.WordSize * 2
	}
	return s.WordSize // catch-all
}

// align returns the smallest y >= x such that y % a == 0.
func align(x, a int64) int64 {
	y := x + a - 1
	return y - y%a
}

// ptrdata returns the number of bytes at the head of T that the GC must scan
// for pointers. A return of 0 means the type is pointer-free; otherwise it is
// the offset just past the last pointer-bearing word. Results are memoised
// in ptrCache.
func (s *gcSizes) ptrdata(T types.Type) int64 {
	if v, ok := s.ptrCache[T]; ok {
		return v
	}
	v := s.ptrdataUncached(T)
	if s.ptrCache != nil {
		s.ptrCache[T] = v
	}
	return v
}

// ptrdataUncached computes T's ptrdata from its underlying type without
// consulting the cache. Arrays propagate their element's pointer span,
// structs track the offset of the last pointer-bearing field, and basic
// types return either a word (string, unsafe.Pointer) or zero. Panics on
// types the analyser does not expect to encounter at this layer.
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
		if n == 0 {
			return 0
		}
		a := s.ptrdata(t.Elem())
		if a == 0 {
			return 0
		}
		z := s.Sizeof(t.Elem())
		return (n-1)*z + a
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
				p = o + fp
			}
			o += sz
		}
		return p
	}

	panic("impossible")
}

// hasSuffix reports whether fn ends with any of the given suffixes.
func hasSuffix(fn string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(fn, s) {
			return true
		}
	}
	return false
}

// hasGeneratedComment reports whether file carries the canonical "Code
// generated ... DO NOT EDIT." marker before its package keyword.
//
// file.Comments is sorted by source position, so the loop walks comment
// groups from the top of the file downward and bails out as soon as it
// crosses file.Package — comments that appear after the package keyword
// cannot be the generated-file header per the Go specification and need
// not be scanned. Within a candidate comment group the individual
// // lines are matched against reGeneratedBy (the rec-generated DFA for
// ^//\s*Code generated by .* DO NOT EDIT\.$).
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

// normalizeExcludePaths rewrites every absolute entry in paths as a
// wd-relative path. Patterns that are already relative are passed through
// unchanged. isExcluded compares against a wd-relative file name, so mixing
// absolute and relative paths in filepath.Rel/filepath.Match would silently
// fail; this normalisation step keeps both forms compatible.
//
// When no entry is absolute (the common case), the input slice is returned
// unchanged — no allocation. Callers must therefore not mutate the result;
// they only read from it. When at least one entry is absolute, a fresh slice
// is returned and the input is left untouched.
//
// Errors from filepath.Rel are wrapped with the offending path and the
// working directory so users can diagnose cross-volume mismatches (Windows)
// or working-directory mismatches without inspecting the underlying error.
func normalizeExcludePaths(wd string, paths []string) ([]string, error) {
	if !slices.ContainsFunc(paths, filepath.IsAbs) {
		return paths, nil
	}
	out := make([]string, len(paths))
	for i, p := range paths {
		if !filepath.IsAbs(p) {
			out[i] = p
			continue
		}
		rel, err := filepath.Rel(wd, p)
		if err != nil {
			return nil, fmt.Errorf("exclude path %q is not resolvable relative to working directory %q (cross-volume on Windows, or requires runtime cwd): %w", p, wd, err)
		}
		out[i] = rel
	}
	return out, nil
}

// isExcluded reports whether fn is excluded by the given excludeDirs or
// excludeFiles patterns relative to wd. Errors interpreting paths are logged
// to stderr and the file is treated as excluded so the analyzer does not
// silently emit diagnostics it cannot ground.
func isExcluded(wd, fn string, excludeDirs, excludeFiles []string) bool {
	relfn, err := filepath.Rel(wd, fn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v %s: %v\n", ErrPreFilterFiles, fn, err)
		return true
	}
	dir := filepath.Dir(relfn)
	for _, excludeDir := range excludeDirs {
		rel, err := filepath.Rel(excludeDir, dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v %s: %v\n", ErrPreFilterFiles, fn, err)
			return true
		}
		if !strings.HasPrefix(rel, "..") {
			return true
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

// hasIgnoreComment reports whether the struct body's opening-brace
// decorations include a // betteralign:ignore line directive. It scans every
// comment attached to the opening brace because DST groups consecutive
// comments together; the directive only counts if it appears as a recognised
// "//"-style line comment (see commentHasDirective).
func hasIgnoreComment(node *dst.FieldList) bool {
	for _, opening := range node.Decs.Opening.All() {
		if commentHasDirective(opening, ignoreStruct) {
			return true
		}
	}

	return false
}

// commentGroupHasOptIn reports whether cg carries a // betteralign:check line
// directive. A nil cg is treated as having no comments and returns false, so
// callers can pass an *ast.CommentGroup that may be missing.
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

// commentHasDirective reports whether comment is a "//"-style line comment
// whose body contains directive as a whitespace-delimited token. Block
// comments and bare strings are rejected; substring matches (e.g.
// "betteralign:ignored" matching "betteralign:ignore") are rejected by the
// trailing word-boundary check.
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

// declaredWithOptInComment reports whether decl's own doc comment carries a
// // betteralign:check directive. This handles the `type ( ... )` block form,
// where a directive attached to the GenDecl applies to every TypeSpec inside
// the parenthesised group rather than to any individual spec.
func declaredWithOptInComment(decl *ast.GenDecl) bool {
	return commentGroupHasOptIn(decl.Doc)
}

// applyToFile atomically writes buf to fn while preserving the original
// file's permission bits. An empty buf is refused with ErrEmptyBuffer so a
// degenerate Fprint result (e.g. from a future DST regression) cannot blank
// the user's source file. Non-regular targets (symlinks, devices, named
// pipes) are rejected with ErrNotRegularFile so the analyzer cannot silently
// follow a symlink and clobber an unintended file. Stat and write failures
// are wrapped with ErrStatFile and ErrWriteFile respectively so callers can
// classify them via errors.Is.
//
// os.Lstat is used (rather than os.Stat) so that the regular-file check
// observes the symlink directly. With os.Stat a symlink whose target is a
// regular file would pass the IsRegular guard and the subsequent rename
// would replace the symlink itself, clobbering the user's intent. A TOCTOU
// window remains between Lstat and the renameio temp-rename, but the common
// "symlink already at the path" case is now refused outright.
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
