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

// getVersionString returns version info like:
// "betteralign version X.Y.X (commit short-SHA) built on YYYY-MM-DD"
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
