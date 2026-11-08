// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package betteralign

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// dumpInflight writes src to a per-worker file under $FUZZ_INFLIGHT_DIR before
// each fuzz exec. When a worker is SIGKILL'd silently by the coordinator (a
// failure mode that emits no panic, no stack trace, just "EOF" from the
// framework), the file is the only record of the input the worker was
// processing. No-op when the env var is unset, so production test runs are
// unaffected.
func dumpInflight(src string) {
	dir := os.Getenv("FUZZ_INFLIGHT_DIR")
	if dir == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "worker-"+strconv.Itoa(os.Getpid())+".txt"), []byte(src), 0o600)
}

// markPhase writes a short phase tag to a per-worker file. Combined with
// dumpInflight's source capture, this lets us reconstruct after a silent
// worker death exactly what input was being processed AND which stage of
// the pipeline died on it: typecheck, walk, or check.
func markPhase(p string) {
	dir := os.Getenv("FUZZ_INFLIGHT_DIR")
	if dir == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "phase-"+strconv.Itoa(os.Getpid())+".txt"), []byte(p), 0o600)
}

// addFuzzCorpus walks every .go and .go.golden file under root and adds its
// bytes as a fuzz seed. Errors are silent; missing corpus just means fewer
// seeds, never a test failure.
func addFuzzCorpus(f *testing.F, root string) {
	f.Helper()
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".go.golden") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		f.Add(string(data))
		return nil
	})
}

// typeCheckFuzzSource parses and type-checks src, returning the package or, for
// uninteresting input, a nil package and a non-empty skip reason. It never
// touches *testing.T, so — unlike typeCheckFuzzInput — it is safe to call from a
// spawned goroutine (see replayHarnessCrashers, whose watchdog type-checks off
// the test goroutine).
//
// The Importer is nil so any imports resolve to errors-but-non-nil packages; the
// resulting *types.Package is still usable for struct introspection. The skip
// reason is "input not valid Go" on parser failure. Bytes are fed to
// parser.ParseFile directly via its src parameter — no temp file is created per
// exec, which matters at fuzz throughput where 56k tempdir create/cleanup
// cycles/sec would otherwise hammer the disk. Panics from go/types itself
// (malformed generics, recursive alias chains — known upstream defects) are
// recovered and returned as a skip reason so the fuzzer keeps hunting for real
// betteralign bugs.
//
// IgnoreFuncBodies: true bypasses constant folding inside function bodies
// (BUG-44). Adversarial inputs like `134291756e439044200-4-5-…` produce
// multi-million-digit rationals through go/constant's exact arithmetic;
// without the flag, one such 119-byte input pushed type-checking to 8.5 s
// and tripped a fuzzing watchdog. The fuzz invariants only look at
// package-scope named types via pkg.Scope().Names(), so skipping bodies
// is invisible to every assertion the harness makes.
func typeCheckFuzzSource(src string) (pkg *types.Package, skip string) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "input.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, "input not valid Go"
	}
	conf := types.Config{
		Error:            func(error) {}, // swallow type errors; partial info is enough
		Importer:         nil,            // imports resolve to errors; struct internals still type-check
		IgnoreFuncBodies: true,           // BUG-44: skip constant folding inside function bodies — saw 8.5s hang on huge float exponent
	}
	defer func() {
		if r := recover(); r != nil {
			pkg, skip = nil, fmt.Sprintf("go/types panic (upstream): %v", r)
		}
	}()
	pkg, _ = conf.Check("fuzz", fset, []*ast.File{file}, nil)
	return pkg, ""
}

// typeChecksCleanly reports whether src type-checks without errors, the only
// packages an analysis driver hands the analyzer.
func typeChecksCleanly(src string) bool {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "input.go", src, parser.SkipObjectResolution)
	if err != nil {
		return false
	}
	clean := true
	conf := types.Config{Error: func(error) { clean = false }, IgnoreFuncBodies: true}
	defer func() {
		if recover() != nil {
			clean = false
		}
	}()
	_, _ = conf.Check("fuzz", fset, []*ast.File{file}, nil)
	return clean
}

// typeCheckFuzzInput type-checks src for the fuzz targets, delegating to
// typeCheckFuzzSource and translating an uninteresting-input signal into t.Skip.
// It must be called from the goroutine running the fuzz/test function, because
// t.Skip calls SkipNow; the crasher-replay harness type-checks off the test
// goroutine and so calls typeCheckFuzzSource directly instead.
func typeCheckFuzzInput(t *testing.T, src string) *types.Package {
	t.Helper()
	pkg, skip := typeCheckFuzzSource(src)
	if skip != "" {
		t.Skip(skip)
	}
	return pkg
}

