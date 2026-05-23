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

1. **DecorateFile (`*ast.File` → `*File`)** runs in four passes:
   - Pass 1: walk the AST and register every decoratable `*ast.StructType`.
   - Pass 2: collect every nested struct body range (including rejected
     ones — single-line braces or field-on-`{`-line layouts that can't
     be safely span-decorated), so their comments don't leak into the
     enclosing struct's classification.
   - Pass 3: classify every comment group inside each struct body via
     five rules (opening-brace, trailing-line, lead-doc, multi-line block
     extension, floating). The full rule set lives in `decorateComments`.
   - Pass 4: compute per-field `leadBlanks` and `trailBlanks` byte ranges
     for the dual-attachment emit.

2. **Fprint** short-circuits to `w.Write(f.source)` when no struct was
   mutated. For dirty files, it splices the original source around
   synthesized struct bodies, runs `go/format.Source` for column
   alignment, and re-parses to validate before writing.

## Testing

- 25 unit tests covering decoration corner cases (nested structs,
  rejected layouts, comment classification, lead/trail attachment) and a
  table-driven `TestFprint` exercising 9 scenarios from identity to
  multi-blank-line reorder.
- Go-native fuzz targets `FuzzDecorateFileIdentity` and
  `FuzzDecorateFileReorder` seeded with hand-crafted edge cases and
  every `.go` / `.go.golden` file from the project's testdata.
- One regression seed committed under
  `testdata/fuzz/FuzzDecorateFileReorder/400aebff9b202cec` for the
  form-feed-in-import gofmt-corruption case the fuzzer found early on.
- 12 hours of fuzzing on the final code (6 h identity, 6 h reorder,
  ~621M execs combined) produced zero new failures.

## Performance vs `github.com/sirkon/dst`

Measured on Xeon E5-1630 v3 @ 3.70GHz, Go 1.26.3, Linux.

### Microbenchmark (`go test -bench=. -benchmem -count=10`)

Same parser-prepared `*ast.File` reused across all iterations. Parsing is
outside the timer. Benchstat with `n=10`; all deltas significant at
`p=0.000`.

| Operation | `sirkon/dst` | `internal/dstmin` | Δ wall-clock | Δ B/op | Δ allocs/op |
| --- | ---: | ---: | ---: | ---: | ---: |
| `DecorateFile` | 54.94 µs | 8.46 µs | **−84.60%** (6.5×) | −72.66% | −88.93% |
| `FprintIdentity` (clean-file pass-through) | 27.95 µs | 25.59 ns | **−99.91%** (~1100×) | −100% | −100% |
| `DecorateReorderPrint` (decorate + swap two fields + print) | 93.71 µs | 56.08 µs | **−40.15%** (1.67×) | −44.80% | −29.93% |
| geomean | 52.40 µs | 2.30 µs | **−95.61%** | | |

### Macro benchmark (end-to-end `betteralign -apply ./...`)

Synthetic corpus: 500 misaligned structs across 50 files in 10 packages
(248 KB total source). Each struct has lead-doc, trailing line comments,
a struct tag, and a floating blank-line-separated trailing comment block —
exercising every classifier rule. `hyperfine` with 3 warmup + 20 timed
runs per binary, alternating, fresh corpus copied for every run.

| Implementation | Wall-clock mean | Min | Max | User CPU mean |
| --- | ---: | ---: | ---: | ---: |
| `sirkon/dst` (main) | 811.9 ± 138.2 ms | 648.8 ms | 1313.8 ms | 236.9 ms |
| `internal/dstmin` (HEAD) | **665.6 ± 59.3 ms** | 559.9 ms | 759.3 ms | **110.5 ms** |
| ratio | **1.22× faster** | | | **2.14× less CPU** |

Both binaries produce **byte-identical output** on the corpus (verified
with `diff -r` between the two reordered trees).

### Binary size

| Implementation | Stripped binary | Δ |
| --- | ---: | ---: |
| `sirkon/dst` (main) | 7,696,546 B (7.34 MB) | — |
| `internal/dstmin` (HEAD) | **7,266,466 B (6.93 MB)** | **−5.6%** |

### Dependency footprint

| Implementation | Direct deps in `go.mod` |
| --- | ---: |
| `sirkon/dst` (main) | 6 |
| `internal/dstmin` (HEAD) | **5** (no `sirkon/dst`) |

## Why dstmin wins

- **The clean-file fast path is essentially free.** Most files in a real
  codebase do not have a misaligned struct. `Fprint` on a clean file is
  literally `w.Write(f.source)` — one syscall, zero allocations, ~25 ns.
  `sirkon/dst` always reconstructs the AST through its restorer and runs
  `go/printer` regardless, paying ~28 µs and 148 allocations on every
  no-op. For betteralign's typical workload (analyze many files,
  mutate few) this dominates the wall-clock savings.

- **Decoration is 6.5× cheaper.** `sirkon/dst` materialises one fragment
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
  improvement (~22%) is smaller than the per-operation improvement because
  process startup, AST parsing, type-checking, and atomic file writes are
  shared between the implementations. The User CPU time is a cleaner
  signal (2.14× less) — that is where dstmin pays back.

- **Allocation pressure is much lower, so GC pauses are rarer.** In the
  macro run, `sirkon/dst`'s upper tail reached 1.31s (1.6× its own mean)
  while dstmin's max was 759 ms (1.14× its mean). The tight dstmin
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
