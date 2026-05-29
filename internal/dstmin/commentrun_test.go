// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package dstmin

import (
	"go/ast"
	"go/token"
	"testing"
)

// bruteCommentRun is the obvious O(C) reference: every comment group whose
// byte range overlaps the open struct-body interval (opening, closing), in
// source order. commentRun must return exactly this, just via binary search.
func bruteCommentRun(comments []*ast.CommentGroup, opening, closing token.Pos) []*ast.CommentGroup {
	var out []*ast.CommentGroup
	for _, cg := range comments {
		if cg.End() > opening && cg.Pos() < closing {
			out = append(out, cg)
		}
	}
	return out
}

func TestCommentRun_MatchesBruteForceForEveryStruct(t *testing.T) {
	sources := map[string]string{
		"none": `package p

type S struct {
	a int
	b int
}
`,
		"before_inside_after": `package p

// doc before S
type S struct {
	// lead a
	a int // trail a
	// floating
	b int
}

// doc after S
var x int
`,
		"two_structs": `package p

// doc T
type T struct {
	// lead p
	p int
}

// doc U
type U struct {
	q int // trail q
	// floating between q and r
	r int
}
`,
		"nested": `package p

type Outer struct {
	// outer lead
	Inner struct {
		// inner lead
		z int // inner trail
	}
	// outer floating
	w int
}
`,
	}

	for name, src := range sources {
		t.Run(name, func(t *testing.T) {
			_, f, _ := parseSource(t, src)

			var structs []*ast.StructType
			ast.Inspect(f, func(n ast.Node) bool {
				if st, ok := n.(*ast.StructType); ok && st.Fields != nil {
					structs = append(structs, st)
				}
				return true
			})
			if len(structs) == 0 {
				t.Fatal("fixture produced no structs")
			}

			for i, st := range structs {
				want := bruteCommentRun(f.Comments, st.Fields.Opening, st.Fields.Closing)
				got := commentRun(f.Comments, st.Fields.Opening, st.Fields.Closing)
				if len(got) != len(want) {
					t.Fatalf("struct %d: commentRun len = %d, want %d", i, len(got), len(want))
				}
				for j := range want {
					if got[j] != want[j] {
						t.Errorf("struct %d: commentRun[%d] = %p, want %p", i, j, got[j], want[j])
					}
				}
			}
		})
	}
}
