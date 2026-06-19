// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"runtime/debug"
)

const (
	defaultUnknownInfo = "unknown"
	defaultDevVersion  = "dev"
)

var (
	Version = defaultDevVersion
	Commit  = defaultUnknownInfo
	Date    = defaultUnknownInfo
)

// fillFromBuildInfo populates Version/Commit/Date from runtime build info when
// ldflags weren't used (e.g. `go install module@version`).
func fillFromBuildInfo() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	fillFromBuildInfoData(info)
}

// fillFromBuildInfoData is fillFromBuildInfo with BuildInfo injected for tests.
// Only fields still at their default sentinel are overwritten, so -ldflags
// values take precedence.
func fillFromBuildInfoData(info *debug.BuildInfo) {
	if Version == defaultDevVersion {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			Version = v
		}
	}

	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if Commit == defaultUnknownInfo && s.Value != "" {
				Commit = s.Value
			}
		case "vcs.time":
			if Date == defaultUnknownInfo && s.Value != "" {
				Date = s.Value
			}
		}
	}
}

// getVersionString returns a banner of the form:
//
//	betteralign version X.Y.Z (commit short-SHA) built on YYYY-MM-DD
//
// It first calls fillFromBuildInfo. The commit and date segments are omitted
// when unknown; the commit is shortened to 7 chars and the date to its
// YYYY-MM-DD prefix.
func getVersionString() string {
	fillFromBuildInfo()

	version := fmt.Sprintf("betteralign version %s", Version)

	if Commit != defaultUnknownInfo {
		shortCommit := Commit
		if len(Commit) > 7 {
			shortCommit = Commit[:7]
		}
		version += fmt.Sprintf(" (commit %s)", shortCommit)
	}

	if Date != defaultUnknownInfo {
		if len(Date) >= 10 {
			version += fmt.Sprintf(" built on %s", Date[:10])
		} else {
			version += fmt.Sprintf(" built on %s", Date)
		}
	}

	return version
}
