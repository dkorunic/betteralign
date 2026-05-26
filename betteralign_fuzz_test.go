// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package betteralign

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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

// typeCheckFuzzInput parses and type-checks src. The Importer is nil so any
// imports resolve to errors-but-non-nil packages; the resulting *types.Package
// is still usable for struct introspection. Returns nil on parser failure
// (uninteresting input). Bytes are fed to parser.ParseFile directly via its
// src parameter — no temp file is created per exec, which matters at fuzz
// throughput where 56k tempdir create/cleanup cycles/sec would otherwise
// hammer the disk. Panics from go/types itself (malformed generics, recursive
// alias chains — known upstream defects) are recovered and reported as Skip
// so the fuzzer keeps hunting for real betteralign bugs.
func typeCheckFuzzInput(t *testing.T, src string) (pkg *types.Package) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "input.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Skip("input not valid Go")
	}
	conf := types.Config{
		Error:    func(error) {}, // swallow type errors; partial info is enough
		Importer: nil,            // imports resolve to errors; struct internals still type-check
	}
	defer func() {
		if r := recover(); r != nil {
			t.Skipf("go/types panic (upstream): %v", r)
		}
	}()
	pkg, _ = conf.Check("fuzz", fset, []*ast.File{file}, nil)
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
		sizes := newGCSizes(8, 8)
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
