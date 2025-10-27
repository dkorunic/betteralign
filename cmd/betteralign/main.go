// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//
// Forked and modified by Dinko Korunic, 2022

package main

import (
	"fmt"
	"os"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/dkorunic/betteralign"
	"go.uber.org/automaxprocs/maxprocs"
	"golang.org/x/tools/go/analysis/singlechecker"
)

const maxMemRatio = 0.9

func main() {
	_, _ = memlimit.SetGoMemLimitWithOpts(
		memlimit.WithRatio(maxMemRatio),
		memlimit.WithProvider(
			memlimit.ApplyFallback(
				memlimit.FromCgroup,
				memlimit.FromSystem,
			),
		),
	)

	undo, _ := maxprocs.Set()
	defer undo()

	args := os.Args[1:]

	// check for version flags
	for _, arg := range args {
		if arg == "-V" || arg == "--version" {
			fmt.Println(getVersionString())
			os.Exit(0)
		}
	}

	singlechecker.Main(betteralign.Analyzer)
}
