package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestShortCommit(t *testing.T) {
	tests := []struct {
		commit string
		want   string
	}{
		{"", "unknown"},
		{"abc", "abc"},
		{"abcdefg", "abcdefg"},
		{"0123456789abcdef", "0123456"},
	}
	for _, tt := range tests {
		if got := (Info{Commit: tt.commit}).ShortCommit(); got != tt.want {
			t.Errorf("ShortCommit(%q) = %q, want %q", tt.commit, got, tt.want)
		}
	}
}

func TestString(t *testing.T) {
	info := Info{
		Version:   "0.0.1",
		Commit:    "0123456789abcdef",
		Date:      "2026-10-10T00:00:00Z",
		GoVersion: "go1.27.0",
		OS:        "linux",
		Arch:      "arm64",
	}
	want := "gremloader 0.0.1 (commit 0123456, built 2026-10-10T00:00:00Z, go1.27.0, linux/arm64)"
	if got := info.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	info.Modified = true
	info.Date = ""
	if got := info.String(); !strings.Contains(got, "commit 0123456+dirty,") || strings.Contains(got, "built") {
		t.Errorf("dirty/undated String() = %q", got)
	}
}

func TestFillFromBuildInfoDoesNotOverrideStamped(t *testing.T) {
	settings := []debug.BuildSetting{
		{Key: "vcs.revision", Value: "embeddedrevision"},
		{Key: "vcs.time", Value: "2020-01-01T00:00:00Z"},
		{Key: "vcs.modified", Value: "true"},
	}

	stamped := Info{Commit: "stampedcommit", Date: "2026-10-10T00:00:00Z"}
	fillFromBuildInfo(&stamped, settings)
	if stamped.Commit != "stampedcommit" || stamped.Date != "2026-10-10T00:00:00Z" {
		t.Errorf("stamped values were overridden: %+v", stamped)
	}
	if !stamped.Modified {
		t.Error("Modified should still be taken from the build info")
	}

	var bare Info
	fillFromBuildInfo(&bare, settings)
	if bare.Commit != "embeddedrevision" || bare.Date != "2020-01-01T00:00:00Z" {
		t.Errorf("unstamped build did not pick up VCS info: %+v", bare)
	}
}

func TestGetHasRuntimeFields(t *testing.T) {
	got := Get()
	if got.Version == "" || got.GoVersion == "" || got.OS == "" || got.Arch == "" {
		t.Errorf("Get() left a field empty: %+v", got)
	}
}
