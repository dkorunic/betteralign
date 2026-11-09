// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package betteralign

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis"
)

// TestUntrimmed pins declDir's mapping of -trimpath file names back to disk:
// without it GOFLAGS=-trimpath, common in CI, left export data naming
// go.uber.org/atomic@v1.11.0/int64.go, no directory, so the cross-package
// atomic pin silently missed its main target. A module-cache path is
// case-escaped (!foo), falls back to vendor/ without the version when the
// cache lacks it; the main module's path maps to its directory, found from
// the pass's own file when the driver leaves Module.Dir unset; a std file
// trims to its import path (os.file's internal/poll.FD).
func TestUntrimmed(t *testing.T) {
	tmp := t.TempDir()
	cache, root, bare := filepath.Join(tmp, "cache"), filepath.Join(tmp, "m"), filepath.Join(tmp, "bare")
	for _, d := range []string{
		filepath.Join(cache, "github.com", "!foo", "bar@v1.0.0", "x"),
		filepath.Join(root, "vendor"),
		filepath.Join(root, "p"),
		bare,
	} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := gomodcache
	gomodcache = func() string { return cache }
	t.Cleanup(func() { gomodcache = saved })

	mainMod := &analysis.Module{Path: "example.com/m", Dir: root}
	self := filepath.Join(root, "p", "p.go")
	for _, tc := range []struct {
		fn   string
		mod  *analysis.Module
		self string
		want string
	}{
		{"github.com/Foo/bar@v1.0.0/x/a.go", nil, "", filepath.Join(cache, "github.com", "!foo", "bar@v1.0.0", "x", "a.go")},
		{"github.com/Foo/baz@v1.0.0/a.go", mainMod, "", filepath.Join(root, "vendor", "github.com", "Foo", "baz", "a.go")},
		{"github.com/Foo/baz@v1.0.0/a.go", nil, "", ""},
		{"example.com/m/sub/a.go", mainMod, "", filepath.Join(root, "sub", "a.go")},
		{"example.com/m/sub/a.go", &analysis.Module{Path: "example.com/m"}, self, filepath.Join(root, "sub", "a.go")},
		{"golang.org/x/net/http2/h.go", mainMod, "", filepath.Join(root, "vendor", "golang.org", "x", "net", "http2", "h.go")},
		{"golang.org/x/net/http2/h.go", &analysis.Module{Path: "example.com/b", Dir: bare}, "", ""},
		{"internal/poll/fd_mutex.go", nil, "", filepath.Join(goroot(), "src", "internal", "poll", "fd_mutex.go")},
		{"nosuchstd/x.go", mainMod, "", filepath.Join(root, "vendor", "nosuchstd", "x.go")},
	} {
		if got := untrimmed(tc.fn, tc.mod, tc.self); got != tc.want {
			t.Errorf("untrimmed(%q) = %q, want %q", tc.fn, got, tc.want)
		}
	}
}
