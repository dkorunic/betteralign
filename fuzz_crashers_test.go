// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package betteralign

import (
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOptimalOrderCrashersDoNotHang replays every saved crasher from
// fuzz/optimalorder/crashers/ through the same logic FuzzOptimalOrder
// runs and asserts each input completes well under a fuzzing watchdog.
// Acts as the regression net for BUG-44: an adversarial float-exponent
// literal in a function body (134291756e439044200 - 4 - 5 - …) pushed
// go/types constant folding to 8.5 s before IgnoreFuncBodies: true was
// wired into typeCheckFuzzInput. The per-subtest deadline of 2 s is
// conservative — the fix brings the pathological input down to
// microseconds. Skipped silently when the crashers directory is absent.
func TestOptimalOrderCrashersDoNotHang(t *testing.T) {
	replayHarnessCrashers(t, "fuzz/optimalorder/crashers", runOptimalOrderInvariant)
}

// TestGCSizesCrashersDoNotHang is the BUG-44 regression net for the
// FuzzGCSizes side. Mirrors TestOptimalOrderCrashersDoNotHang.
func TestGCSizesCrashersDoNotHang(t *testing.T) {
	replayHarnessCrashers(t, "fuzz/gcsizes/crashers", runGCSizesInvariant)
}

// replayHarnessCrashers runs each raw crasher input (extension-less files;
// .output/.quoted/.metadata siblings skipped) through fn under a 2 s deadline,
// as a per-input subtest. Skips when dir is absent so CI without a fuzz workdir
// stays green.
//
// The replay runs in a watchdog goroutine to catch a BUG-44-style hang, so it
// must avoid SkipNow/FailNow off the test goroutine: type-checking goes through
// the *testing.T-free typeCheckFuzzSource (silent return on uninteresting
// input), fn asserts only via t.Errorf, and t.Fatalf stays on the test goroutine.
func replayHarnessCrashers(t *testing.T, dir string, fn func(t *testing.T, pkg *types.Package)) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("crashers dir unavailable: %v", err)
	}
	var inputs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.Contains(name, ".") {
			continue
		}
		inputs = append(inputs, name)
	}
	if len(inputs) == 0 {
		t.Skip("no crasher inputs found")
	}
	for _, name := range inputs {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("read crasher %s: %v", name, err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				pkg, skip := typeCheckFuzzSource(string(data))
				if skip != "" || pkg == nil {
					return // uninteresting input: silent acceptance
				}
				fn(t, pkg)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("harness did not complete within 2s — IgnoreFuncBodies regression")
			}
		})
	}
}

// runOptimalOrderInvariant mirrors the FuzzOptimalOrder walk (type-checking and
// skips happen in replayHarnessCrashers) and asserts via t.Errorf only.
func runOptimalOrderInvariant(t *testing.T, pkg *types.Package) {
	t.Helper()
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
		checkOptimalOrderInvariants(t, name, st, sizes)
	}
}

// runGCSizesInvariant mirrors the FuzzGCSizes walk so the regression test
// exercises the same path.
func runGCSizesInvariant(t *testing.T, pkg *types.Package) {
	t.Helper()
	sizes := newGCSizes(8, 8)
	for _, name := range pkg.Scope().Names() {
		tn, ok := pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok {
			continue
		}
		checkGCSizesInvariants(t, name, tn.Type(), sizes)
	}
}
