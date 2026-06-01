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
// File, StructType, FieldList, Field, FieldListDecorations,
// Decorations) so the caller diff is minimal.
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

// NewDecorator constructs a Decorator bound to fset for the lifetime of the
// decoration session. Reuse across multiple DecorateFile calls is supported
// and intentional: each call appends into Dst.Nodes so the caller can resolve
// any *ast.StructType back to its DST wrapper without re-decorating. fset
// must be the same FileSet the input *ast.File was parsed against, otherwise
// position lookups (tf.Offset, tf.Line) return garbage. Not safe for
// concurrent decoration through one Decorator — the Dst.Nodes map is plain.
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
	Decs FieldListDecorations
}

// FieldListDecorations holds decoration buckets. Only Opening is decorator-populated.
type FieldListDecorations struct {
	Start   Decorations
	Opening Decorations
	End     Decorations
}

// Decorations is a sequence of parser-produced comment-text strings.
type Decorations []string

// nestedRange is a [lo, hi) token.Pos interval of a nested struct body.
type nestedRange struct{ lo, hi token.Pos }

// All exposes the underlying slice as []string. Exists solely for API parity
// with github.com/sirkon/dst, whose Decorations type wraps the slice through
// a getter — keeping the same shape here means the betteralign caller diff
// against the sirkon/dst era stayed minimal. The slice is aliased, not
// copied; mutating it mutates the receiver.
func (d Decorations) All() []string { return []string(d) }

// Field wraps *ast.Field.
type Field struct {
	ast   *ast.Field
	Names []struct{}
	// Lead-doc comment lines, in source order.
	lead Decorations
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

// DecorateFile is the production entry point: it reads the source from disk
// (the path comes from dec.Fset.File(f.Pos()).Name(), so f must belong to
// dec.Fset) and hands off to DecorateFileSrc. Splice-based reprinting needs
// the verbatim source bytes — they are NOT recoverable from the AST alone,
// since the parser discards positional whitespace and comments that don't
// belong to any node. Returns ErrSourceRead wrapped with the path on read
// failure so callers can errors.Is() the sentinel without parsing strings.
// The returned *File retains the source slice by reference; do not mutate
// the bytes underneath.
func (dec *Decorator) DecorateFile(f *ast.File) (*File, error) {
	tf := dec.Fset.File(f.Pos())
	src, err := os.ReadFile(tf.Name())
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrSourceRead, tf.Name(), err)
	}
	return dec.DecorateFileSrc(f, src), nil
}

