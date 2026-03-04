# Test Improvements Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix state-accumulation bugs in the test helper and add test coverage for `betteralign:ignore`, `test_files`, `generated_files`, and grouped `type (...)` declarations.

**Architecture:** Each gap is addressed by (a) adding testdata under `testdata/src/` following the existing pattern of per-scenario directories with `// want` annotations, and (b) adding or updating test functions in `betteralign_test.go`. The `StringArrayFlag` state bug is fixed at the production-code boundary by exporting `ResetFlags()` from `betteralign.go` and calling it at the top of `NewTestAnalyzer()`.

**Tech Stack:** Go, `golang.org/x/tools/go/analysis/analysistest`, `gotest.tools/v3/golden`

---

## Task 1: Export `ResetFlags()` in `betteralign.go`

**Files:**
- Modify: `betteralign.go` (after the `init()` function, around line 143)

**Step 1: Add `ResetFlags` function**

Insert after `func init()` (line 143 area):

```go
// ResetFlags resets all package-level flag variables to their zero values.
// Tests must call this (via NewTestAnalyzer) before each run to prevent
// StringArrayFlag.Set from accumulating values across test functions.
func ResetFlags() {
	fApply = false
	fTestFiles = false
	fGeneratedFiles = false
	fOptInMode = false
	fExcludeFiles = nil
	fExcludeDirs = nil
}
```

**Step 2: Run existing tests to confirm no regression**

```bash
go test ./... -run TestFlag
```
Expected: all pass (same behaviour as before).

---

## Task 2: Fix `NewTestAnalyzer()` to call `ResetFlags`

**Files:**
- Modify: `betteralign_test.go` (the `NewTestAnalyzer` function, lines 51–60)

**Step 1: Add `betteralign.ResetFlags()` call**

Replace the body of `NewTestAnalyzer`:

```go
func NewTestAnalyzer() *analysis.Analyzer {
	betteralign.ResetFlags()
	analyzer := &analysis.Analyzer{
		Name:     betteralign.Analyzer.Name,
		Doc:      betteralign.Analyzer.Doc,
		Requires: betteralign.Analyzer.Requires,
		Run:      betteralign.Analyzer.Run,
	}
	betteralign.InitAnalyzer(analyzer)
	return analyzer
}
```

**Step 2: Run all tests**

```bash
go test ./...
```
Expected: `ok github.com/dkorunic/betteralign`.

---

## Task 3: Add `betteralign:ignore` test case to `testdata/src/a/a.go`

**Files:**
- Modify: `testdata/src/a/a.go` (append new struct at end)
- Modify: `testdata/src/a/a.go.golden` (append matching unchanged struct)

**Step 1: Append to `testdata/src/a/a.go`**

```go
type IgnoredBad struct {
	// betteralign:ignore
	x byte
	y int32
	z byte
}
```

No `// want` annotation — the ignore comment must suppress the diagnostic.

**Step 2: Append the same block to `testdata/src/a/a.go.golden`**

```go
type IgnoredBad struct {
	// betteralign:ignore
	x byte
	y int32
	z byte
}
```

The struct is unchanged in the golden file because the rewrite is skipped.

**Step 3: Run tests**

```bash
go test ./... -run TestSuggestions
go test ./... -run TestApply
```
Expected: both pass. If `TestApply` fails with a golden-diff, check that the golden file matches exactly (spacing, trailing newline) what `decorator.Fprint` produces.

---

## Task 4: Add grouped `type (...)` declaration test case

**Files:**
- Modify: `testdata/src/a/a.go` (append grouped declaration)
- Modify: `testdata/src/a/a.go.golden` (append reordered grouped declaration)

**Step 1: Append to `testdata/src/a/a.go`**

```go
type (
	GroupedBad1 struct { // want "struct of size 12 could be 8"
		x byte
		y int32
		z byte
	}
	GroupedBad2 struct { // want "struct of size 12 could be 8"
		x byte
		y int32
		z byte
	}
)
```

Both structs have identical bad layout; both must produce diagnostics. This verifies that the second struct in a grouped declaration is not silently skipped.

**Step 2: Append to `testdata/src/a/a.go.golden`**

