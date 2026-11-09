package main

import (
	"regexp"
	"testing"
)

func TestVersionLine(t *testing.T) {
	defer func(v, c, d string) { version, commit, date = v, c, d }(version, commit, date)
	version, commit, date = "0.1.0", "abc1234", "2026-11-08T12:00:00Z"
	want := regexp.MustCompile(`^ucfs 0\.1\.0 \(commit abc1234, built 2026-11-08T12:00:00Z, go1\.\d+.*, \w+/\w+\)$`)
	if got := versionLine(); !want.MatchString(got) {
		t.Errorf("versionLine() = %q", got)
	}
	version, commit, date = "dev", "", ""
	if got := versionLine(); !regexp.MustCompile(`^ucfs dev \(commit \S+`).MatchString(got) {
		t.Errorf("versionLine() without ldflags = %q", got)
	}
}
