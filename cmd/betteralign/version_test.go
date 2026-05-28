// Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
//
// SPDX-FileCopyrightText: Copyright (c) 2026 Dinko Korunic <dinko.korunic@gmail.com>
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

// withVersionVars installs the trio for the test and restores it on cleanup.
func withVersionVars(t *testing.T, version, commit, date string) {
	t.Helper()
	origV, origC, origD := Version, Commit, Date
	t.Cleanup(func() {
		Version = origV
		Commit = origC
		Date = origD
	})
	Version = version
	Commit = commit
	Date = date
}

// TestGetVersionString_CommitTruncation pins the seven-char short-SHA contract.
func TestGetVersionString_CommitTruncation(t *testing.T) {
	tests := []struct {
		name      string
		commit    string
		wantShort string
	}{
		{
			name:      "long hash truncated to seven chars",
			commit:    "abcdef1234567890",
			wantShort: "abcdef1",
		},
		{
			name:      "exactly seven chars not truncated",
			commit:    "abcdef1",
			wantShort: "abcdef1",
		},
		{
			name:      "six chars not truncated",
			commit:    "abcdef",
			wantShort: "abcdef",
		},
		{
			name:      "boundary: eight chars truncated to seven",
			commit:    "abcdef12",
			wantShort: "abcdef1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withVersionVars(t, "v1.0.0", tc.commit, defaultUnknownInfo)
			got := getVersionString()
			want := "(commit " + tc.wantShort + ")"
			if !strings.Contains(got, want) {
				t.Errorf("getVersionString() = %q, want substring %q", got, want)
			}
		})
	}
}

// TestGetVersionString_DateTruncation pins the YYYY-MM-DD ten-char truncation contract.
func TestGetVersionString_DateTruncation(t *testing.T) {
	tests := []struct {
		name     string
		date     string
		wantDate string
	}{
		{
			name:     "RFC3339 timestamp truncated to YYYY-MM-DD",
			date:     "2026-05-28T12:34:56Z",
			wantDate: "2026-05-28",
		},
		{
			name:     "exactly ten chars not truncated",
			date:     "2026-05-28",
			wantDate: "2026-05-28",
		},
		{
			name:     "short date emitted verbatim",
			date:     "today",
			wantDate: "today",
		},
		{
			name:     "boundary: eleven chars truncated to ten",
			date:     "2026-05-28T",
			wantDate: "2026-05-28",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withVersionVars(t, "v1.0.0", defaultUnknownInfo, tc.date)
			got := getVersionString()
			want := "built on " + tc.wantDate
			if !strings.Contains(got, want) {
				t.Errorf("getVersionString() = %q, want substring %q", got, want)
			}
			// Byte 11 (the `T` in RFC3339) must never leak through.
			if len(tc.date) >= 11 && strings.Contains(got, "built on "+tc.date[:11]) {
				t.Errorf("getVersionString() leaked byte 11 of date: %q", got)
			}
		})
	}
}

// TestFillFromBuildInfo_DevelFiltered pins that "(devel)" must never reach the banner.
func TestFillFromBuildInfo_DevelFiltered(t *testing.T) {
	tests := []struct {
		name        string
		mainVersion string
		wantVersion string
	}{
		{
			name:        "(devel) is filtered",
			mainVersion: "(devel)",
			wantVersion: defaultDevVersion,
		},
		{
			name:        "real semver tag wins over default",
			mainVersion: "v1.2.3",
			wantVersion: "v1.2.3",
		},
		{
			name:        "empty Main.Version leaves default",
			mainVersion: "",
			wantVersion: defaultDevVersion,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withVersionVars(t, defaultDevVersion, defaultUnknownInfo, defaultUnknownInfo)
			info := &debug.BuildInfo{Main: debug.Module{Version: tc.mainVersion}}
			fillFromBuildInfoData(info)
			if Version != tc.wantVersion {
				t.Errorf("Version = %q, want %q", Version, tc.wantVersion)
			}
		})
	}
}

// TestFillFromBuildInfo_VCSSettings pins vcs.revision/vcs.time pickup, with ldflags winning over build info.
func TestFillFromBuildInfo_VCSSettings(t *testing.T) {
	t.Run("populates Commit and Date when unset", func(t *testing.T) {
		withVersionVars(t, defaultDevVersion, defaultUnknownInfo, defaultUnknownInfo)
		info := &debug.BuildInfo{
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abcdef1234567890"},
				{Key: "vcs.time", Value: "2026-05-28T12:00:00Z"},
			},
		}
		fillFromBuildInfoData(info)
		if Commit != "abcdef1234567890" {
			t.Errorf("Commit = %q, want %q", Commit, "abcdef1234567890")
		}
		if Date != "2026-05-28T12:00:00Z" {
			t.Errorf("Date = %q, want %q", Date, "2026-05-28T12:00:00Z")
		}
	})

	t.Run("ldflags-set Commit is not overwritten", func(t *testing.T) {
		withVersionVars(t, defaultDevVersion, "fromldflags", defaultUnknownInfo)
		info := &debug.BuildInfo{
			Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abcdef1"}},
		}
		fillFromBuildInfoData(info)
		if Commit != "fromldflags" {
			t.Errorf("Commit = %q, want %q (ldflags must win)", Commit, "fromldflags")
		}
	})
}

// TestWantsVersion pins recognition of -V/-version/--version and the scan-terminating
// conditions (positional arg, `--`, empty string).
func TestWantsVersion(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"-V", []string{"-V"}, true},
		{"-version", []string{"-version"}, true},
		{"--version", []string{"--version"}, true},
		{"empty", nil, false},
		{"-apply", []string{"-apply"}, false},
		{"positional then -version", []string{"./pkg/...", "-version"}, false},
		{"-- then -version", []string{"--", "-version"}, false},
		{"empty arg then -version", []string{"", "-version"}, false},
		{"flag then -V", []string{"-apply", "-V"}, true},
		{"two version flags", []string{"-V", "--version"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := wantsVersion(tc.args); got != tc.want {
				t.Errorf("wantsVersion(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestGetVersionString_OmitsUnknownSegments confirms commit/date are dropped when unset.
func TestGetVersionString_OmitsUnknownSegments(t *testing.T) {
	withVersionVars(t, "v1.0.0", defaultUnknownInfo, defaultUnknownInfo)
	got := getVersionString()
	if strings.Contains(got, "commit") {
		t.Errorf("getVersionString() = %q, must not mention commit when unknown", got)
	}
	if strings.Contains(got, "built on") {
		t.Errorf("getVersionString() = %q, must not mention build date when unknown", got)
	}
}
