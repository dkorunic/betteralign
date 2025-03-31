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
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
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
	*f = append(*f, strings.Split(value, ",")...)
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

func run(pass *analysis.Pass) (any, error) {
	if a := pass.Analyzer.Flags.Lookup("fix"); a != nil && a.Value.String() == "true" {
		fApply = true
	}

	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	dec := decorator.NewDecorator(pass.Fset)
	nodeFilter := []ast.Node{
		(*ast.File)(nil),
		(*ast.StructType)(nil),
		(*ast.GenDecl)(nil),
	}

	var aFile *ast.File
	var dFile *dst.File
	var strName string
	var strOptedIn bool

	applyFixesFset := make(map[string][]byte)
	testFset := make(map[string]bool)
	generatedFset := make(map[string]bool)

	inspect.Preorder(nodeFilter, func(node ast.Node) {
		fn := pass.Fset.File(node.Pos()).Name()

		if !fTestFiles && hasSuffixes(testFset, fn, testSuffixes) {
			return
		}

		if !fGeneratedFiles && hasSuffixes(generatedFset, fn, generatedSuffixes) {
			return
		}

		if len(fExcludeDirs) > 0 || len(fExcludeFiles) > 0 {
			wd, err := os.Getwd()
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v %s: %v", ErrPreFilterFiles, fn, err)
				return
			}
			relfn, err := filepath.Rel(wd, fn)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v %s: %v", ErrPreFilterFiles, fn, err)
				return
			}
			dir := filepath.Dir(relfn)
			for _, excludeDir := range fExcludeDirs {
				rel, err := filepath.Rel(excludeDir, dir)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%v %s: %v", ErrPreFilterFiles, fn, err)
					return
				}
				if !strings.HasPrefix(rel, "..") {
					return
				}
			}
			for _, excludeFile := range fExcludeFiles {
				match, err := filepath.Match(excludeFile, relfn)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%v %s: %v", ErrPreFilterFiles, fn, err)
					return
				}
				if match {
					return
				}
			}
		}

		if f, ok := node.(*ast.File); ok {
			aFile = f
			dFile, _ = dec.DecorateFile(aFile)

			if !fGeneratedFiles && hasGeneratedComment(generatedFset, fn, aFile) {
				return
			}

			return
		}

		var ok bool
		var s *ast.StructType
		var g *ast.GenDecl

		if g, ok = node.(*ast.GenDecl); ok {
			if g.Tok == token.TYPE {
				decl := g.Specs[0].(*ast.TypeSpec)
				strName = decl.Name.Name
				strOptedIn = declaredWithOptInComment(g)
			}

			return
		}

		if s, ok = node.(*ast.StructType); !ok {
			return
		}

		// ignore structs with anonymous fields
		if strName == "" {
			return
		}

		// if in opt-in mode, ignore structs that lack the opt-in comment magic substring
		if fOptInMode && !strOptedIn {
			return
		}

		if tv, ok := pass.TypesInfo.Types[s]; ok {
			betteralign(pass, s, tv.Type.(*types.Struct), dec, dFile, applyFixesFset, fn)
		}
	})

	if !fApply {
		return nil, nil
	}

	for fn, buf := range applyFixesFset {
		if err := applyToFile(fn, buf); err != nil {
			fmt.Fprintf(os.Stderr, "error applying fixes to %v: %v\n", fn, err)
		}
	}

	return nil, nil
}

var unsafePointerTyp = types.Unsafe.Scope().Lookup("Pointer").(*types.TypeName).Type()

func betteralign(pass *analysis.Pass, aNode *ast.StructType, typ *types.Struct, dec *decorator.Decorator,
	dFile *dst.File, fixOps map[string][]byte, fn string,
) {
	wordSize := pass.TypesSizes.Sizeof(unsafePointerTyp)
	maxAlign := pass.TypesSizes.Alignof(unsafePointerTyp)

	s := gcSizes{wordSize, maxAlign}
	optimal, indexes := optimalOrder(typ, &s)
	optsz, optptrs := s.Sizeof(optimal), s.ptrdata(optimal)

	var message string
	if sz := s.Sizeof(typ); sz != optsz {
		message = fmt.Sprintf("%d bytes saved: struct of size %d could be %d", sz-optsz, sz, optsz)
	} else if ptrs := s.ptrdata(typ); ptrs != optptrs {
		message = fmt.Sprintf("%d bytes saved: struct with %d pointer bytes could be %d", ptrs-optptrs, ptrs, optptrs)
	} else {
		// Already optimal order.
		return
	}

	dNode := dec.Dst.Nodes[aNode].(*dst.StructType)

	// Skip if explicitly ignored with magic comment substring.
	if hasIgnoreComment(dNode.Fields) {
		return
	}

	// Flatten the ast node since it could have multiple field names per list item while
	// *types.Struct only have one item per field.
	// TODO: Preserve multi-named fields instead of flattening.
	flat := make([]*dst.Field, 0, len(indexes))
	dummy := &dst.Field{}
	for _, f := range dNode.Fields.List {
		flat = append(flat, f)
		if len(f.Names) == 0 {
			continue
		}

		for range f.Names[1:] {
			flat = append(flat, dummy)
		}
	}

	// Sort fields according to the optimal order.
	reordered := make([]*dst.Field, 0, len(indexes))
	for _, index := range indexes {
		f := flat[index]
		if f == dummy {
			continue
		}
		reordered = append(reordered, f)
	}

	dNode.Fields.List = reordered

	var buf bytes.Buffer
	if err := decorator.Fprint(&buf, dFile); err != nil {
		return
	}

	pass.Report(analysis.Diagnostic{
		Pos:            aNode.Pos(),
		End:            aNode.Pos() + token.Pos(len("struct")),
		Message:        message,
		SuggestedFixes: nil,
	})

	fixOps[fn] = buf.Bytes()
}

