# betteralign

[![GitHub license](https://img.shields.io/github/license/dkorunic/betteralign)](https://github.com/dkorunic/betteralign/blob/main/LICENSE)
[![GitHub release](https://img.shields.io/github/release/dkorunic/betteralign)](https://github.com/dkorunic/betteralign/releases/latest)
[![go-recipes](https://raw.githubusercontent.com/nikolaydubina/go-recipes/main/badge.svg?raw=true)](https://github.com/nikolaydubina/go-recipes)

![](gopher.png)

## About

**betteralign** reports Go structs whose field order wastes memory or GC pointer bytes, and rewrites them while preserving comments. It is a fork of, and drop-in replacement for, [fieldalignment](https://cs.opensource.google/go/x/tools/+/master:go/analysis/passes/fieldalignment/fieldalignment.go).

Differences from `fieldalignment`:

- **Comments are preserved** on rewrite (doc, line and floating comments). Go's AST [does not attach comments to nodes](https://github.com/golang/go/issues/20744), so `fieldalignment` drops them.
- **Layout contracts are respected**: a struct whose field order the code depends on (positional literals, `unsafe`, atomics, codecs, assembly, conversions, …) is reported but never rewritten. See [What is never rewritten](#what-is-never-rewritten).
- **One run reaches a fixed point**: nested structs are measured at the layout the same run gives them, so a second `-fix`/`-apply` changes nothing.
- **Stable sort**: equal-ranked fields keep their relative order (upstream's `sort.Slice` does not).
- **No crash on generic structs**: upstream's `ptrdata` panics (`"impossible"`) on a type-parameter field.
- **Generated files** (`_generated.go`, `_gen.go`, `.gen.go`, `.pb.go`, `.pb.gw.go`, or a `Code generated … DO NOT EDIT.` header) and **test files** are skipped by default.
- `// betteralign:ignore` and opt-in (`-opt_in` with `// betteralign:check`) annotations.
- Diagnostics lead with the saving: `8 bytes saved: struct of size 24 could be 16`.
- Atomic file writes ([not on Windows](https://github.com/golang/go/issues/22397#issuecomment-498856679)); per-pass memoisation of size/alignment/ptrdata; `GOMEMLIMIT` set to 90% of the cgroup or system memory.

It builds on [fieldalignment](https://cs.opensource.google/go/x/tools/+/master:go/analysis/passes/fieldalignment/fieldalignment.go) (the Go Authors), [maligned](https://github.com/mdempsky/maligned) (Matthew Dempsky) and [structslop](https://github.com/orijtech/structslop) (orijtech).

## How it works

Fields are sorted stably by:

1. zero-sized types first,
2. higher alignment first,
3. pointer-bearing types before pointer-free ones,
4. among pointer-bearing types, fewer trailing non-pointer bytes first,
5. larger size first.

A multi-name field (`A, B int`) moves as one declaration.

**Size.** `struct { Flag bool; ID int64; Count int32 }` is 24 bytes on 64-bit (7 bytes of padding before `ID`, 4 after `Count`); `ID, Count, Flag` is 16.

**Pointer bytes.** `ptrdata` is the prefix of a value that may hold pointers. `struct { Zone string; IP []byte }` is 40 bytes either way, but `ptrdata` is 24 with `Zone` first and 32 with `IP` first (`string` has 8 trailing non-pointer bytes, `[]byte` 16). Before Go 1.26 `ptrdata` was also the bytes the GC scanned per value; under the Green Tea GC (default in Go 1.26) marking is span-based, and a smaller `ptrdata` lowers the scan-work estimate used for pacing. The diagnostic is worded for the analyzed package's Go version (the newer of its `go.mod` `go` directive and a file's `//go:build go1.x`).

**Nested structs.** A struct held by value is measured at the layout the run leaves it in, so the outer struct is laid out around the reordered inner one. Diagnostic figures use the same projected layout. Convergence assumes gofmt-clean input.

**Rewriting.** `internal/dstmin` decorates the parsed file with per-field byte spans, splices the reordered field bodies into the original source and runs `go/format.Source`. Each reordered struct body is cut from that output and attached as a `SuggestedFix` (`Rearrange fields`), already gofmt-aligned, so any driver that applies analysis fixes gets the same result as `-apply`. Shapes the splice cannot handle safely (single-line structs, fields sharing a line, a block comment crossing a brace, …) are reported without a fix. See [`internal/dstmin/README.md`](internal/dstmin/README.md).

## Installation

Download a binary from the [releases page](https://github.com/dkorunic/betteralign/releases), or:

```shell
go install github.com/dkorunic/betteralign/cmd/betteralign@latest
```

## Usage

```shell
betteralign ./...                  # report; exit status 3 if anything is reported
betteralign -apply ./...           # rewrite files (atomic writes)
betteralign -fix ./...             # rewrite via the analysis driver's suggested fixes
betteralign -apply -diff ./...     # preview: gofmt-style unified diff, nothing written
betteralign -fix -diff ./...       # preview in the driver's diff format
```

```
Usage: betteralign [-flag] [package]

Flags:
  -V    print version and exit
  -all
        no effect (deprecated)
  -apply
        apply suggested fixes
  -c int
        display offending line with this many lines of context (default -1)
  -cpuprofile string
        write CPU profile to this file
  -debug string
        debug flags, any subset of "fpstv"
  -diff
        with -fix, don't update the files, but print a unified diff
  -exclude_dirs value
        exclude directories matching a pattern
  -exclude_files value
        exclude files matching a pattern
  -fix
        apply all suggested fixes
  -flags
        print analyzer flags in JSON
  -generated_files
        also check and fix generated files
  -json
        emit JSON output
  -memprofile string
        write memory profile to this file
  -opt_in
        opt-in mode on per-struct basis with 'betteralign:check' in comment
  -source
        no effect (deprecated)
  -tags string
        no effect (deprecated)
  -test
        indicates whether test files should be analyzed, too (default true)
  -test_files
        also check and fix test files
  -trace string
        write trace log to this file
  -v    no effect (deprecated)
```

### `-apply`, `-fix` and `-diff`

- `-apply` writes each rewritten file atomically. With `-diff` it writes nothing and prints `diff X.orig X` / `--- X.orig` / `+++ X` hunks, `X` slash-separated and relative to the working directory when under it, so `git apply -p0` or `patch -p0` applies them; with `-json` the diff goes to stderr. Exit status is 3 when there are diagnostics.
- `-fix` lets the driver apply the suggested fixes (merging duplicates from a package and its test variant). `-fix -diff` prints the driver's diff (absolute paths, `(old)`/`(new)` headers) and exits 0. `-fix` output is never JSON. `-fix` takes precedence over `-apply`.
- With `-apply` (and no `-diff`) no suggested fixes are attached, so a host that applies collected fixes (e.g. golangci-lint `--fix`) must not also set `-apply`.
- The driver refuses fixes in generated files, so with `-generated_files`, `-fix` rewrites through the `-apply` path.

### Under `go vet`

`go vet -vettool=$(which betteralign)` works for reporting and for `-fix` (`-fix -diff` to preview; go vet rejects `-diff` without `-fix`). `-apply` is refused with an error: go vet runs each package in its own process, and a dependency rewritten before its importer is analyzed breaks cross-package conversions. `-fix -generated_files` is plain `-fix` there (noted on stderr). Vet the whole module: vetting only an importer (`./user`) reorders its side of a conversion while the partner stays. For a `go.work` workspace, vet one module at a time, or run betteralign itself from the root (`betteralign -fix ./m1/... ./m2/...`).

### Selecting files and structs

- `-generated_files`, `-test_files`: also analyze and fix generated or test files.
- `-exclude_dirs`: a separator-free pattern (`vendor`, `test*`) matches a directory of that name at any depth; a pattern with a separator (`a/internal`) matches that path relative to the working directory.
- `-exclude_files`: matches the file's path relative to the working directory (`p.go` does not exclude `p/p.go`; use `p/p.go` or `*/p.go`).
- A malformed pattern is a flag error (exit 2).
- `// betteralign:ignore` inside a struct's opening brace skips that struct.
- `-opt_in` checks only structs whose `type` declaration (or enclosing `type ( … )` group) carries `// betteralign:check`.

Only named struct types (`type T struct{…}`, `type A = struct{…}`) are analyzed; anonymous structs are not.

### Runtime tuning

Loading and type-checking produce mostly short-lived garbage. `GOGC=off betteralign ./...` was about 15% faster on a large warm-cache run (`GOGC=400` about 13%). `GOGC=off` lets the heap grow until the `GOMEMLIMIT` backstop; on tight memory limits prefer `GOGC=400`.

## What is never rewritten

**Not reported at all:**

- a struct with a `structs.HostLayout` field, or a blank `_` field of non-zero size (deliberate padding);
- a struct with an `asn1:"…"` or `xml:"…"` tag or an `encoding/asn1` field type, on it or on a struct it holds by value, or with an `ssh`/`sshtype` tag: their encoders follow field order and usually live in another package;
- a generic struct holding a type parameter by value (`B T`, `B [4]T`): its layout varies per instantiation (`*T`, `[]T`, `map[K]T` are fine).

**Reported with `reorder skipped: …` and no fix**, because the code depends on the field order:

- a positional composite literal (`T{1, 2}`) anywhere in the package, including files the current build excludes (other `GOOS`/`GOARCH`, unset tags), in-package tests and the external `package p_test`;
- `unsafe.Offsetof(x.f)`;
- a 64-bit `sync/atomic` function on a field (`atomic.AddInt64(&s.n, 1)` needs 8-byte alignment on 32-bit platforms), and every struct holding such a struct by value, also when the access is in another package (`os.file` holding `internal/poll.FD`);
- a conversion to or from another struct type, or identity with an unnamed struct type (literal, alias, generic instance, another package's `func F(x struct{…})`). A partner in another package of the same run (module, `go.work` workspace, std) is predicted instead, and both sides reorder identically;
- `unsafe.Pointer` conversions (also `*[]T`, `**T`), and pointer arithmetic from a field address (`uintptr(unsafe.Pointer(&p.f)) + n`, `unsafe.Add`); a plain `unsafe.Pointer(&x.f)` depends only on `f` and pins nothing;
- an argument of `encoding/binary`, `encoding/asn1` or `x/crypto/ssh`, also through one local variable or one forwarding function;
- `reflect.TypeOf(x).Field(c)` / `ValueOf(x).Field(c)` with a constant index;
- parameters, results and receivers of body-less (assembly) functions, of Go functions and variables referenced from the package's `.s` files, and of functions a two-argument `//go:linkname` pushes into another package (unless that package can name the type itself: an exported type, pushed into a package outside the pusher's imports);
- standard-library structs the Go toolchain lays out itself: `runtime`, `internal/abi`, `internal/runtime/maps`, `reflect`, `runtime/metrics`, `testing.Internal*`.

Structs held by value inside a punned, encoded or assembly-accessed struct are pinned with it.

**Files never rewritten:** a file containing `\r` (normalise to LF), one with a `//line` directive, cgo files (`import "C"`; analyzed only with `-generated_files`, since the driver sees cgo's generated translation), and every file of a package whose directory holds an in-package `_test.go` that does not parse (noted on stderr).

## Limitations

- **Positional literals in importing packages are not detected.** A positional literal of an exported struct in another package is invisible to the pass that rewrites the struct: the build breaks, or values are silently mis-assigned if the types still fit. Run `go build ./...` and the tests after rewriting, or convert such literals to keyed form first.
- **Field order stays observable.** `encoding/json` key order, `fmt`'s `%v`/`%+v`, reflection-based dumpers (`go/ast.Print`), and tests asserting `unsafe.Sizeof` change with a reorder. Third-party reflection codecs, untagged structs reaching `encoding/xml`, `reflect.FieldByIndex`/`NumField` loops, values reaching an encoder through `interface{}`, cgo mirrors without `structs.HostLayout`, syscall and wire-format structs, and `Offsetof` in another package are not recognised. Mark such structs `// betteralign:ignore` or use `-opt_in`.
- **Generated copies are not regenerated.** Generated files are skipped, so a file generated from a rewritten source keeps the old order (in std, `go/types` is generated from `cmd/compile/internal/types2`). Re-run the generators (`go generate ./...`, or the project's equivalent) after rewriting.
- **Build-excluded files are scanned syntactically only.** Positional literals, assembly stubs, linkname pushes and assembly references there are found; type-based contracts (`Offsetof`, conversions, `unsafe.Pointer`, codecs) are not. Run under each `GOOS`/`GOARCH` and tag set you ship.
- **In-package test literals need the test variant.** The base pass leaves the rewrite to the `[p.test]` variant when the directory holds an in-package test file. Under `-test=false` no variant runs and the base pass rewrites; keep test loading enabled when applying. File arguments (`betteralign -apply p/p.go`) load no variant, so such a rewrite stays deferred (noted on stderr); run on the package instead. A struct pinned only from a test may be reported twice: plain by the base pass, with the caveat by the variant.
- **Cross-package predictions can miss.** The local side of a conversion follows a partner predicted to move. It breaks the build when the partner stays because of a pin in its own package (literal, `Offsetof`, atomics, codecs), or because an ill-typed in-package test file makes the driver skip its test variant, and when a library module is rewritten without the modules that convert to its structs.
- **Cross-package atomic holders are matched by field name.** A same-named field of another struct over-pins; under `-trimpath`, a `replace`d or other `go.work` module is not resolved and pins nothing.
- **Projected figures can be off.** A struct whose field names are all exported must sort identically in every package, so an inner struct pinned in its own package is measured as reordered although it stays (in std, `compress/gzip.Header` goes from 72 to 80 pointer bytes, since `time.Time` is pinned in `time`). A struct with an unexported field is measured against its package's pins; if the inner pin is only in an in-package test, the base pass keeps reporting the outer struct without a fix. The code stays correct; `-opt_in` measures every inner struct as declared.
- **A leading UTF-8 BOM is dropped** by `-apply` and `-fix` (gofmt removes it).
- **False sharing.** The most compact order can put fields written by different goroutines on one cache line.

Rewriting Go 1.27.3's `std` and `cmd`: everything builds for 21 `GOOS`/`GOARCH` targets, a second run changes nothing, and `go test -short std cmd` (amd64; `std` also on 386) fails only where tests compare field-order-dependent output: `go/ast` `ExamplePrint`, `net/http` `TestTransportEventTrace` (`%+v`), `cmd/compile/internal/logopt` and a `cmd/go` script (JSON key order), `types2` `TestSizeof`, and `go/types` `TestGenerate` (generated copies).

## Star history

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://star-history.dera.page/svg?repos=dkorunic/betteralign&type=date&theme=dark" />
  <source media="(prefers-color-scheme: light)" srcset="https://star-history.dera.page/svg?repos=dkorunic/betteralign&type=date" />
  <img alt="Star History Chart" src="https://star-history.dera.page/svg?repos=dkorunic/betteralign&type=date" />
</picture>