// FuzzOptimalOrder exercises optimalOrder on every named-struct type in
// arbitrary parser-accepted Go source. Invariants checked per struct:
//
//   - optimalOrder does not panic
//   - returned indexes are a valid permutation of [0, NumFields)
//   - returned optSize never exceeds the struct's original Sizeof
//   - returned optPtrdata never exceeds the struct's original ptrdata
//
// each under 64-bit sizes, 32-bit sizes (where only an align64 type, which
// needs sync/atomic and so never appears here, may break the last two), and
// 64-bit sizes measuring every named struct as reordered (gcSizes.projected),
// which recurses through rewriteCandidate on inner structs.
//
// Skip conditions (treated as "uninteresting input"): parser failure,
// type-checker producing nil package, struct with zero fields.
func FuzzOptimalOrder(f *testing.F) {
	addFuzzCorpus(f, "testdata")
	f.Add("package p\n\ntype S struct { a int; b byte; c int64 }\n")
	f.Add("package p\n\ntype S struct {}\n")
	f.Add("package p\n\ntype S struct { _ [0]func() }\n")
	f.Add("package p\n\ntype S[T any] struct { x T; y int }\n")

	f.Fuzz(func(t *testing.T, src string) {
		dumpInflight(src)
		markPhase("typecheck")
		pkg := typeCheckFuzzInput(t, src)
		if pkg == nil {
			t.Skip("type-check produced nil package")
		}
		markPhase("walk")
		models := []*gcSizes{newGCSizes(8, 8), newGCSizes(4, 4)}
		// Projection runs only where the analyzer does: drivers skip packages
		// with type errors (no RunDespiteErrors), and an invalid by-value
		// cycle (type e struct{ e }) yields figures no real pass can see.
		if typeChecksCleanly(src) {
			projected := newGCSizes(8, 8)
			projected.rewritten = func(*types.Struct) bool { return true }
			models = append(models, projected)
		}
		for _, sizes := range models {
			for _, name := range pkg.Scope().Names() {
				tn, ok := pkg.Scope().Lookup(name).(*types.TypeName)
				if !ok {
					continue
				}
				named, ok := tn.Type().(*types.Named)
				if !ok {
					continue
				}
				st, ok := named.Origin().Underlying().(*types.Struct)
				if !ok {
					continue
				}
				if st.NumFields() == 0 {
					continue
				}
				markPhase("check")
				checkOptimalOrderInvariants(t, name, st, sizes)
			}
		}
		markPhase("done")
	})
}

func checkOptimalOrderInvariants(t *testing.T, name string, st *types.Struct, sizes *gcSizes) {
	t.Helper()
	nf := st.NumFields()
	indexes, optSize, optPtrdata := optimalOrder(st, sizes)

	if len(indexes) != nf {
		t.Errorf("%s: len(indexes)=%d, want %d", name, len(indexes), nf)
		return
	}
	seen := make([]bool, nf)
	for _, idx := range indexes {
		if idx < 0 || idx >= nf {
			t.Errorf("%s: idx %d out of range [0,%d)", name, idx, nf)
			return
		}
		if seen[idx] {
			t.Errorf("%s: idx %d appears twice", name, idx)
			return
		}
		seen[idx] = true
	}

	// Non-negativity guard. Without this the upper-bound checks below are
	// trivially satisfied when origSize / origPtrdata saturate to MaxInt64,
	// hiding bugs where the optimalOrder accumulator wraps to a negative.
	if optSize < 0 {
		t.Errorf("%s: optSize=%d (negative; integer overflow in accumulator)", name, optSize)
	}
	if optPtrdata < 0 {
		t.Errorf("%s: optPtrdata=%d (negative; integer overflow in accumulator)", name, optPtrdata)
	}

	origSize := sizes.Sizeof(st)
	if optSize > origSize {
		t.Errorf("%s: optSize=%d > origSize=%d", name, optSize, origSize)
	}
	origPtrdata := sizes.ptrdata(st)
	if optPtrdata > origPtrdata {
		t.Errorf("%s: optPtrdata=%d > origPtrdata=%d", name, optPtrdata, origPtrdata)
	}
}

