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
// ldflags were not used (e.g. `go install module@version`). Go embeds the
// module version in info.Main.Version and, for VCS-aware builds, the revision
// and commit time in info.Settings.
func fillFromBuildInfo() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}

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

// getVersionString returns a human-readable version banner of the form:
//
//	betteralign version X.Y.Z (commit short-SHA) built on YYYY-MM-DD
//
// The commit and build-date segments are omitted when their corresponding
// ldflags were not provided (still equal to defaultUnknownInfo). Commit hashes
// longer than seven characters are truncated to their short form. Build
// dates of ten or more characters are sliced to their first ten bytes (the
// YYYY-MM-DD prefix of an RFC 3339 timestamp); shorter dates are emitted
// as-is.
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
