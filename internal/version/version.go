// Package version holds the build identity of the binary.
//
// Version, Commit and Date are stamped at build time:
//
//	go build -ldflags "-X github.com/enls/gremloader/internal/version.Version=0.0.1 \
//	  -X github.com/enls/gremloader/internal/version.Commit=$(git rev-parse HEAD) \
//	  -X github.com/enls/gremloader/internal/version.Date=2026-10-10T00:00:00Z"
//
// An unstamped build (go run, go test, go install) reports "dev" and falls back to the VCS
// information the Go toolchain embeds, so a developer build still says which commit it is.
//
// This is the program's version. The on-disk file formats carry their own, separate
// schema_version.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Set by -ldflags -X at build time.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info is the build identity, for display and for structured output.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	Modified  bool   `json:"modified"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Get returns the identity of the running binary.
func Get() Info {
	info := Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		fillFromBuildInfo(&info, bi.Settings)
	}
	if info.Version == "" {
		info.Version = "dev"
	}
	return info
}

// fillFromBuildInfo adds VCS data the toolchain embedded, without overriding anything stamped
// explicitly with -ldflags.
func fillFromBuildInfo(info *Info, settings []debug.BuildSetting) {
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			if info.Commit == "" {
				info.Commit = s.Value
			}
		case "vcs.time":
			if info.Date == "" {
				info.Date = s.Value
			}
		case "vcs.modified":
			info.Modified = s.Value == "true"
		}
	}
}

// ShortCommit is the first 7 characters of the commit, or "unknown".
func (i Info) ShortCommit() string {
	switch {
	case i.Commit == "":
		return "unknown"
	case len(i.Commit) > 7:
		return i.Commit[:7]
	default:
		return i.Commit
	}
}

// String is the one-line human form, also used as the first line of each log.
func (i Info) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "gremloader %s (commit %s", i.Version, i.ShortCommit())
	if i.Modified {
		b.WriteString("+dirty")
	}
	if i.Date != "" {
		fmt.Fprintf(&b, ", built %s", i.Date)
	}
	fmt.Fprintf(&b, ", %s, %s/%s)", i.GoVersion, i.OS, i.Arch)
	return b.String()
}
