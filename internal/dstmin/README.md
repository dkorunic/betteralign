# internal/dstmin

Minimal hand-written replacement for the betteralign-relevant subset of
`github.com/sirkon/dst` and `github.com/sirkon/dst/decorator`. Supports
exactly one mutation — reordering the field list of an `*ast.StructType` —
and reprints the file by byte-splicing the modified struct bodies into the
original source, then finalising through `go/format.Source` for column
alignment.

Comments and blank lines are preserved via a **dual-attachment** model:
each inter-field blank in source is decorated as a trail-blank on the
preceding field AND a lead-blank on the following field. On reorder both
emit; `go/format.Source` then naturally coalesces the duplicate and strips
brace-adjacent blanks, matching `sirkon/dst`'s observable output.

The package is intentionally not exported; it exists only to remove the
~17k LOC `sirkon/dst` dependency from `betteralign` while keeping the
caller diff to two import lines and two call-site renames.

## Implementation outline

1. **`DecorateFileSrc` (`*ast.File` + source → `*File`)** does the decoration;
   `DecorateFile` is a thin wrapper that reads the file from disk first. It
   never errors — any layout that can't be safely span-decorated is dropped
   from the result and simply left unrewritten. The work is:
   - A **single preorder walk** registers every decoratable `*ast.StructType`
     and, in the same pass, records the body ranges of nested structs (even
     rejected ones) against their decorated ancestors, so inner-struct comments
     don't leak into the enclosing struct's classification. Each candidate is
     filtered through `buildStruct` plus three safety guards, each consulting a
     per-struct comment run narrowed by binary search (`commentRun`) rather than
     the whole `f.Comments` slice:
     - `hasUnsafeBlockComment` — unsafe block-comment layouts: crossing the `{`
       line, on the `}` line, multi-line on a field line, or in a multi-comment
       group (BUG-32/33/41/44);
     - `hasBuildConstraintComment` — mid-body `//go:build` constraints (BUG-42);
     - `hasLineDirectiveComment` — `//line` / `/*line*/` directives (BUG-43).
   - **Comment routing** (`decorateComments`) classifies each comment group in a
     surviving struct body with four rules: opening-brace (1), lead-doc (2),
     trailing-line / multi-line-block extension (3), and floating block (4).
   - A final per-field pass computes `leadBlanks` / `trailBlanks` byte ranges
     for the dual-attachment emit.

2. **`Fprint`** short-circuits to `w.Write(f.source)` when no struct was
   mutated. For dirty files, it splices the original source around
   synthesized struct bodies, runs `go/format.Source` for column
   alignment, and re-parses to validate before writing.

## Testing

- ~30 decoration-level unit tests (`TestDecorateFile_*`, `TestReorder_*`)
  covering corner cases: nested structs, rejected layouts (single-line braces,
  field-on-`{`-line, unsafe block comments, mid-body build constraints and line
  directives), comment classification, and lead/trail attachment.
- A table-driven `TestFprint` with 12 scenarios from identity to
  multi-blank-line reorder, plus `TestFprint_GofmtCorruptedOutputIsRejected`,
  a regression for the form-feed-in-import-path gofmt corruption case.
- `TestCommentRun_MatchesBruteForceForEveryStruct` cross-checks the
  binary-search comment-run narrowing against a brute-force scan, and
  `TestReorderCrashersDoNotPanic` replays the committed crash corpus.
- Go-native fuzz targets `FuzzDecorateFileIdentity` and `FuzzDecorateFileReorder`,
  seeded with hand-crafted edge cases and every `.go` / `.go.golden` file from
  the project's testdata; the reorder oracle is itself guarded by the
  `TestCommentTextsImmune*` invariant tests.
- A committed reorder regression corpus (~34 seeds) under
  `testdata/fuzz/FuzzDecorateFileReorder/`, including named cases for the
  BUG-31..44 reorder issues surfaced by later fuzzing and targeted analysis:
  genuine splice bugs (since fixed), unsafe layouts the decorator now rejects,
  and fuzz-oracle false positives pinned as regressions.

## Performance vs `github.com/sirkon/dst`

Measured on Xeon E5-1630 v3 @ 3.70GHz, Go 1.26.3, Linux.

### Microbenchmark (`go test -bench=. -benchmem -count=10`)

Same parser-prepared `*ast.File` reused across all iterations. Parsing is
outside the timer. Benchstat with `n=10`; all deltas significant at
`p=0.000`.

| Operation | `sirkon/dst` | `internal/dstmin` | Δ wall-clock | Δ B/op | Δ allocs/op |
| --- | ---: | ---: | ---: | ---: | ---: |
| `DecorateFile` | 55.05 µs | 8.91 µs | **−83.81%** (6.2×) | −71.91% | −88.93% |
| `FprintIdentity` (clean-file pass-through) | 27.73 µs | 24.66 ns | **−99.91%** (~1100×) | −100% | −100% |
| `DecorateReorderPrint` (decorate + swap two fields + print) | 93.53 µs | 56.91 µs | **−39.15%** (1.64×) | −44.31% | −29.93% |
| geomean | 52.27 µs | 2.32 µs | **−95.56%** | | |

### Macro benchmark (end-to-end `betteralign -apply ./...`)

Synthetic corpus: 500 misaligned structs across 50 files in 10 packages
(248 KB total source). Each struct has lead-doc, trailing line comments,
a struct tag, and a floating blank-line-separated trailing comment block —
exercising every classifier rule. `hyperfine` with 3 warmup + 20 timed
runs per binary, alternating, fresh corpus copied for every run.

