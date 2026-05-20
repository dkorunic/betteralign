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
	Version   = defaultDevVersion
	Commit    = defaultUnknownInfo
	Date      = defaultUnknownInfo
	Timestamp = defaultUnknownInfo
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
				if Timestamp == defaultUnknownInfo {
					Timestamp = s.Value
				}
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
// longer than seven characters are truncated to their short form; build dates
// longer than ten characters are truncated to the YYYY-MM-DD prefix.
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

	builtDate := Date
	if builtDate == defaultUnknownInfo && Timestamp != defaultUnknownInfo {
		builtDate = Timestamp
	}

	if builtDate != defaultUnknownInfo {
		if len(builtDate) >= 10 {
			version += fmt.Sprintf(" built on %s", builtDate[:10])
		} else {
			version += fmt.Sprintf(" built on %s", builtDate)
		}
	}

	return version
}
