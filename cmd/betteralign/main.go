// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//
// Forked and modified by Dinko Korunic, 2022

package main

import (
	_ "github.com/KimMachineGun/automemlimit"
	"github.com/dkorunic/betteralign"
	"go.uber.org/automaxprocs/maxprocs"
	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() {
	undo, _ := maxprocs.Set()
	defer undo()

	singlechecker.Main(betteralign.Analyzer)
}