| Implementation | Wall-clock mean | Min | Max | User CPU mean |
| --- | ---: | ---: | ---: | ---: |
| `sirkon/dst` (pre-migration) | 782.3 ± 127.0 ms | 584.0 ms | 1124.7 ms | 240.4 ms |
| `internal/dstmin` (HEAD) | **698.0 ± 70.0 ms** | 582.6 ms | 894.3 ms | **105.2 ms** |
| ratio | **1.12× faster** | | | **2.29× less CPU** |

Both binaries produce **byte-identical output** on the corpus (verified
with `diff -r` between the two reordered trees).

### Binary size

| Implementation | Stripped binary | Δ |
| --- | ---: | ---: |
| `sirkon/dst` (pre-migration) | 7,696,546 B (7.34 MB) | — |
| `internal/dstmin` (HEAD) | **7,270,562 B (6.93 MB)** | **−5.5%** |

### Dependency footprint

| Implementation | Direct deps in `go.mod` |
| --- | ---: |
| `sirkon/dst` (pre-migration) | 6 |
| `internal/dstmin` (HEAD) | **5** (no `sirkon/dst`) |

## Why dstmin wins

- **The clean-file fast path is essentially free.** Most files in a real
  codebase do not have a misaligned struct. `Fprint` on a clean file is
  literally `w.Write(f.source)` — one syscall, zero allocations, ~25 ns.
  `sirkon/dst` always reconstructs the AST through its restorer and runs
  `go/printer` regardless, paying ~28 µs and 148 allocations on every
  no-op. For betteralign's typical workload (analyze many files,
  mutate few) this dominates the wall-clock savings.

- **Decoration is 6.2× cheaper.** `sirkon/dst` materialises one fragment
  object (`commentFragment`, `newlineFragment`, `tokenFragment`, ...) for
  every comment and token in the file, sorts them by position, then walks
  the result. dstmin walks the AST once and records byte offsets — 28
  allocations vs 253 for the same input.

- **Mutation reuses the existing source.** dstmin synthesizes only the
  modified struct body and byte-splices it back into the original source,
  then runs `go/format.Source` once to re-align trailing-comment columns.
  `sirkon/dst` rebuilds the full AST, runs a fragment restorer, and
  reprints the whole file through `go/printer`.

- **The macro speedup is muted by I/O.** The end-to-end analyzer wall-clock
  improvement (~12%) is smaller than the per-operation improvement because
  process startup, AST parsing, type-checking, and atomic file writes are
  shared between the implementations. The User CPU time is a cleaner
  signal (2.29× less) — that is where dstmin pays back.

- **Allocation pressure is much lower, so GC pauses are rarer.** In the
  macro run, `sirkon/dst`'s upper tail reached 1.12 s (1.44× its own mean)
  while dstmin's max was 894 ms (1.28× its mean). The tight dstmin
  distribution is consistent with fewer transient allocations and less
  GC work during the per-file pipeline.

## Reproducing the benchmarks

The numbers above were collected during the initial migration from
`sirkon/dst` to `internal/dstmin`. The recipes below reproduce them
against any prior commit that still uses `sirkon/dst` (i.e. before this
package was added). Substitute `<sirkon-rev>` with such a commit.

### Microbenchmark

Three benchmarks (`DecorateFile`, `FprintIdentity`,
`DecorateReorderPrint`) live in a temporary `bench_compare_test.go` file
on each side, exercising the same synthetic Go source. The dstmin
version uses the package's own types; the sirkon/dst version lives at
the repo root and imports `github.com/sirkon/dst`. Both record `name`,
`code`, and `mutate func` — only the API calls differ.

```sh
# Capture dstmin numbers.
GOTMPDIR=/tmp go test -run='^$' -bench='Benchmark' -benchmem -count=10 \
  ./internal/dstmin/ > /tmp/bench-dstmin.txt

# Switch to a commit that still uses sirkon/dst, drop in the equivalent
# bench file at the repo root, and capture.
git checkout <sirkon-rev>
GOTMPDIR=/tmp go test -run='^$' -bench='Benchmark' -benchmem -count=10 \
  . > /tmp/bench-sirkon.txt
git checkout -

# Normalise the pkg: line so benchstat can align the two reports.
sed -i 's|^pkg:.*|pkg: bench|' /tmp/bench-dstmin.txt /tmp/bench-sirkon.txt
benchstat -col=.file /tmp/bench-sirkon.txt /tmp/bench-dstmin.txt
```

### Macro benchmark

Generate a corpus of 500 misaligned structs (10 packages × 5 files × 10
structs), build both binaries, and compare with `hyperfine`:

```sh
# Build dstmin binary from HEAD.
go build -trimpath -ldflags="-s -w" -o /tmp/betteralign-dstmin ./cmd/betteralign

# Build sirkon/dst binary from a prior revision.
git checkout <sirkon-rev>
go build -trimpath -ldflags="-s -w" -o /tmp/betteralign-dst ./cmd/betteralign
git checkout -

# Generate the misaligned-struct corpus at /tmp/bench-corpus
# (10 packages × 5 files × 10 structs each, with lead-doc, trailing
# line comments, struct tags, floating trailing blocks).

hyperfine \
  --prepare 'rm -rf /tmp/bench-work && cp -r /tmp/bench-corpus /tmp/bench-work' \
  --warmup 3 --runs 20 --ignore-failure \
  --command-name 'dstmin'     'cd /tmp/bench-work && /tmp/betteralign-dstmin -apply ./... >/dev/null 2>&1' \
  --command-name 'sirkon/dst' 'cd /tmp/bench-work && /tmp/betteralign-dst   -apply ./... >/dev/null 2>&1'
```