// FuzzGCSizes exercises Sizeof / Alignof / ptrdata on every named type in
// arbitrary parser-accepted Go source. Invariants:
//
//   - none of the three operations panics (this is what protected us against
//     the upstream "ptrdata panic on *types.TypeParam" bug)
//   - Sizeof >= ptrdata for any type (a pointer-bearing prefix can't exceed
//     the total size)
//   - Alignof >= 1 for any type (per the language spec)
//
// Skip conditions: parser failure, nil package.
func FuzzGCSizes(f *testing.F) {
	addFuzzCorpus(f, "testdata")
	f.Add("package p\n\ntype S struct { a int; b byte }\n")
	f.Add("package p\n\ntype I interface{ M() }\n")
	f.Add("package p\n\ntype G[T any] struct { x T }\n")
	f.Add("package p\n\ntype F func() error\n")
	f.Add("package p\n\ntype A [3]uint8\n")

	f.Fuzz(func(t *testing.T, src string) {
		dumpInflight(src)
		markPhase("typecheck")
		pkg := typeCheckFuzzInput(t, src)
		if pkg == nil {
			t.Skip("type-check produced nil package")
		}
		markPhase("walk")
		sizes := newGCSizes(8, 8)
		for _, name := range pkg.Scope().Names() {
			tn, ok := pkg.Scope().Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			typ := tn.Type()
			markPhase("check")
			checkGCSizesInvariants(t, name, typ, sizes)
		}
		markPhase("done")
	})
}

func checkGCSizesInvariants(t *testing.T, name string, typ types.Type, sizes *gcSizes) {
	t.Helper()
	sz := sizes.Sizeof(typ)
	al := sizes.Alignof(typ)
	pd := sizes.ptrdata(typ)
	if sz < 0 {
		t.Errorf("%s: Sizeof=%d (negative; integer overflow)", name, sz)
	}
	if pd < 0 {
		t.Errorf("%s: ptrdata=%d (negative; integer overflow)", name, pd)
	}
	if al < 1 {
		t.Errorf("%s: Alignof=%d, want >=1", name, al)
	}
	if pd > sz {
		t.Errorf("%s: ptrdata=%d > Sizeof=%d", name, pd, sz)
	}
}

// FuzzGeneratedByMatcher differential-tests the generated reGeneratedBy DFA
// against Go's regexp engine on the exact pattern it was generated from. The
// matcher in match_generated.go is emitted by the rec tool (see the `generate`
// Taskfile target), so any disagreement with the reference regex is a
// code-generation bug that would mis-classify generated files. Inputs are
// confined to the matcher's real domain — a single, valid-UTF-8 comment line —
// because hasGeneratedComment never feeds it anything else, and multiline ^/$
// or invalid UTF-8 would surface differences that aren't generation bugs.
func FuzzGeneratedByMatcher(f *testing.F) {
	ref := regexp.MustCompile(`^//\s*Code generated by .* DO NOT EDIT\.$`)
	for _, s := range []string{
		"// Code generated by foo. DO NOT EDIT.",
		"//   Code generated by protoc-gen-go. DO NOT EDIT.",
		"//\tCode generated by sqlc. DO NOT EDIT.",
		"//Code generated by x. DO NOT EDIT.",
		"// Code generated by x. DO NOT EDIT",
		"// code generated by x. DO NOT EDIT.",
		"// Code generated by x. DO NOT EDIT. ",
		"// Code generated. DO NOT EDIT.",
		"// Code generated by x. do not edit.",
		"package main",
		"",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		if strings.ContainsAny(s, "\n\r") || !utf8.ValidString(s) {
			t.Skip("outside the matcher's single-valid-comment-line domain")
		}
		if got, want := reGeneratedBy(s), ref.MatchString(s); got != want {
			t.Errorf("reGeneratedBy(%q) = %v, regexp = %v", s, got, want)
		}
	})
}

// fuzzOtherBuildSrc is the package FuzzScanOtherBuild resolves excluded
// files' literal paths against: every container shape resolvePath walks
// (named slices, arrays, maps, pointers, nesting, generic definitions and a
// generic alias, a self-referential slice). No unnamed struct is an element
// of a generic type: an instance would substitute it into a struct no
// declaration holds.
const fuzzOtherBuildSrc = `package fz

type S1 struct {
	a bool
	b int64
}

type S2 struct{ c int32 }

type G[T any] struct{ v T }

type (
	Sl     []S1
	PSl    []*S2
	M      map[S1]S2
	MP     map[string]*S1
	Arr    [3]S2
	Nest   [][]S1
	Ptr    *S1
	Inline []struct{ x int }
	Def    S2
	Al     = S1
	Self   []Self
	Rec    struct{ next *Rec }
	Ifc    interface{ M() }
	Fn     func(S1) S2
	Ch     chan S1
)

type List[E any] []E

type Map[K comparable, V any] map[K]V

type GA[T any] = []T

type Pair[A comparable, B any] map[A]B

type GInst List[G[S1]]
`