```go
type (
	GroupedBad1 struct { // want "struct of size 12 could be 8"
		y int32
		x byte
		z byte
	}
	GroupedBad2 struct { // want "struct of size 12 could be 8"
		y int32
		x byte
		z byte
	}
)
```

Fields are reordered to optimal layout.

**Step 3: Run tests**

```bash
go test ./... -run TestSuggestions
go test ./... -run TestApply
```

Expected: both pass. If `TestApply` golden comparison fails due to whitespace, adjust the golden file to exactly match `decorator.Fprint` output (run `TestApply` once with `-update` flag from `gotest.tools/v3/golden` or inspect the temp file manually).

---

## Task 5: Add `test_files` flag testdata

**Files:**
- Create: `testdata/src/testfiles/a/testfiles.go`
- Create: `testdata/src/testfiles/a/testfiles_test.go`
- Create: `testdata/src/testfiles/b/testfiles.go`
- Create: `testdata/src/testfiles/b/testfiles_test.go`

**Step 1: Create `testdata/src/testfiles/a/testfiles.go`** (empty package, no structs)

```go
package testfiles
```

**Step 2: Create `testdata/src/testfiles/a/testfiles_test.go`** (bad struct, NO `// want`)

The struct is suboptimally laid out but should produce **no diagnostic** because `test_files` is not set:

```go
package testfiles

type TestBad struct {
	x byte
	y int32
	z byte
}
```

**Step 3: Create `testdata/src/testfiles/b/testfiles.go`** (empty package)

```go
package testfiles
```

**Step 4: Create `testdata/src/testfiles/b/testfiles_test.go`** (bad struct, WITH `// want`)

The struct must produce a diagnostic when `test_files=true`:

```go
package testfiles

type TestBad struct { // want "struct of size 12 could be 8"
	x byte
	y int32
	z byte
}
```

---

## Task 6: Add `TestFlagTestFiles` to `betteralign_test.go`

**Files:**
- Modify: `betteralign_test.go` (append after `TestFlagOptInMode`)

**Step 1: Append test function**

```go
func TestFlagTestFiles(t *testing.T) {
	t.Run("test files excluded by default", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		analysistest.Run(t, testdata, analyzer, "testfiles/a")
	})

	t.Run("test files included with flag", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		analyzer.Flags.Set("test_files", "true")
		analysistest.Run(t, testdata, analyzer, "testfiles/b")
	})
}
```

**Step 2: Run new test**

```bash
go test ./... -run TestFlagTestFiles -v
```
Expected: both sub-tests pass.

---

## Task 7: Add `generated_files` flag testdata

Tests the comment-based generated-file detection (`hasGeneratedComment`) using the standard `// Code generated by … DO NOT EDIT.` header.

**Files:**
- Create: `testdata/src/generated/a/gen.go`
- Create: `testdata/src/generated/b/gen.go`

**Step 1: Create `testdata/src/generated/a/gen.go`** (generated header + bad struct, NO `// want`)

```go
// Code generated by tool. DO NOT EDIT.

package generated

type GenBad struct {
	x byte
	y int32
	z byte
}
```

**Step 2: Create `testdata/src/generated/b/gen.go`** (generated header + bad struct, WITH `// want`)

```go
// Code generated by tool. DO NOT EDIT.

package generated

type GenBad struct { // want "struct of size 12 could be 8"
	x byte
	y int32
	z byte
}
```

---

## Task 8: Add `TestFlagGeneratedFiles` to `betteralign_test.go`

**Files:**
- Modify: `betteralign_test.go` (append after `TestFlagTestFiles`)

**Step 1: Append test function**

```go
func TestFlagGeneratedFiles(t *testing.T) {
	t.Run("generated files excluded by default", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		analysistest.Run(t, testdata, analyzer, "generated/a")
	})

	t.Run("generated files included with flag", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		analyzer.Flags.Set("generated_files", "true")
		analysistest.Run(t, testdata, analyzer, "generated/b")
	})
}
```

**Step 2: Run new test**

```bash
go test ./... -run TestFlagGeneratedFiles -v
```
Expected: both sub-tests pass.

---

## Task 9: Final verification

**Step 1: Run the full test suite**

```bash
go test ./...
```
Expected: `ok github.com/dkorunic/betteralign`

**Step 2: Run tests with race detector**

```bash
go test -race ./...
```
Expected: pass with no data race warnings.
