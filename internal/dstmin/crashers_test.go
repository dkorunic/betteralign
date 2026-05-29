// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

//go:build !gofuzz

package dstmin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReorderCrashersDoNotPanic replays every saved crasher input through
// reorderInvariant — the exact shared body the FuzzDecorateFileReorder
// driver runs — and asserts none of them panics or trips any of its
// oracles (valid output, comment-multiset preservation, field count,
// per-field signature). Acts as the regression net for crashes already
// fixed under fuzz/decoratefilereorder/ — any future change that
// reintroduces one will fail the corresponding subtest. Inputs are read
// from ../../fuzz/decoratefilereorder/crashers, which is populated by
// `task fuzz-go-decoratefilereorder`; the test skips silently when that
// directory is absent so CI without the fuzz workdir stays green. Each
// subtest is named after the crasher's hash so failures point straight
// back at the offending input.
func TestReorderCrashersDoNotPanic(t *testing.T) {
	dir := filepath.Join("..", "..", "fuzz", "decoratefilereorder", "crashers")
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
			reorderInvariant(t, string(data))
		})
	}
}
