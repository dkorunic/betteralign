// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

// Package dstmin is an internal replacement for the betteralign-relevant
// subset of github.com/sirkon/dst and github.com/sirkon/dst/decorator. It
// supports exactly one mutation — reordering the field list of an
// *ast.StructType — and reprints the file with comments preserved by
// byte-splicing unmodified source spans around synthesized struct bodies.
//
// API parity: this package intentionally mirrors the names betteralign.go
// referenced in sirkon/dst (Decorator, NewDecorator, DecorateFile, Fprint,
// File, StructType, FieldList, Field) so the caller diff is minimal.
package dstmin

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
)

// Sentinel errors classified via errors.Is.
var (
	ErrSourceRead = errors.New("dstmin: unable to read source")
	ErrFormat     = errors.New("dstmin: formatted output rejected by gofmt")
)

// Decorator owns decorated trees; Dst.Nodes maps AST struct types to DST wrappers.
type Decorator struct {
	Fset *token.FileSet
	Dst  struct {
		Nodes map[ast.Node]any
	}
}

// NewDecorator binds a Decorator to fset for the decoration session. Reuse
// across DecorateFile calls is intentional — each appends into Dst.Nodes so the
// caller can resolve any *ast.StructType to its DST wrapper. fset must be the
// FileSet f was parsed against, or position lookups return garbage. Not
// concurrency-safe.
func NewDecorator(fset *token.FileSet) *Decorator {
	d := &Decorator{Fset: fset}
	d.Dst.Nodes = make(map[ast.Node]any, 64)
	return d
}

// File is the tree root for one decorated *ast.File.
type File struct {
	ast    *ast.File
	source []byte
	tf     *token.File
	// Every decorated *StructType, in source order.
	structs []*StructType
}

// StructType wraps *ast.StructType. The only mutable surface is Fields.List.
type StructType struct {
	ast    *ast.StructType
	Fields *FieldList
	// Per-line indent for fields in this struct body.
	indent string
	// Snapshot of Fields.List at decoration time; used to detect mutation.
	origList []*Field
	// Byte range of the struct body, between the { and } lines.
	bodyStart, bodyEnd int
}

// FieldList wraps *ast.FieldList.
type FieldList struct {
	List []*Field
}

// nestedRange is a [lo, hi) token.Pos interval of a nested struct body.
type nestedRange struct{ lo, hi token.Pos }

// Field wraps *ast.Field.
type Field struct {
	ast   *ast.Field
	Names []struct{}
	// Lead-doc comment lines, in source order.
	lead []string
	// Inter-field blanks attach to both neighbours; gofmt coalesces duplicates.
	leadBlanksStart, leadBlanksEnd   int
	trailBlanksStart, trailBlanksEnd int
	// First lead-doc line; equals bodyStart when no lead-doc attaches.
	leadDocStart int
	// Byte range of the field's declaration line(s), incl. trailing newline.
	bodyStart, bodyEnd int
	// Byte range of trailing floating block from Rule 4. Zero-width when none.
	trailStart, trailEnd int
}

// DecorateFile is the production entry point: it reads f's source from disk
// (path via dec.Fset, so f must belong to it) and hands off to DecorateFileSrc.
// Splicing needs the verbatim bytes — the AST alone drops positional whitespace
// and unattached comments. Returns ErrSourceRead on failure. The returned *File
// aliases the source bytes; don't mutate them.
func (dec *Decorator) DecorateFile(f *ast.File) (*File, error) {
	tf := dec.Fset.File(f.Pos())
	if tf == nil {
		// Defensive: no *token.File means f isn't in dec.Fset, so there's
		// nothing to read or splice against.
		return nil, fmt.Errorf("%w: position not in FileSet", ErrSourceRead)
	}
	src, err := readSourceOnce(tf.Name())
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrSourceRead, tf.Name(), err)
	}
	return dec.DecorateFileSrc(f, src), nil
}

// sourceCache memoises each file's first-read bytes by path, process-wide.
var sourceCache sync.Map // map[string][]byte

