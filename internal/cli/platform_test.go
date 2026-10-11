package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/platform"
	"github.com/enls/gremloader/internal/testutil/archives"
)

// twoOSFile installs app from a per-platform asset into a per-OS folder, as the Craft file does.
const twoOSFile = `schema_version = 1

[package.app]
repo      = "o/app"
asset     = { "windows/amd64" = "app-{version}-win.zip", "linux/amd64" = "app-{version}.zip" }
checksums = "SHA256SUMS.txt"
dir       = { windows = '{root}\App', linux = "~/.local" }
`

// The platform the run installs for can be set, so the Linux layout is tested on Windows and the
// Windows one on Linux; the engine, the asset choice and the folder all follow it.
func TestPlatformCanBeChosen(t *testing.T) {
	r := newRig(t, twoOSFile)
	r.platform = platform.Platform{OS: "linux", Arch: "amd64"}
	if err := os.MkdirAll(r.deps().home, 0o755); err != nil {
		t.Fatal(err)
	}
	r.publish("1.0.0", 10, archives.X("bin/app", "app 1"))
	if code, out, errOut := r.run("install", "app", "--yes"); code != 0 {
		t.Fatalf("install for linux: exit %d\n%s%s", code, out, errOut)
	}
	b, err := os.ReadFile(filepath.Join(r.deps().home, ".local", "bin", "app"))
	if err != nil || string(b) != "app 1" {
		t.Fatalf("~/.local/bin/app = %q, %v", b, err)
	}

	r = newRig(t, twoOSFile)
	r.platform = platform.Platform{OS: "windows", Arch: "amd64"}
	r.publish("1.0.0", 10, archives.X("bin/app", "app 1"))
	code, out, errOut := r.run("install", "app", "--yes")
	if code == 0 || !strings.Contains(out+errOut, "app-1.0.0-win.zip") {
		t.Fatalf("install for windows without its asset: exit %d\n%s%s", code, out, errOut)
	}
}
