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
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
)

// Sentinel errors; wrapped with fmt.Errorf and classified via errors.Is.
var (
	ErrSourceRead = errors.New("dstmin: unable to read source")
	ErrFormat     = errors.New("dstmin: formatted output rejected by gofmt")
)

// Decorator owns decorated trees. Dst.Nodes maps each *ast.StructType to its
// *StructType wrapper for AST→DST lookup.
type Decorator struct {
	Fset *token.FileSet
	Dst  struct {
		Nodes map[ast.Node]any
	}
}

// NewDecorator returns a Decorator bound to fset. The returned value owns its
// own Dst.Nodes map; callers may DecorateFile multiple files through the same
// decorator and the maps accumulate.
func NewDecorator(fset *token.FileSet) *Decorator {
	d := &Decorator{Fset: fset}
	d.Dst.Nodes = make(map[ast.Node]any, 64)
	return d
}

// File is the tree root for one decorated *ast.File.
type File struct {
	ast    *ast.File
	source []byte
	tf     *token.File // cached for offsetOf.
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

// All returns the underlying slice. Provided for API parity with sirkon/dst,
// which exposes decorations as a struct method.
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

// DecorateFile decorates f and returns the resulting *File. Sources are read
// from disk via dec.Fset.File(f.Pos()).Name(). Returns ErrSourceRead wrapped
// with the filename when the read fails.
func (dec *Decorator) DecorateFile(f *ast.File) (*File, error) {
	tf := dec.Fset.File(f.Pos())
	src, err := os.ReadFile(tf.Name())
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrSourceRead, tf.Name(), err)
	}

	df := &File{ast: f, source: src, tf: tf}

	// Pass 1: register every decoratable struct.
	ast.Inspect(f, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		dstSt, ok := dec.buildStruct(df, st)
		if !ok {
			return true
		}
		dec.Dst.Nodes[st] = dstSt
		df.structs = append(df.structs, dstSt)
		return true
	})

	// Pass 2: nested-range lookup, including rejected structs, to prevent comment leak.
	nestedRanges := make(map[*ast.StructType][]nestedRange, len(df.structs))
	ast.Inspect(f, func(n ast.Node) bool {
		inner, ok := n.(*ast.StructType)
		if !ok || inner.Fields == nil {
			return true
		}
		for _, dstOuter := range df.structs {
			outer := dstOuter.ast
			if inner == outer {
				continue
			}
			if inner.Fields.Opening > outer.Fields.Opening && inner.Fields.Closing < outer.Fields.Closing {
				nestedRanges[outer] = append(nestedRanges[outer], nestedRange{lo: inner.Fields.Opening, hi: inner.Fields.Closing})
			}
		}
		return true
	})

	// Pass 3: route comments per struct.
	for _, dstSt := range df.structs {
		dec.decorateComments(df, dstSt.ast, dstSt, f.Comments, nestedRanges[dstSt.ast])
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

	return df, nil
}

// buildStruct constructs the DST wrappers for one *ast.StructType. Decoration
// (comment routing, source-span recording) is layered on top in later tasks.
// Returns (nil, false) for single-line struct declarations where { and } share
// a line — span arithmetic would invert for those, so they are not decorated.
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

// decorateComments walks file.Comments and routes each comment group inside
// the struct's body to Opening, a Field's lead, or a Field's trailing block,
// per the rules documented in the design spec. nested is the precomputed list
// of direct and indirect descendant struct body ranges for st; it is built
// once in DecorateFile to avoid per-struct ast.Inspect re-walks.
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

// insideStructBody reports whether cg is between st.Fields.Opening and
// st.Fields.Closing (exclusive of the brace lines themselves except for
// same-line cases).
func insideStructBody(st *ast.StructType, cg *ast.CommentGroup) bool {
	return cg.Pos() > st.Fields.Opening && cg.End() < st.Fields.Closing
}

// offsetOf returns the byte offset of pos within the file df.source belongs
// to. Pos must be valid and inside the file.
func (df *File) offsetOf(pos token.Pos) int {
	return df.tf.Offset(pos)
}

// lineStartOffset returns the byte offset of the first byte of the line
// containing offset. Walks back from offset to the preceding newline.
func (df *File) lineStartOffset(offset int) int {
	for offset > 0 && df.source[offset-1] != '\n' {
		offset--
	}
	return offset
}

// lineEndOffset returns the byte offset just past the next newline at or
// after offset (or len(df.source) if EOF reached). The returned offset is
// suitable as an exclusive upper bound — emitting source[start:end] includes
// the trailing newline.
func (df *File) lineEndOffset(offset int) int {
	for offset < len(df.source) && df.source[offset] != '\n' {
		offset++
	}
	if offset < len(df.source) {
		offset++ // include the newline
	}
	return offset
}

// consumeBlankLines walks lines from off, returning the offset of the first
// non-blank line at or before limit. A line is blank if it contains only tab
// or space characters before its newline.
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
		off = lineEnd
	}
	return off
}

// filterOutermost filters dirty to only the outermost dirty structs. f.structs
// is in source order from ast.Inspect (parent before children); a dirty struct
// nested inside another dirty struct's body span would cause spliceDirty to
// emit a backward slice. Drop the inner ones — their reorder is silently lost
// but the file stays valid. (In practice betteralign filters to named top-level
// struct declarations, so this case is not reachable today.)
//
// Inner dirty structs whose body span is inside another dirty struct are intentionally dropped.
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

// Fprint writes the source representation of f to w, with any modifications
// to *StructType.Fields.List applied. Returns ErrFormat wrapped with details
// when go/format.Source rejects the spliced output, or when the formatted
// bytes fail to re-parse.
//
// Each field emits both its lead-blanks and trail-blanks; gofmt's natural
// behavior coalesces the resulting double-blanks and strips brace-adjacent
// blanks, producing output that matches sirkon/dst's dual-decoration model.
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

// dirtyStructs returns every struct in f whose Fields.List has been mutated
// relative to its decoration-time snapshot (pointer-sequence comparison).
func dirtyStructs(f *File) []*StructType {
	out := make([]*StructType, 0, 4)
	for _, st := range f.structs {
		if isDirty(st) {
			out = append(out, st)
		}
	}
	return out
}

// isDirty reports whether st's current Fields.List differs (by pointer
// sequence) from the snapshot captured at decoration time.
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

// spliceDirty builds the output buffer by interleaving original-source spans
// and synthesized struct bodies. dirty must be in source order (which it is
// since DecorateFile populates f.structs in source-walk order).
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

// synthesizeBodyInto writes a struct body's bytes in the new field order
// directly into buf. Per design: lead-doc lines, the field's verbatim body
// span, the field's verbatim trailing span. Between fields, no extra separator
// (the field's bodyEnd already includes the trailing newline).
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