// readSourceOnce pins the bytes the splice runs against to the file as first
// seen, so they still match the AST offsets after -fix rewrites it (issue #36).
//
// Under `betteralign -fix ./...` a non-test file loads in several package
// variants (base + [p.test]), so independent passes decorate and rewrite the
// same path. A fresh os.ReadFile would hand a later pass an earlier pass's
// rewrite, and its stale offsets would duplicate/drop fields. Safe because a
// pass only rewrites a file it first decorated through here, so every read —
// including concurrent misses — precedes the first write.
func readSourceOnce(name string) ([]byte, error) {
	if v, ok := sourceCache.Load(name); ok {
		return v.([]byte), nil
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	actual, _ := sourceCache.LoadOrStore(name, b)
	return actual.([]byte), nil
}

// DecorateFileSrc is the in-memory entry point for tests and fuzz harnesses
// holding the bytes. f must be parsed from src via parser.ParseComments through
// dec.Fset, else positions won't line up and splicing emits garbage. The
// returned *File aliases src; don't mutate it.
//
// Four passes: (1) a preorder walk filtering structs through buildStruct + the
// comment guards and recording nested-struct ranges; (2) registering survivors
// in dec.Dst.Nodes; (3) per-struct comment routing via decorateComments; (4)
// per-field lead/trail blank spans for Fprint's dual-emit. Never errors —
// unsplicable shapes are dropped from File.structs.
func (dec *Decorator) DecorateFileSrc(f *ast.File, src []byte) *File {
	tf := dec.Fset.File(f.Pos())
	df := &File{ast: f, source: src, tf: tf}

	// f isn't in dec.Fset: no positions to splice against. Return it undecorated
	// so Fprint emits src verbatim rather than nil-deref offsetOf. DecorateFile
	// guards this; direct callers (tests, fuzz harnesses) don't.
	if tf == nil {
		return df
	}

	// Stack holds enclosing structs; decoratedSet filters which ancestors record
	// nested ranges for the routing pass.
	nestedRanges := make(map[*ast.StructType][]nestedRange)
	decoratedSet := make(map[*ast.StructType]struct{})
	var stack []*ast.StructType
	ast.Inspect(f, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		for len(stack) > 0 {
			top := stack[len(stack)-1]
			if st.Fields.Opening > top.Fields.Opening && st.Fields.Closing < top.Fields.Closing {
				break
			}
			stack = stack[:len(stack)-1]
		}
		for _, outer := range stack {
			if _, decorated := decoratedSet[outer]; !decorated {
				continue
			}
			nestedRanges[outer] = append(nestedRanges[outer], nestedRange{lo: st.Fields.Opening, hi: st.Fields.Closing})
		}
		if dstSt, ok := dec.buildStruct(df, st); ok {
			// Narrow once, shared by all guards, to avoid per-struct rescans.
			run := commentRun(f.Comments, st.Fields.Opening, st.Fields.Closing)
			if !hasUnsafeBlockComment(dec.Fset, st, run) &&
				!hasBuildConstraintComment(st, run) &&
				!hasLineDirectiveComment(st, run) {
				dec.Dst.Nodes[st] = dstSt
				df.structs = append(df.structs, dstSt)
				decoratedSet[st] = struct{}{}
			}
		}
		stack = append(stack, st)
		return true
	})

	// Pass 3: route each struct's comments over its narrowed run.
	for _, dstSt := range df.structs {
		run := commentRun(f.Comments, dstSt.ast.Fields.Opening, dstSt.ast.Fields.Closing)
		dec.decorateComments(df, dstSt.ast, dstSt, run, nestedRanges[dstSt.ast])
	}

	// Pass 4: per-field leadBlanks and trailBlanks for dual-attachment emit.
	for _, dstSt := range df.structs {
		for i, fld := range dstSt.Fields.List {
			if i == 0 {
				fld.leadBlanksStart = dstSt.bodyStart
			} else {
				prev := dstSt.Fields.List[i-1]
				if prev.trailEnd > prev.trailStart {
					fld.leadBlanksStart = prev.trailEnd
				} else {
					fld.leadBlanksStart = prev.bodyEnd
				}
			}
			fld.leadBlanksEnd = fld.leadDocStart
			if fld.trailEnd > fld.trailStart {
				fld.trailBlanksStart = fld.trailEnd
			} else {
				fld.trailBlanksStart = fld.bodyEnd
			}
			if i+1 < len(dstSt.Fields.List) {
				fld.trailBlanksEnd = dstSt.Fields.List[i+1].leadDocStart
			} else {
				fld.trailBlanksEnd = dstSt.bodyEnd
			}
		}
	}

	return df
}

