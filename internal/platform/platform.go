// Package platform names the operating system and CPU a package is installed for, the way the
// package file and the lock file spell it: Go's own names, "windows/amd64", "linux/arm64".
//
// The known set is deliberately small. A key outside it in a package file is almost always a typo
// ("window/amd64", "linux/x86_64"), and silently never matching would be worse than an error.
package platform

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
)

// OSes and Arches are the names a package file may use.
var (
	OSes   = []string{"darwin", "linux", "windows"}
	Arches = []string{"386", "amd64", "arm", "arm64"}
)

// Platform is one OS and CPU.
type Platform struct {
	OS   string
	Arch string
}

// Current is the platform this binary runs on.
func Current() Platform { return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH} }

func (p Platform) String() string { return p.OS + "/" + p.Arch }

// Parse reads "os/arch".
func Parse(s string) (Platform, error) {
	os, arch, ok := strings.Cut(s, "/")
	if !ok {
		return Platform{}, fmt.Errorf("platform %q: want os/arch, for example windows/amd64", s)
	}
	if !ValidOS(os) {
		return Platform{}, fmt.Errorf("platform %q: unknown operating system %q (known: %s)", s, os, strings.Join(OSes, ", "))
	}
	if !slices.Contains(Arches, arch) {
		return Platform{}, fmt.Errorf("platform %q: unknown architecture %q (known: %s)", s, arch, strings.Join(Arches, ", "))
	}
	return Platform{OS: os, Arch: arch}, nil
}

// ValidOS reports whether s is a known operating system name.
func ValidOS(s string) bool { return slices.Contains(OSes, s) }