// fuzzOtherBuildPkg type-checks fuzzOtherBuildSrc and returns the package and
// every struct type written in it.
func fuzzOtherBuildPkg(tb testing.TB) (*types.Package, map[*types.Struct]bool) {
	tb.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fz.go", fuzzOtherBuildSrc, parser.SkipObjectResolution)
	if err != nil {
		tb.Fatal(err)
	}
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue)}
	pkg, err := (&types.Config{}).Check("example.com/fz", fset, []*ast.File{file}, info)
	if err != nil {
		tb.Fatal(err)
	}
	structs := make(map[*types.Struct]bool)
	ast.Inspect(file, func(n ast.Node) bool {
		if st, ok := n.(*ast.StructType); ok {
			structs[info.Types[st].Type.(*types.Struct)] = true
		}
		return true
	})
	return pkg, structs
}

// otherBuildResult is everything FuzzScanOtherBuild derives from one input.
type otherBuildResult struct {
	lits     map[string]string
	asm      map[string][]asmStub
	resolved map[string][]*types.Struct
}

// scanAndResolve scans each NUL-separated part of src as an excluded file,
// merges the scans as otherBuildPositional does, and resolves every literal
// path and assembly operand name as collectLayoutPins does.
func scanAndResolve(pkg *types.Package, src string) otherBuildResult {
	o := otherBuild{lits: make(map[string]string), asm: make(map[string][]asmStub), decls: make(map[string][]ast.Expr)}
	for i, part := range strings.Split(src, "\x00") {
		o.add(scanOtherBuildSrc("f"+strconv.Itoa(i)+".go", []byte(part)))
	}
	res := otherBuildResult{lits: o.lits, asm: o.asm, resolved: make(map[string][]*types.Struct)}
	for path := range o.lits {
		res.resolved["lit "+path] = resolveLiteralPath(pkg, path, o.decls)
	}
	for name := range o.asm {
		res.resolved["asm "+name] = resolveLiteralPath(pkg, name, o.decls)
	}
	return res
}

// FuzzScanOtherBuild feeds scanOtherBuild's syntactic scan and
// resolveLiteralPath arbitrary files. A build-excluded file can hold anything,
// including declarations no build compiles (type A B; type B A in each of
// several files once made resolution exponential), so beyond not panicking
// each input must resolve within a time bound, deterministically, and only to
// structs the package declares. A NUL separates files.
func FuzzScanOtherBuild(f *testing.F) {
	pkg, structs := fuzzOtherBuildPkg(f)
	addFuzzCorpus(f, filepath.Join("testdata", "src", "otherbuild"))
	for _, s := range []string{
		"package fz\ntype A B\ntype B A\nvar _ = A{{1}}\n\x00package fz\ntype A B\ntype B A\n\x00package fz\ntype A B\ntype B A\n",
		"package fz\ntype A A\nvar _ = A{{1}, {2}}\n",
		"package fz\ntype L[E any] []E\ntype A []L[A]\nvar _ = L[A]{{{1}}}\nvar _ = List[A]{{1}}\n",
		"package fz\nvar _ = Nest{{{true, 1}}}\nvar _ = M{{true, 2}: {3}}\nvar _ = MP{\"k\": {false, 4}}\n",
		"package fz\nvar _ = List[S1]{{true, 1}}\nvar _ = GA[S2]{{2}}\nvar _ = Map[S1, S2]{{true, 1}: {2}}\nvar _ = Pair[Sl, Arr]{{{true, 1}}: {{2}}}\n",
		"package fz\nvar _ = GInst{{S1{true, 1}}}\nvar _ = Self{{{}}}\nvar _ = PSl{{1}}\nvar _ = Al{true, 1}\nvar _ = Def{1}\n",
		"package fz\ntype t []Sl\ntype u map[t]*Arr\nvar _ = u{{{{true, 1}}}: {{2}}}\nvar _ = []t{{{{true, 3}}}}\n",
		"package fz\nfunc f(a *S1, b ...S2) (Sl, [4]*Arr)\nfunc (r *Rec) g(List[S1])\n//go:linkname h runtime.h\nfunc h(Nest)\n",
		"\xef\xbb\xbfpackage fz\r\ntype A []S1\r\nvar _ = A{{true, 1}}\r\n",
		"//go:build ignore\n\npackage fz\nvar _ = S1{true, 1}\n",
		"package fz\ntype T struct { /*line x:1*/ a bool; b int64 }\nvar _ = T{true, 1}\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		// Both runs must be slow to fail: blowup repeats, a scheduler or GC
		// stall under a loaded fuzzer rarely does.
		start := time.Now()
		first := scanAndResolve(pkg, src)
		d1 := time.Since(start)
		second := scanAndResolve(pkg, src)
		if d2 := time.Since(start) - d1; min(d1, d2) > 200*time.Millisecond {
			t.Fatalf("scanning and resolving took %v and %v, want < 200ms", d1, d2)
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("nondeterministic:\nfirst  %+v\nsecond %+v", first, second)
		}
		for key, sts := range first.resolved {
			for _, st := range sts {
				if !structs[st] {
					t.Errorf("%s resolved to %v, not a struct the package declares", key, st)
				}
			}
		}
	})
}