// buildStruct builds the DST wrappers and per-field byte spans for one struct,
// or returns (nil, false) for source shapes splicing can't handle. Comment
// state (lead/trail) is filled later by decorateComments. This is the single
// source of truth for structural rejections; each carries its bug tag so
// regressions land near the matching test:
//
//   - single-line struct: span arithmetic inverts.
//   - first field on the { line: field span overlaps the header.
//   - last field on the } line (BUG-31): field span overflows the footer.
//   - fields sharing a source line (BUG-34/38): no clean per-field byte cut.
//   - \r (BUG-35) or \f (BUG-37) in the body: break position math / gofmt.
//   - *//* or */// in the body: gofmt-impossible (BUG-40).
//
// Comment-induced shapes are caught separately by the has*Comment guards.
func (dec *Decorator) buildStruct(df *File, st *ast.StructType) (*StructType, bool) {
	dstSt := &StructType{ast: st}
	fl := &FieldList{}
	fl.List = make([]*Field, len(st.Fields.List))

	// Body span: between the { and } lines.
	lbraceLineEnd := df.lineEndOffset(df.offsetOf(st.Fields.Opening))
	rbraceLineStart := df.lineStartOffset(df.offsetOf(st.Fields.Closing))
	if lbraceLineEnd > rbraceLineStart {
		// Single-line { ... }: spans would invert.
		return nil, false
	}
	if len(st.Fields.List) > 0 && df.offsetOf(st.Fields.List[0].Pos()) < lbraceLineEnd {
		// First field on the { line: spans would overlap the struct header.
		return nil, false
	}
	// BUG-31: last field on } line — bodyEnd would overflow the struct footer.
	if n := len(st.Fields.List); n > 0 && df.offsetOf(st.Fields.List[n-1].End()) >= rbraceLineStart {
		return nil, false
	}
	// BUG-34/38: consecutive fields must not share a source line.
	for i := 1; i < len(st.Fields.List); i++ {
		prevLast := df.tf.Line(st.Fields.List[i-1].End() - 1)
		curFirst := df.tf.Line(st.Fields.List[i].Pos())
		if prevLast >= curFirst {
			return nil, false
		}
	}
	// BUG-35/37: \r breaks comment position math; \f makes gofmt collapse trailing newlines.
	structStart := df.offsetOf(st.Fields.Opening)
	structEnd := df.offsetOf(st.Fields.Closing) + 1
	// Reject in one sweep: \r/\f break splice math (BUG-35/37); *//* and *///
	// are gofmt-impossible (BUG-40).
	body := df.source[structStart:structEnd]
	for i := range body {
		switch body[i] {
		case '\r', '\f':
			return nil, false
		case '*':
			if i+3 < len(body) && body[i+1] == '/' && body[i+2] == '/' &&
				(body[i+3] == '*' || body[i+3] == '/') {
				return nil, false
			}
		}
	}
	dstSt.bodyStart = lbraceLineEnd
	dstSt.bodyEnd = rbraceLineStart

	// Indent: leading whitespace of the first non-blank body line.
	dstSt.indent = "\t"
	for off := lbraceLineEnd; off < rbraceLineStart; {
		lineEnd := df.lineEndOffset(off)
		i := off
		for i < lineEnd && (df.source[i] == '\t' || df.source[i] == ' ') {
			i++
		}
		if i > off && i < lineEnd && df.source[i] != '\n' {
			dstSt.indent = string(df.source[off:i])
			break
		}
		off = lineEnd
	}

	for i, af := range st.Fields.List {
		field := &Field{ast: af}
		field.Names = make([]struct{}, len(af.Names))
		field.bodyStart = df.lineStartOffset(df.offsetOf(af.Pos()))
		field.bodyEnd = df.lineEndOffset(df.offsetOf(af.End()))
		field.leadDocStart = field.bodyStart
		field.trailStart = field.bodyEnd
		field.trailEnd = field.bodyEnd
		fl.List[i] = field
	}
	dstSt.Fields = fl
	dstSt.origList = append([]*Field(nil), fl.List...)
	return dstSt, true
}

