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

`

const (
	ignoreStruct = "betteralign:ignore"
	optInStruct  = "betteralign:check"
)

var (
	// flags
	fApply          bool
	fTestFiles      bool
	fGeneratedFiles bool
	fOptInMode      bool
	fExcludeFiles   StringArrayFlag
	fExcludeDirs    StringArrayFlag

	// default test and generated suffixes
	testSuffixes      = []string{"_test.go"}
	generatedSuffixes = []string{"_generated.go", "_gen.go", ".gen.go", ".pb.go", ".pb.gw.go"}

	// errors
	ErrStatFile       = errors.New("unable to stat the file")
	ErrNotRegularFile = errors.New("not a regular file, skipping")
	ErrWriteFile      = errors.New("unable to write to file")
	ErrPreFilterFiles = errors.New("failed to pre-filter files")
)

type StringArrayFlag []string

func (f *StringArrayFlag) String() string {
	return fmt.Sprintf("%v", *f)
}

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
	Run:      run,
}

func InitAnalyzer(analyzer *analysis.Analyzer) {
	analyzer.Flags.BoolVar(&fApply, "apply", false, "apply suggested fixes")
	analyzer.Flags.BoolVar(&fTestFiles, "test_files", false, "also check and fix test files")
	analyzer.Flags.BoolVar(&fGeneratedFiles, "generated_files", false, "also check and fix generated files")
	analyzer.Flags.BoolVar(&fOptInMode, "opt_in", false, fmt.Sprintf("opt-in mode on per-struct basis with '%s' in comment", optInStruct))
	analyzer.Flags.Var(&fExcludeFiles, "exclude_files", "exclude files matching a pattern")
	analyzer.Flags.Var(&fExcludeDirs, "exclude_dirs", "exclude directories matching a pattern")
}

func init() {
	InitAnalyzer(Analyzer)
}

// ResetFlags resets all package-level flag variables to their zero values.
// This function is intended for use in tests only. It must be called (via
// NewTestAnalyzer in betteralign_test.go) before each analyzer construction
// to prevent StringArrayFlag.Set from accumulating values across test
// functions. It has no effect on the behaviour of the analyzer in production.
func ResetFlags() {
	fApply = false
	fTestFiles = false
	fGeneratedFiles = false
	fOptInMode = false
	fExcludeFiles = nil
	fExcludeDirs = nil
}

func run(pass *analysis.Pass) (any, error) {
	// Snapshot package-level flags so concurrent passes see a consistent view.
	apply := fApply
	if a := pass.Analyzer.Flags.Lookup("fix"); a != nil && a.Value.String() == "true" {
		apply = true
	}
	testFiles := fTestFiles
	generatedFiles := fGeneratedFiles
	optInMode := fOptInMode
	excludeDirs := fExcludeDirs
	excludeFiles := fExcludeFiles

	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	dec := decorator.NewDecorator(pass.Fset)

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

		pass.Report(analysis.Diagnostic{
			Pos:            s.Pos(),
			End:            s.Pos() + token.Pos(len("struct")),
			Message:        message,
			SuggestedFixes: nil,
		})

		// Skip DST mutation when only emitting diagnostics.
		if !apply {
			return
		}

		// Flatten multi-name fields (A, B int) into one slot per name.
		// TODO: preserve multi-named fields instead of flattening.
		flat := make([]*dst.Field, 0, len(indexes))
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
			// Refuse to blank the user's source on a degenerate Fprint result.
			if buf.Len() == 0 {
				fmt.Fprintf(os.Stderr, "refusing to write empty buffer to %s\n", fn)
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
func optimalOrder(str *types.Struct, sizes *gcSizes) (indexes []int, optSize, optPtrdata int64) {
	nf := str.NumFields()

	type elem struct {
		alignof int64
		sizeof  int64
		ptrdata int64
		index   int
	}

	elems := make([]elem, nf)
	for i := range nf {
		field := str.Field(i)
		ft := field.Type()
		elems[i] = elem{
			alignof: sizes.Alignof(ft),
			sizeof:  sizes.Sizeof(ft),
			ptrdata: sizes.ptrdata(ft),
			index:   i,
		}
	}

	slices.SortStableFunc(elems, func(a, b elem) int {
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

func newGCSizes(wordSize, maxAlign int64) *gcSizes {
	return &gcSizes{
		WordSize:   wordSize,
		MaxAlign:   maxAlign,
		sizeCache:  make(map[types.Type]int64),
		alignCache: make(map[types.Type]int64),
		ptrCache:   make(map[types.Type]int64),
	}
}

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

// hasGeneratedComment reports whether file carries a "Code generated ... DO NOT
// EDIT." comment before its package keyword.
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

func hasIgnoreComment(node *dst.FieldList) bool {
	for _, opening := range node.Decs.Opening.All() {
		if commentHasDirective(opening, ignoreStruct) {
			return true
		}
	}

	return false
}

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

func declaredWithOptInComment(decl *ast.GenDecl) bool {
	return commentGroupHasOptIn(decl.Doc)
}

func applyToFile(fn string, buf []byte) error {
	st, err := os.Stat(fn)
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
