# betteralign

[![GitHub license](https://img.shields.io/github/license/dkorunic/betteralign)](https://github.com/dkorunic/betteralign/blob/master/LICENSE)
[![GitHub release](https://img.shields.io/github/release/dkorunic/betteralign)](https://github.com/dkorunic/betteralign/releases/latest)
[![go-recipes](https://raw.githubusercontent.com/nikolaydubina/go-recipes/main/badge.svg?raw=true)](https://github.com/nikolaydubina/go-recipes)

![](gopher.png)

## About

**betteralign** is a tool to detect structs that would use less memory if their fields were sorted and optionally sort such fields.

This is a fork of an official Go [fieldalignment](https://cs.opensource.google/go/x/tools/+/master:go/analysis/passes/fieldalignment/fieldalignment.go) tool and vast majority of the alignment code has remained the same. There are however some notable changes:

- skips over generated files, either files with known "generated" suffix (`_generated.go`, `_gen.go`, `.gen.go`, `.pb.go`, `.pb.gw.go`) or due to package-level comment containing `Code generated by... DO NOT EDIT.` string,
- skips over test files (files with `_test.go` suffix),
- skips over structs marked with comment `betteralign:ignore`,
- doesn't lose comments (field comments, doc comments, floating comments or otherwise) but the comment position heuristics is still work in progress,
- does very reliable atomic file I/O with strong promise not to corrupt and/or lose contents upon rewrite ([not on Windows](https://github.com/golang/go/issues/22397#issuecomment-498856679) platform),
- has more thorough testing in regards to expected optimised vs golden results,
- integrates better with environments with restricted CPU and/or memory resources (Docker containers, K8s containers, LXC, LXD etc).

Retaining comments has been done with using [DST](https://github.com/dave/dst) (Decorated Syntax Tree) with decorating regular AST. Sadly when using DST we cannot use "fix" mode with SuggestedFixes, but we have to print whole DST (over the original file) to retain decorations. Exactly this reason is why `-fix` flag from analysis package doesn't work (we don't use SuggestedFixes) and why we cannot integrate with [golangci-lint](https://golangci-lint.run/).

In case you are wondering why DST and not AST, in general sense AST does not [associate comments to nodes](https://github.com/golang/go/issues/20744), but it holds fixed offsets. Original fieldalignment tool just erases all struct/field/floating comments due to this issue and while there is a CL with [a possible fix](https://go-review.googlesource.com/c/go/+/429639), it's still a work in progress as of this time.

Note, this is a single-pass analysis tool and sorting all structs optimally might require **more than one pass**.

Let us also mention original projects handling this task and without which this derivative work wouldn't exist in the first place:

- [fieldalignment](https://cs.opensource.google/go/x/tools/+/master:go/analysis/passes/fieldalignment/fieldalignment.go) from **Go Authors**
- [maligned](https://github.com/mdempsky/maligned) from **Matthew Dempsky**
- [structslop](https://github.com/orijtech/structslop) from **orijtech**

## Installation

There are two ways of installing betteralign:

### Manual:

Download your preferred flavor from [the releases](https://github.com/dkorunic/betteralign/releases) page and install manually, typically to `/usr/local/bin/betteralign`

### Using go install:

```shell
go install github.com/dkorunic/betteralign/cmd/betteralign@latest
```

## Usage

```shell
betteralign: find structs that would use less memory if their fields were sorted

Usage: betteralign [-flag] [package]

This analyzer find structs that can be rearranged to use less memory, and provides
a suggested edit with the most compact order.

Note that there are two different diagnostics reported. One checks struct size,
and the other reports "pointer bytes" used. Pointer bytes is how many bytes of the
object that the garbage collector has to potentially scan for pointers, for example:

	struct { uint32; string }

have 16 pointer bytes because the garbage collector has to scan up through the string's
inner pointer.

	struct { string; *uint32 }

has 24 pointer bytes because it has to scan further through the *uint32.

	struct { string; uint32 }

has 8 because it can stop immediately after the string pointer.

Be aware that the most compact order is not always the most efficient.
In rare cases it may cause two variables each updated by its own goroutine
to occupy the same CPU cache line, inducing a form of memory contention
known as "false sharing" that slows down both goroutines.

Unlike most analyzers, which report likely mistakes, the diagnostics
produced by betteralign very rarely indicate a significant problem,
so the analyzer is not included in typical suites such as vet or
gopls. Use this standalone command to run it on your code:

   $ go install github.com/dkorunic/betteralign/cmd/betteralign@latest
   $ betteralign [packages]



Flags:
  -V	print version and exit
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
  -v	no effect (deprecated)
```

To get all recommendations on your project:

```shell
betteralign ./...
```

To automatically fix your project files, while ignoring test and generated files:

```shell
betteralign -apply ./...
```

It is possible to include generated files and test files by enabling `generated_files` and/or `test_files` flags, or exclude certain files or directories with the `exclude_dirs` and/or `exclude_files` flags.

## Star history

[![Star History Chart](https://api.star-history.com/svg?repos=dkorunic/betteralign&type=Date)](https://star-history.com/#dkorunic/betteralign&Date)