// decorateComments routes each in-body comment group to its owning field's
// lead or trail slot so a reorder carries comments with their field. Comments
// that never move with a field (on the { line, floating above the first
// field, or in an empty struct) are skipped: their bytes survive through the
// verbatim splice and blank-span emits instead. fileComments is st's narrowed
// run (see commentRun); nested lists descendant struct bodies, whose comments
// are routed by the inner struct's own call. These two narrowings keep the
// pass off the O(structs × comments) quadratic.
//
// Rules apply in order, first match wins (the numbering predates implementation
// order and is kept for the BUG-36/39 references on Rules 4/2):
//
//	Rule 1: comment on the { line                 -> skip (header's verbatim span)
//	Rule 3: comment starts on a field's last line -> field's trail
//	Rule 2: comment ends on the line before a field -> field's lead-doc
//	Rule 4: floating block between fields          -> preceding field's trail,
//	        or skip when no field precedes (first field's leadBlanks covers it)
//
// Mutates dstSt.Fields.List in place.
func (dec *Decorator) decorateComments(df *File, st *ast.StructType, dstSt *StructType, fileComments []*ast.CommentGroup, nested []nestedRange) {
	if len(dstSt.Fields.List) == 0 {
		// No fields: nothing can be reordered, so an empty struct is never
		// dirty and its body comments always survive via the verbatim path.
		return
	}

	// tf.Line is O(1) and alloc-free; hoist once.
	tf := dec.Fset.File(st.Fields.Opening)
	lbraceLine := tf.Line(st.Fields.Opening)

	// Per-field source-line range.
	type fline struct{ first, last int }
	flines := make([]fline, len(dstSt.Fields.List))
	for i, fld := range dstSt.Fields.List {
		flines[i].first = tf.Line(fld.ast.Pos())
		flines[i].last = tf.Line(fld.ast.End())
	}

	for _, cg := range fileComments {
		if !insideStructBody(st, cg) {
			continue
		}
		// Skip comments inside nested structs; the inner call routes them.
		skip := false
		for _, nr := range nested {
			if cg.Pos() > nr.lo && cg.End() < nr.hi {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		cgStartLine := tf.Line(cg.Pos())
		cgEndLine := tf.Line(cg.End())

		// Rule 1: comment on the { line — the { line sits outside the body
		// span [bodyStart, bodyEnd), so the verbatim splice keeps it in place.
		if cgStartLine == lbraceLine {
			continue
		}

		// Rule 3: comment on field's last line. Single-line is in bodyEnd; multi-line extends trail.
		emittedAsTrailingLine := false
		for fi, fl := range flines {
			if cgStartLine != fl.last {
				continue
			}
			emittedAsTrailingLine = true
			if cgEndLine > fl.last {
				cgEndOff := df.lineEndOffset(df.offsetOf(cg.End()))
				if dstSt.Fields.List[fi].trailStart == dstSt.Fields.List[fi].trailEnd {
					dstSt.Fields.List[fi].trailStart = dstSt.Fields.List[fi].bodyEnd
				}
				if cgEndOff > dstSt.Fields.List[fi].trailEnd {
					dstSt.Fields.List[fi].trailEnd = cgEndOff
				}
			}
			break
		}
		if emittedAsTrailingLine {
			continue
		}

		// Rule 2: comment ends on the line just before a field — lead-doc.
		assigned := false
		for i, fl := range flines {
			if cgEndLine == fl.first-1 {
				// BUG-39: cg overlaps previous field's trail — extend trail, don't split.
				if i > 0 && df.offsetOf(cg.Pos()) < dstSt.Fields.List[i-1].trailEnd {
					cgEndOff := df.lineEndOffset(df.offsetOf(cg.End() - 1))
					if cgEndOff > dstSt.Fields.List[i-1].trailEnd {
						dstSt.Fields.List[i-1].trailEnd = cgEndOff
					}
					assigned = true
					break
				}
				for _, c := range cg.List {
					dstSt.Fields.List[i].lead = append(dstSt.Fields.List[i].lead, c.Text)
				}
				// Earliest lead-doc position; lead-blanks pass needs it.
				cgStart := df.lineStartOffset(df.offsetOf(cg.Pos()))
				if cgStart < dstSt.Fields.List[i].leadDocStart {
					dstSt.Fields.List[i].leadDocStart = cgStart
				}
				assigned = true
				break
			}
		}
		if assigned {
			continue
		}

		// Rule 4: floating block — attach to the nearest preceding field, else Opening.
		attachIdx := -1
		for i, fl := range flines {
			if fl.last < cgStartLine {
				attachIdx = i
			}
		}
		if attachIdx == -1 {
			// Floating above the first field: its bytes live in
			// [bodyStart, leadDocStart), which the first field's leadBlanks
			// span re-emits wherever that field lands.
			continue
		}
		// BUG-36: comment group overlaps next field's body — skip to avoid double-write on reorder.
		if attachIdx+1 < len(flines) {
			if df.offsetOf(cg.End()) > dstSt.Fields.List[attachIdx+1].bodyStart {
				continue
			}
		}
		// Extend trail to cover this comment and any blank lines that follow.
		cgEndOff := df.lineEndOffset(df.offsetOf(cg.End()))
		nextFieldStart := len(df.source)
		if attachIdx+1 < len(flines) {
			nextFieldStart = dstSt.Fields.List[attachIdx+1].bodyStart
		}
		cgEndOff = df.consumeBlankLines(cgEndOff, nextFieldStart)
		if dstSt.Fields.List[attachIdx].trailStart == dstSt.Fields.List[attachIdx].trailEnd {
			// First trail block: anchor at the field's bodyEnd.
			dstSt.Fields.List[attachIdx].trailStart = dstSt.Fields.List[attachIdx].bodyEnd
		}
		if cgEndOff > dstSt.Fields.List[attachIdx].trailEnd {
			dstSt.Fields.List[attachIdx].trailEnd = cgEndOff
		}
	}
}

// insideStructBody is the body-membership predicate. Both bounds are exclusive
// on purpose: a comment touching the { or } belongs to the surrounding source
// span, where the verbatim splice copies it — claiming it here would double it
// against spliceDirty's cursor advance past bodyEnd.
func insideStructBody(st *ast.StructType, cg *ast.CommentGroup) bool {
	return cg.Pos() > st.Fields.Opening && cg.End() < st.Fields.Closing
}

// commentRun returns the contiguous sub-slice of comments overlapping the
// (opening, closing) body interval, replacing the per-struct full rescan of
// f.Comments that made decoration O(structs × comments). Sound because
// f.Comments is Pos-sorted and groups never overlap, so two binary searches
// isolate the run in O(log C). Nested-struct comments stay in the outer run;
// decorateComments skips them via nested ranges. Aliases comments; read-only.
func commentRun(comments []*ast.CommentGroup, opening, closing token.Pos) []*ast.CommentGroup {
	n := len(comments)
	lo := sort.Search(n, func(i int) bool { return comments[i].End() > opening })
	hi := sort.Search(n, func(i int) bool { return comments[i].Pos() >= closing })
	if lo >= hi {
		return nil
	}
	return comments[lo:hi]
}

// hasUnsafeBlockComment rejects structs whose body holds a block-comment shape
// splicing can't keep consistent. The cases share one comment scan:
//
//   - BUG-32: /* */ crossing the { line — halves land in different splice
//     partitions and reorder splits the comment.
//   - BUG-44: closing */ on the } line — its trail swallows the brace, closing
//     the struct early (covers the multi-line and single-line forms).
//   - BUG-33: closing */ on a field's first line — header and field bytes share
//     a line, no clean per-field cut.
//   - BUG-41: a multi-comment group containing any block comment — routing
//     moves the group as a unit and can't keep the bytes consistent.
//
// fieldFirstLines is built lazily so the no-block-comment case stays alloc-free.
// Assumes {/} are on distinct lines (buildStruct already ruled out single-line).
func hasUnsafeBlockComment(fset *token.FileSet, st *ast.StructType, comments []*ast.CommentGroup) bool {
	tf := fset.File(st.Fields.Opening)
	lbraceLine := tf.Line(st.Fields.Opening)
	rbraceLine := tf.Line(st.Fields.Closing)
	var fieldFirstLines map[int]bool
	for _, cg := range comments {
		if cg.End() <= st.Fields.Opening || cg.Pos() >= st.Fields.Closing {
			continue
		}
		// BUG-41: multi-comment group with any block comment — routing can't keep bytes consistent.
		if len(cg.List) > 1 {
			for _, c := range cg.List {
				if strings.HasPrefix(c.Text, "/*") {
					return true
				}
			}
		}
		for _, c := range cg.List {
			if !strings.HasPrefix(c.Text, "/*") {
				continue
			}
			cgStart := tf.Line(c.Pos())
			cgEnd := tf.Line(c.End() - 1)
			// BUG-44: comment closing on the } line; its trail swallows } on reorder.
			// Checked before the single-line skip, which would otherwise let it pass.
			if cgEnd == rbraceLine {
				return true
			}
			if cgStart == cgEnd {
				continue
			}
			// BUG-32: block comment crossing the { line — splits across splice partitions.
			if cgStart <= lbraceLine && cgEnd > lbraceLine {
				return true
			}
			if fieldFirstLines == nil {
				fieldFirstLines = make(map[int]bool, len(st.Fields.List))
				for _, fld := range st.Fields.List {
					fieldFirstLines[tf.Line(fld.Pos())] = true
				}
			}
			if fieldFirstLines[cgEnd] {
				return true
			}
		}
	}
	return false
}

// hasLineDirectiveComment rejects structs whose body carries a //line or /*line
// directive. BUG-43: the parser remaps tf.Line() to the logical line after such
// a directive, but every other guard reads tf.Line() expecting monotonic growth
// with byte offset, so physical and logical line numbers silently mix. The
// prefix match mirrors the scanner: literal "//line " / "/*line " with one
// space after "line".
func hasLineDirectiveComment(st *ast.StructType, comments []*ast.CommentGroup) bool {
	for _, cg := range comments {
		if cg.End() <= st.Fields.Opening || cg.Pos() >= st.Fields.Closing {
			continue
		}
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//line ") || strings.HasPrefix(c.Text, "/*line ") {
				return true
			}
		}
	}
	return false
}