// DecorateFileSrc is the in-memory entry point used by tests and fuzz
// harnesses that already hold the source bytes. f must come from parsing src
// with parser.ParseComments through dec.Fset, otherwise the recorded
// positions don't line up with the source and splicing emits garbage. The
// returned *File aliases src; never mutate the bytes for the lifetime of
// the *File. The decoration runs in four passes against the AST: (1) a
// single preorder walk that filters structs through buildStruct + the
// safety guards (hasUnsafeBlockComment, hasBuildConstraintComment,
// hasLineDirectiveComment), each fed the per-struct commentRun rather than
// the whole f.Comments slice, and records nested-struct ranges for the next
// pass; (2) implicit during the walk — registering decorated structs in
// dec.Dst.Nodes; (3) per-struct comment routing via decorateComments, over
// the same narrowed run and using the precomputed nested ranges to avoid
// re-walking the AST; (4)
// per-field lead/trail blank spans so Fprint can dual-emit boundary
// whitespace and let gofmt coalesce duplicates. Never errors — inputs
// that would corrupt splicing are silently filtered out and their
// structs simply do not appear in the returned File.structs.
func (dec *Decorator) DecorateFileSrc(f *ast.File, src []byte) *File {
	tf := dec.Fset.File(f.Pos())
	df := &File{ast: f, source: src, tf: tf}

	// Single preorder walk registers decoratable structs and records nested-range entries
	// against decorated ancestors. The stack holds every struct visited so far; decoratedSet
	// filters which ancestors contribute ranges.
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

// buildStruct constructs the DST wrappers and per-field byte spans for one
// struct, or returns (nil, false) when the struct's source shape would break
// splice-based reprinting. The returned StructType is fully populated apart
// from comment-derived state (lead/trail), which decorateComments fills in
// later. The rejection sweep here is the single source of truth for "shapes
// dstmin will never touch"; each `return nil, false` carries the originating
// bug tag so future regressions land near the matching test:
//
//   - single-line struct ({ ... } on one line): span arithmetic inverts.
//   - first field on the { line: field span would overlap struct header.
//   - last field on the } line (BUG-31): field span overflows struct footer.
//   - consecutive fields sharing a source line (BUG-34/38): no clean
//     per-field byte cut.
//   - body containing \r (BUG-35) or \f (BUG-37): position math and gofmt
//     trailing-newline collapse, respectively, both break splicing.
//   - body containing *//* or *///: gofmt-impossible patterns (BUG-40).
//
// hasUnsafeBlockComment / hasBuildConstraintComment / hasLineDirectiveComment
// catch comment-induced shapes; this function handles structural shapes only.
// Pure with respect to df.source — never mutates the bytes.
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

// decorateComments routes each in-body comment group to one of four
// attachment slots (Opening, lead-doc of a field, trailing block of a field,
// or none) so that reorder can move comments together with their owning
// field. fileComments is the caller-narrowed run for st (see commentRun),
// not the whole file's comment list — every entry already overlaps st's
// body, so the insideStructBody re-filter below only has to discard the
// boundary-touching edge cases. nested is the precomputed slice of
// descendant struct body ranges for st — comments inside an inner struct
// are routed by that inner struct's own decorateComments call, not by the
// outer one. Two changes keep this off the O(structs × comments) quadratic:
// hoisting the nested list out of an ast.Inspect re-walk per struct, and
// the commentRun binary-search narrowing at the call site. Together the
// pass is O(structs × log C + Σ run lengths); for the common flat,
// non-nested layout the run lengths sum to ~C, giving O(N log C + C).
//
// The four rules are applied in order; the first match wins:
//
//	Rule 1: comment on the { line          -> Opening
//	Rule 3: comment starts on a field's last line -> field's trail
//	Rule 2: comment ends on the line just before a field -> field's lead-doc
//	Rule 4: floating block between fields  -> nearest preceding field's trail
//
// Rule numbering predates the implementation order and is preserved to keep
// the BUG- references readable (BUG-36 / BUG-39 are extension fixes on
// Rules 4 and 2 respectively, where a comment group spans across the rule
// boundary; the in-line // comments at each fix mark the BUG tag).
// Mutates dstSt.Fields.List entries in place. Pure with respect to df.source.
func (dec *Decorator) decorateComments(df *File, st *ast.StructType, dstSt *StructType, fileComments []*ast.CommentGroup, nested []nestedRange) {
	if len(dstSt.Fields.List) == 0 {
		// No fields: every body comment routes to Opening.
		for _, cg := range fileComments {
			if !insideStructBody(st, cg) {
				continue
			}
			for _, c := range cg.List {
				dstSt.Fields.Decs.Opening = append(dstSt.Fields.Decs.Opening, c.Text)
			}
		}
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

		// Rule 1: comment on the { line.
		if cgStartLine == lbraceLine {
			for _, c := range cg.List {
				dstSt.Fields.Decs.Opening = append(dstSt.Fields.Decs.Opening, c.Text)
			}
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
			for _, c := range cg.List {
				dstSt.Fields.Decs.Opening = append(dstSt.Fields.Decs.Opening, c.Text)
			}
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

// insideStructBody is the body-membership predicate used by decorateComments
// and the comment-based safety guards. The strict inequality on both ends
// is intentional: a comment that starts AT the { position is the brace's
// own associated comment (none in Go, but defensive), and one that ends AT
// the } position would otherwise be claimed by this struct AND swallowed
// by spliceDirty's cursor advance past bodyEnd. Treating both boundaries
// as exclusive keeps the comment in the surrounding source span, where
// the verbatim splice copies it without dstmin needing to attach it.
func insideStructBody(st *ast.StructType, cg *ast.CommentGroup) bool {
	return cg.Pos() > st.Fields.Opening && cg.End() < st.Fields.Closing
}

// commentRun returns the contiguous sub-slice of comments whose byte range
// overlaps the open struct-body interval (opening, closing). It exists to
// kill the per-struct full rescan of f.Comments that the comment-based
// guards and decorateComments would otherwise each perform: that made
// decoration O(structs × comments), quadratic on large well-commented files.
//
// The optimisation is sound because of two ast invariants: f.Comments is
// sorted by Pos, and Go comment groups never overlap, so both Pos() and
// End() grow monotonically across the slice. The overlapping groups are
// therefore one contiguous run that two binary searches isolate in
// O(log C): the lower bound is the first group whose End passes opening, the
// upper bound is the first group that starts at or after closing. Comments
// inside a nested struct still overlap the outer body and so stay in the
// outer's run — decorateComments routes them away via its nested-range skip,
// preserving the pre-optimisation behaviour exactly. The returned slice
// aliases comments; callers must not mutate it.
func commentRun(comments []*ast.CommentGroup, opening, closing token.Pos) []*ast.CommentGroup {
	n := len(comments)
	lo := sort.Search(n, func(i int) bool { return comments[i].End() > opening })
	hi := sort.Search(n, func(i int) bool { return comments[i].Pos() >= closing })
	if lo >= hi {
		return nil
	}
	return comments[lo:hi]
}

// hasUnsafeBlockComment rejects structs whose body holds a block comment
// shape that splice-based reprinting cannot keep consistent. Three failure
// modes are covered together because all three are queries against the
// same iterator over comments — separating them would re-scan three times:
//
//   - BUG-32: a /* ... */ that spans the { boundary (opens on the { line,
//     closes inside the body). The two halves would land in different
//     splice partitions (struct-body vs surrounding source) and reorder
//     would split the comment in two.
//   - BUG-44: a block comment whose closing */ lands on the } line, before
//     the brace. decorateComments routes it to a field's trail, whose span
//     then reaches lineEndOffset of the } line and swallows the brace;
//     reorder emits } mid-body and the struct closes early, dropping a
//     field. Covers both the multi-line close-brace cross and the
//     single-line /* */-on-the-}-line case.
//   - BUG-33: a multi-line block comment whose closing */ lands on a
//     field's first line. The header bytes and the field bytes share a
//     line, so there's no clean per-field byte cut.
//   - BUG-41: a comment group containing multiple comments where any of
//     them is a block comment. Comment routing assigns the group as a unit;
//     the trailing/lead bytes can't be kept consistent when reorder moves
//     the group's anchor.
//
// fieldFirstLines is built lazily so the common case (no block comments in
// body) stays alloc-free. Pure predicate, no mutation. Caller must have
// already ruled out single-line { ... } via buildStruct's earlier checks —
// the lbraceLine/rbraceLine comparisons assume {/} are on distinct lines.
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

// hasLineDirectiveComment rejects structs whose body carries a recognised
// Go line directive (//line file:line or /*line file:line*/). BUG-43: the
// parser remaps tf.Line() for every position after such a directive to the
// logical line; dstmin's other guards (hasUnsafeBlockComment, the per-rule
// line comparisons in decorateComments, the consecutive-field check in
// buildStruct) all read tf.Line() expecting monotonic growth with byte
// offset, so a directive in the body lets a comment's *end* (physical line)
// and a later field's *start* (logical line) sit in different number spaces
// and overlap silently. The prefix match mirrors the scanner's recognition
// rule exactly: literal "//line " or "/*line " with a single space after
// "line"; tabs, newlines, and forms like /*line:42*/ are not directives and
// stay decoratable. Pure predicate, no mutation.
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

// hasBuildConstraintComment rejects structs whose body carries a comment
// gofmt recognises as a build constraint (//go:build, //+build, or
// // +build). BUG-42: gofmt hoists such directives to the file header even
// when they appear mid-struct, and in doing so drops the line break
// separating the carrier field from its successor — after a reorder the
// resulting field count diverges from the gofmt-normalised input. Detection
// delegates to go/build/constraint.IsGoBuild and IsPlusBuild so the rule
// stays in lock-step with gofmt's own recognition, including the word
// boundary that excludes lookalikes like //go:buildtag or //+buildfoo. Pure
// predicate, no mutation.
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

// offsetOf is the byte-offset accessor used throughout the splice math.
// Wraps token.File.Offset for readability and because every offset-using
// call site does df.offsetOf(pos), making the bare token.File access
// implicit. pos must be a valid position inside df.tf; passing a pos from
// another file panics inside token.File.Offset.
func (df *File) offsetOf(pos token.Pos) int {
	return df.tf.Offset(pos)
}

// lineStartOffset is the inclusive lower bound for "take this line's bytes".
// Used to anchor a field's bodyStart at column 0 even when the field
// declaration starts mid-line in the source (after indentation or after a
// trailing-comment-of-previous-field). Walking back to the preceding \n is
// O(line length), which dominates only on pathological single-line files;
// real Go code keeps it tiny. offset is clamped at 0 to handle line 1.
func (df *File) lineStartOffset(offset int) int {
	for offset > 0 && df.source[offset-1] != '\n' {
		offset--
	}
	return offset
}

// lineEndOffset is the exclusive upper bound for "take this line's bytes".
// Returned offset is positioned just past the trailing \n so that
// source[lineStart:lineEnd] is a self-contained line including its
// terminator. EOF without a trailing newline returns len(df.source), which
// callers slice safely — bodyEnd at EOF stays in-range. O(line length).
func (df *File) lineEndOffset(offset int) int {
	for offset < len(df.source) && df.source[offset] != '\n' {
		offset++
	}
	if offset < len(df.source) {
		offset++ // include the newline
	}
	return offset
}

// consumeBlankLines advances past blank lines (tabs and spaces only before
// the newline) starting at off, stopping at the first non-blank line or at
// limit, whichever comes first. Used by Rule 4 in decorateComments to grow
// a field's trailing span to include trailing blanks that visually belong
// to the just-attached floating block — without this, reorder would leave
// orphaned blanks behind the moving field's old position. The limit clamp
// is load-bearing: the next field's bodyStart is the natural boundary, and
// overshooting would double-attach the next field's leading whitespace.
// O(scanned bytes); trivial in practice.
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

// filterOutermost keeps only the outermost dirty structs so spliceDirty's
// monotone cursor advance does not emit a backward slice. dirty arrives in
// source order (parent before children, courtesy of ast.Inspect during
// DecorateFileSrc), so a single forward scan with a containment check
// against the kept-so-far set is sufficient. Inner dirty structs are
// silently dropped: their reorder is lost but the emitted file stays
// valid Go, which is the safe failure mode. In practice betteralign's
// caller filters to named top-level struct declarations before mutating,
// so nested dirties are not reachable today — the filter exists as a
// defence in depth against future callers that mutate freely.
//
// O(K²) in the number of dirty structs, but K is tiny (typically 0 or 1
// per file, occasionally a handful) so the quadratic factor never bites.
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

// Fprint is the public emit entry point. It writes f's source with any
// Fields.List mutations applied, going through gofmt on the way out and
// re-parsing the formatted result to verify well-formed Go before handing
// bytes to w. The double-validation is deliberate: gofmt sometimes
// silently accepts inputs it rewrites into syntactically broken Go (the
// build-constraint promotion case from BUG-42 was discovered through this
// re-parse check), so callers can rely on a successful Fprint meaning
// "the byte slice w received parses".
//
// When no struct in f has been mutated (dirtyStructs returns empty), Fprint
// short-circuits by writing f.source verbatim — neither gofmt nor reparse
// runs. This is what makes the "decorated but never mutated" path a true
// identity: no formatting drift, no risk of gofmt rewriting unrelated
// code.
//
// On gofmt failure or re-parse failure the error wraps ErrFormat so
// callers can errors.Is() and route the failure to "skip this file" rather
// than aborting. Each field's lead-blanks and trail-blanks both get
// emitted; gofmt's coalescing strips the resulting duplicate blank lines
// and brace-adjacent blanks, mirroring sirkon/dst's dual-decoration model
// without dstmin needing its own coalescer.
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

// dirtyStructs collects the structs whose Fields.List the caller has
// reordered since decoration. The comparison is by *Field pointer identity,
// not deep equality, so swap-in-place is detected reliably without dstmin
// having to clone fields. Used by Fprint to decide between the verbatim
// fast path and the splice-and-format slow path. Allocation is preserved
// at zero when nothing is dirty.
func dirtyStructs(f *File) []*StructType {
	out := make([]*StructType, 0, 4)
	for _, st := range f.structs {
		if isDirty(st) {
			out = append(out, st)
		}
	}
	return out
}

// isDirty reports whether st's field list has diverged from the snapshot
// taken at decoration time, by pointer-sequence comparison rather than deep
// equality. A caller that creates new *Field values would falsely appear
// dirty, but the dstmin API exposes no constructor for *Field — the only
// legitimate mutation is reorder of the existing slice, which keeps the
// pointers intact. Length divergence is the cheap early-out for hypothetical
// add/remove.
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

// spliceDirty assembles the output by walking dirty in source order,
// alternating verbatim copies of f.source between body-spans with
// synthesised body content for each dirty struct. The source-order
// precondition is what makes the monotone cursor safe — filterOutermost
// guarantees no inner struct is dirty, so the cursor advance past bodyEnd
// never skips over a span we still need to emit. Pre-allocating the
// buffer to len(f.source) avoids the regrowth loop on the typical "small
// reorder of a large file" case.
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

// synthesizeBodyInto emits one struct body in the (possibly-reordered)
// field sequence, using the decoration-time byte spans for body and
// trailing-block plus the routed lead-doc lines. The emit order per field
// is: leadBlanks (whitespace inherited from the previous field's tail),
// lead-doc comments with the struct's indent re-prepended, verbatim body
// bytes, trailing block bytes, trailBlanks. No separator is interpolated
// between fields because every body-span ends in \n (lineEndOffset
// guarantees) — relying on this saves dstmin from having to reason about
// what separator the original source used.
//
// The dual-attachment design (every field carries both leadBlanks AND
// trailBlanks, even though they are adjacent neighbours' bytes) is what
// makes reorder safe: after a swap, both halves of every gap are emitted
// from the new neighbours' perspective, gofmt coalesces the duplicate
// blank lines, and the output matches what sirkon/dst would have produced.
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