// FuzzDeclSourceDirectives feeds the per-file text checks arbitrary files:
// newDeclSource and declSource.at (which read whatever file a type's position
// names, however inconsistent with it — BUG-45), lineDirective, linkTarget,
// neverBuilt and hasGeneratedComment. None may panic; at must only return the
// struct declared with that first field on the requested line, and nothing in
// a file with a line directive is reorderable.
func FuzzDeclSourceDirectives(f *testing.F) {
	_ = filepath.WalkDir(filepath.Join("testdata", "src", "otherbuild"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if data, err := os.ReadFile(path); err == nil {
			f.Add(string(data), 5)
		}
		return nil
	})
	for _, s := range []struct {
		src  string
		line int
	}{
		{"package p\ntype T struct { // betteralign:ignore\n\ta bool\n}", 3},
		{"package p\ntype T struct{ a bool; b int64 }", 2},
		{"", 0},
		{"package p\ntype T struct {\n\t/*line x:1*/ a bool\n\tb int64\n}\n", 3},
		{"package p\n//line x.go:10\ntype T struct {\n\ta bool\n}\n", 4},
		{"package p\n//line x.go:10:1\ntype T struct {\n\ta bool\n}\n\t//line indented.go:3\n", 4},
		{"package p\n/*line :7*/type T struct {\n\ta bool\n}\n", 3},
		{"\xef\xbb\xbfpackage p\r\ntype T struct {\r\n\ta bool\r\n}\r\n", 3},
		{"// Code generated by x. DO NOT EDIT.\n\npackage p\ntype T struct {\n\ta, b bool\n}\n", 5},
		{"//go:build ignore\n\npackage p\n//go:linkname f p.f\nfunc f()\n//go:linkname g\nfunc g()\nfunc (T) f()\n", 4},
		{"package p\nfunc f()\n\n//go:linkname f runtime.f extra\nvar x int\ntype T struct {\n\tint\n}\n", 7},
		{"package p\ntype (\n\tA struct {\n\t\tx int\n\t}\n\tB struct{ x int }\n)\nfunc f() { type C struct {\n\ta int\n}}\n", 4},
	} {
		f.Add(s.src, s.line)
	}
	f.Fuzz(func(t *testing.T, src string, line int) {
		ds := newDeclSource("f.go", []byte(src))
		_ = neverBuilt([]byte(src))
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "f.go", src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			if _, ok := ds.at(line, "a"); ok {
				t.Fatalf("at(%d) found a declaration in a file that does not parse", line)
			}
			return
		}
		_ = hasGeneratedComment(file)
		held := lineDirective(fset, file)
		for _, d := range file.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok {
				if target := linkTarget(linkTargets(file), fn); target != "" && fn.Recv != nil {
					t.Errorf("linkTarget(method %s) = %q, want none", fn.Name.Name, target)
				}
			}
		}
		// Every struct declaration's first field, by unadjusted line.
		firsts := make(map[int][]string)
		ast.Inspect(file, func(n ast.Node) bool {
			if ts, ok := n.(*ast.TypeSpec); ok {
				if st, ok := ts.Type.(*ast.StructType); ok && st.Fields != nil && len(st.Fields.List) > 0 {
					first := st.Fields.List[0]
					name := embeddedName(first.Type)
					if len(first.Names) > 0 {
						name = first.Names[0].Name
					}
					l := fset.PositionFor(first.Pos(), false).Line
					firsts[l] = append(firsts[l], name)
				}
			}
			return true
		})
		check := func(l int, field string) {
			d, ok := ds.at(l, field)
			if !ok {
				return
			}
			if !slices.Contains(firsts[l], d.field) || d.field != field {
				t.Errorf("at(%d, %q) = first field %q, declared there: %q", l, field, d.field, firsts[l])
			}
			if held && d.reorderable {
				t.Errorf("at(%d, %q) reorderable in a file with a line directive", l, field)
			}
		}
		check(line, "a")
		for l, names := range firsts {
			for _, name := range names {
				check(l, name)
			}
		}
	})
}