// hasBuildConstraintComment rejects structs whose body carries a build
// constraint (//go:build or //+build). BUG-42: gofmt hoists these to the file
// header even mid-struct, dropping the line break after the carrier field so the
// field count diverges after a reorder. Detection delegates to
// go/build/constraint so it stays in lock-step with gofmt's recognition.
func hasBuildConstraintComment(st *ast.StructType, comments []*ast.CommentGroup) bool {
	for _, cg := range comments {
		if cg.End() <= st.Fields.Opening || cg.Pos() >= st.Fields.Closing {
			continue
		}
		for _, c := range cg.List {
			if constraint.IsGoBuild(c.Text) || constraint.IsPlusBuild(c.Text) {
				return true
			}
		}
	}
	return false
}

// offsetOf returns pos's byte offset. pos must belong to df.tf, else
// token.File.Offset panics.
func (df *File) offsetOf(pos token.Pos) int {
	return df.tf.Offset(pos)
}

// lineStartOffset walks back to the start of offset's line (just past the
// preceding \n), anchoring a field's bodyStart at column 0. Clamped at 0 for
// line 1.
func (df *File) lineStartOffset(offset int) int {
	for offset > 0 && df.source[offset-1] != '\n' {
		offset--
	}
	return offset
}

// lineEndOffset returns the offset just past offset's line terminator, so
// source[lineStart:lineEnd] is a whole line including its \n. EOF without a
// trailing newline returns len(df.source).
func (df *File) lineEndOffset(offset int) int {
	for offset < len(df.source) && df.source[offset] != '\n' {
		offset++
	}
	if offset < len(df.source) {
		offset++ // include the newline
	}
	return offset
}

