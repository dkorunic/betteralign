// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//
// Forked and modified by Dinko Korunic, 2022
//
// SPDX-FileCopyrightText: Copyright 2021 The Go Authors
// SPDX-FileCopyrightText: Copyright 2022 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"os"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/dkorunic/betteralign"
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

	if wantsVersion(os.Args[1:]) {
		fmt.Println(getVersionString())
		os.Exit(0)
	}

	singlechecker.Main(betteralign.Analyzer)
}

// wantsVersion reports whether args carry a version flag before the first positional arg or `--`.
func wantsVersion(args []string) bool {
	for _, arg := range args {
		if arg == "--" || len(arg) == 0 || arg[0] != '-' {
			return false
		}
		if arg == "-V" || arg == "-version" || arg == "--version" {
			return true
		}
	}
	return false
}