func optimalOrder(str *types.Struct, sizes *gcSizes) (*types.Struct, []int) {
	nf := str.NumFields()

	type elem struct {
		index   int
		alignof int64
		sizeof  int64
		ptrdata int64
	}

	elems := make([]elem, nf)
	for i := range nf {
		field := str.Field(i)
		ft := field.Type()
		elems[i] = elem{
			i,
			sizes.Alignof(ft),
			sizes.Sizeof(ft),
			sizes.ptrdata(ft),
		}
	}

	sort.SliceStable(elems, func(i, j int) bool {
		ei := &elems[i]
		ej := &elems[j]

		// Place zero sized objects before non-zero sized objects.
		zeroi := ei.sizeof == 0
		zeroj := ej.sizeof == 0
		if zeroi != zeroj {
			return zeroi
		}

		// Next, place more tightly aligned objects before less tightly aligned objects.
		if ei.alignof != ej.alignof {
			return ei.alignof > ej.alignof
		}

		// Place pointerful objects before pointer-free objects.
		noptrsi := ei.ptrdata == 0
		noptrsj := ej.ptrdata == 0
		if noptrsi != noptrsj {
			return noptrsj
		}

		if !noptrsi {
			// If both have pointers...

			// ... then place objects with less trailing
			// non-pointer bytes earlier. That is, place
			// the field with the most trailing
			// non-pointer bytes at the end of the
			// pointerful section.
			traili := ei.sizeof - ei.ptrdata
			trailj := ej.sizeof - ej.ptrdata
			if traili != trailj {
				return traili < trailj
			}
		}

		// Lastly, order by size.
		if ei.sizeof != ej.sizeof {
			return ei.sizeof > ej.sizeof
		}

		return false
	})

	fields := make([]*types.Var, nf)
	indexes := make([]int, nf)
	for i, e := range elems {
		fields[i] = str.Field(e.index)
		indexes[i] = e.index
	}
	return types.NewStruct(fields, nil), indexes
}

// Code below based on go/types.StdSizes.

type gcSizes struct {
	WordSize int64
	MaxAlign int64
}

func (s *gcSizes) Alignof(T types.Type) int64 {
	// For arrays and structs, alignment is defined in terms
	// of alignment of the elements and fields, respectively.
	switch t := T.Underlying().(type) {
	case *types.Array:
		// spec: "For a variable x of array type: unsafe.Alignof(x)
		// is the same as unsafe.Alignof(x[0]), but at least 1."
		return s.Alignof(t.Elem())
	case *types.Struct:
		// spec: "For a variable x of struct type: unsafe.Alignof(x)
		// is the largest of the values unsafe.Alignof(x.f) for each
		// field f of x, but at least 1."
		max := int64(1)
		for i, nf := 0, t.NumFields(); i < nf; i++ {
			if a := s.Alignof(t.Field(i).Type()); a > max {
				max = a
			}
		}
		return max
	}
	a := s.Sizeof(T) // may be 0
	// spec: "For a variable x of any type: unsafe.Alignof(x) is at least 1."
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

func hasSuffixes(fset map[string]bool, fn string, suffixes []string) bool {
	if t, ok := fset[fn]; ok {
		return t
	} else {
		for _, s := range suffixes {
			if strings.HasSuffix(fn, s) {
				fset[fn] = true

				return true
			}
		}
	}

	fset[fn] = false

	return false
}

func hasGeneratedComment(generatedFset map[string]bool, fn string, file *ast.File) bool {
	for _, cg := range file.Comments {
		if cg.Pos() > file.Package {
			return false
		}

		for _, l := range cg.List {
			if reGeneratedBy(l.Text) {
				generatedFset[fn] = true
				return true
			}
		}
	}

	return false
}

func hasIgnoreComment(node *dst.FieldList) bool {
	for _, opening := range node.Decs.Opening.All() {
		if strings.HasPrefix(opening, "//") && strings.Contains(opening, ignoreStruct) {
			return true
		}
	}

	return false
}

func declaredWithOptInComment(decl *ast.GenDecl) bool {
	if decl.Doc == nil {
		return false
	}
	for _, dc := range decl.Doc.List {
		if strings.Contains(dc.Text, optInStruct) {
			return true
		}
	}
	return false
}

func applyToFile(fn string, buf []byte) error {
	st, err := os.Stat(fn)
	if err != nil {
		return fmt.Errorf("%v: %w", ErrStatFile, err)
	}

	if !st.Mode().IsRegular() {
		return fmt.Errorf("%v", ErrNotRegularFile)
	}

	if err := maybe.WriteFile(fn, buf, st.Mode()); err != nil {
		return fmt.Errorf("%v: %w", ErrWriteFile, err)
	}

	return nil
}