// consumeBlankLines advances past blank lines (whitespace only) from off,
// stopping at the first non-blank line or limit. Rule 4 uses it to pull
// trailing blanks into a field's span so a reorder doesn't orphan them. The
// limit clamp (next field's bodyStart) prevents double-attaching its leading
// whitespace.
func (df *File) consumeBlankLines(off, limit int) int {
	for off < limit {
		lineEnd := df.lineEndOffset(off)
		blank := true
		end := min(lineEnd-1, limit)
		for j := off; j < end; j++ {
			if df.source[j] != '\t' && df.source[j] != ' ' {
				blank = false
				break
			}
		}
		if !blank {
			return off
		}
		if lineEnd > limit {
			return limit // never overshoot caller's limit, even on mid-line input
		}
		off = lineEnd
	}
	return off
}

// filterOutermost drops dirty structs nested inside another dirty struct so
// spliceDirty's monotone cursor never emits a backward slice. dirty is in source
// order, so a forward scan with a containment check suffices. Dropped inner
// reorders are lost but the file stays valid Go (the safe failure mode);
// defense in depth, since betteralign only mutates top-level structs today.
// O(K²) but K is tiny.
func filterOutermost(dirty []*StructType) []*StructType {
	out := dirty[:0:0]
	for _, st := range dirty {
		contained := false
		for _, other := range out {
			if st.bodyStart > other.bodyStart && st.bodyEnd < other.bodyEnd {
				contained = true
				break
			}
		}
		if !contained {
			out = append(out, st)
		}
	}
	return out
}

