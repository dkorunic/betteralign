package main

import "fmt"

const (
	defaultUnknownInfo = "unknown"
)

var (
	Version   = "dev"
	Commit    = defaultUnknownInfo
	Date      = defaultUnknownInfo
	Timestamp = defaultUnknownInfo
)

// getVersionString returns a human-readable version banner of the form:
//
//	betteralign version X.Y.Z (commit short-SHA) built on YYYY-MM-DD
//
// The commit and build-date segments are omitted when their corresponding
// ldflags were not provided (still equal to defaultUnknownInfo). Commit hashes
// longer than seven characters are truncated to their short form; build dates
// longer than ten characters are truncated to the YYYY-MM-DD prefix.
func getVersionString() string {
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
