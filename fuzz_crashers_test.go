// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

//go:build !gofuzz

package betteralign

import (
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOptimalOrderCrashersDoNotHang replays every saved go-fuzz crasher
// from fuzz/optimalorder/crashers/ through the same logic
// FuzzOptimalOrder runs and asserts each input completes well under the
// 10 s go-fuzz watchdog. Acts as the regression net for BUG-44: an
// adversarial float-exponent literal in a function body (134291756e
// 439044200 - 4 - 5 - …) pushed go/types constant folding to 8.5 s
// before IgnoreFuncBodies: true was wired into typeCheckFuzzInput. The
// per-subtest deadline of 2 s is conservative — the fix brings the
// pathological input down to microseconds. Skipped silently when the
// crashers directory is absent.
func TestOptimalOrderCrashersDoNotHang(t *testing.T) {
	replayHarnessCrashers(t, "fuzz/optimalorder/crashers", runOptimalOrderInvariant)
}

// TestGCSizesCrashersDoNotHang is the BUG-44 regression net for the
// FuzzGCSizes side. Mirrors TestOptimalOrderCrashersDoNotHang.
func TestGCSizesCrashersDoNotHang(t *testing.T) {
	replayHarnessCrashers(t, "fuzz/gcsizes/crashers", runGCSizesInvariant)
}

// replayHarnessCrashers walks dir for raw crasher inputs (files with no
// extension, paired with .output and .quoted siblings go-fuzz produces),
// runs each through fn under a 2 s deadline, and reports per-input
// failures so the failing hash is easy to map back to the offending
// input. Skips when the directory does not exist so CI without a fuzz
// workdir stays green.
func replayHarnessCrashers(t *testing.T, dir string, fn func(t *testing.T, src string)) {
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
				fn(t, string(data))
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("harness did not complete within 2s — IgnoreFuncBodies regression")
			}
		})
	}
}

// runOptimalOrderInvariant mirrors the FuzzOptimalOrder body so the
// regression test exercises the same path. Inputs the original harness
// would have Skipped surface here as silent acceptance, matching the
// "uninteresting" classification.
func runOptimalOrderInvariant(t *testing.T, src string) {
	t.Helper()
	pkg := typeCheckFuzzInput(t, src)
	if pkg == nil {
		return
	}
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

// runGCSizesInvariant mirrors the FuzzGCSizes body so the regression
// test exercises the same path.
func runGCSizesInvariant(t *testing.T, src string) {
	t.Helper()
	pkg := typeCheckFuzzInput(t, src)
	if pkg == nil {
		return
	}
	sizes := newGCSizes(8, 8)
	for _, name := range pkg.Scope().Names() {
		tn, ok := pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok {
			continue
		}
		checkGCSizesInvariants(t, name, tn.Type(), sizes)
	}
}