// Fprint writes f's source with any Fields.List reorders applied, through gofmt
// and a re-parse before handing bytes to w. The re-parse is deliberate: gofmt
// can silently accept input it rewrites into broken Go (BUG-42's build-constraint
// promotion), so a successful Fprint guarantees w received parsing bytes.
//
// With nothing dirty it writes f.source verbatim — a true identity with no
// formatting drift. gofmt/re-parse failures wrap ErrFormat so callers can skip
// the file. Each field emits both lead- and trail-blanks; gofmt coalesces the
// duplicates, matching sirkon/dst's dual-decoration without a coalescer here.
func Fprint(w io.Writer, f *File) error {
	dirty := dirtyStructs(f)
	if len(dirty) == 0 {
		_, err := w.Write(f.source)
		return err
	}
	dirty = filterOutermost(dirty)
	out := spliceDirty(f, dirty)
	formatted, err := format.Source(out)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrFormat, err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "", formatted, parser.SkipObjectResolution); err != nil {
		return fmt.Errorf("%w: re-parse: %w", ErrFormat, err)
	}
	_, err = w.Write(formatted)
	return err
}

// dirtyStructs collects structs whose Fields.List was reordered since
// decoration, letting Fprint choose the verbatim or splice path.
func dirtyStructs(f *File) []*StructType {
	out := make([]*StructType, 0, 4)
	for _, st := range f.structs {
		if isDirty(st) {
			out = append(out, st)
		}
	}
	return out
}

// isDirty reports whether st's field list diverged from the decoration-time
// snapshot, by *Field pointer-sequence comparison. Reorder keeps the pointers
// intact, and the API exposes no *Field constructor, so this can't false-positive.
func isDirty(st *StructType) bool {
	if len(st.Fields.List) != len(st.origList) {
		return true
	}
	for i, fld := range st.Fields.List {
		if fld != st.origList[i] {
			return true
		}
	}
	return false
}

// spliceDirty walks dirty in source order, alternating verbatim source spans
// with synthesised bodies. Source order plus filterOutermost (no inner dirty)
// keeps the cursor monotone, so it never skips a span. The buffer is pre-grown
// to len(f.source).
func spliceDirty(f *File, dirty []*StructType) []byte {
	var buf bytes.Buffer
	buf.Grow(len(f.source))
	cursor := 0
	for _, st := range dirty {
		buf.Write(f.source[cursor:st.bodyStart])
		synthesizeBodyInto(&buf, f, st)
		cursor = st.bodyEnd
	}
	buf.Write(f.source[cursor:])
	return buf.Bytes()
}

// synthesizeBodyInto emits one struct body in the (possibly reordered) field
// order, each field as: leadBlanks, indented lead-doc, verbatim body, trailing
// block, trailBlanks. No inter-field separator is needed since every body span
// ends in \n. The dual-attachment (every field carries both its lead- and
// trail-blanks) is what makes reorder safe: after a swap both halves of each gap
// are emitted and gofmt coalesces the duplicates.
func synthesizeBodyInto(buf *bytes.Buffer, f *File, st *StructType) {
	for _, fld := range st.Fields.List {
		if fld.leadBlanksEnd > fld.leadBlanksStart {
			buf.Write(f.source[fld.leadBlanksStart:fld.leadBlanksEnd])
		}
		for _, c := range fld.lead {
			buf.WriteString(st.indent)
			buf.WriteString(c)
			buf.WriteByte('\n')
		}
		buf.Write(f.source[fld.bodyStart:fld.bodyEnd])
		if fld.trailEnd > fld.trailStart {
			buf.Write(f.source[fld.trailStart:fld.trailEnd])
		}
		if fld.trailBlanksEnd > fld.trailBlanksStart {
			buf.Write(f.source[fld.trailBlanksStart:fld.trailBlanksEnd])
		}
	}
}
