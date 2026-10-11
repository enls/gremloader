package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/runlock"
	"github.com/enls/gremloader/internal/testutil/archives"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files of the command-line output")

var (
	localTime = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}(:\d{2})?`)
	goBuild   = regexp.MustCompile(`go\d+\.\d+(\.\d+)?, \w+/\w+`)
)

// normalize makes output comparable across machines and OSes: temporary folders, the fake
// server's port, local times, the Go version and path separators.
func (r *rig) normalize(s string) string {
	for _, p := range [][2]string{{r.home, "<HOME>"}, {r.root, "<ROOT>"}, {r.srv.APIBase(), "<API>"}, {r.srv.WebBase(), "<WEB>"}} {
		s = strings.ReplaceAll(s, strings.ReplaceAll(p[0], `\`, `\\`), p[1]) // as JSON writes it
		s = strings.ReplaceAll(s, p[0], p[1])
	}
	s = strings.ReplaceAll(s, `\\`, "/")
	s = strings.ReplaceAll(s, `\`, "/")
	s = localTime.ReplaceAllString(s, "<TIME>")
	return goBuild.ReplaceAllString(s, "<GO>")
}

// golden compares one command's output with testdata/golden/<name>.golden (go test -update
// rewrites them).
func (r *rig) golden(t *testing.T, name string, args ...string) {
	t.Helper()
	code, out, errOut := r.run(args...)
	got := r.normalize(fmt.Sprintf("$ gremloader %s\nexit %d\n--- stdout\n%s--- stderr\n%s", strings.Join(args, " "), code, out, errOut))
	path := filepath.Join("testdata", "golden", name+".golden")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run `go test ./internal/cli -run Golden -update` to create it)", err)
	}
	if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Errorf("%s differs from %s:\n--- got\n%s\n--- want\n%s", name, path, got, want)
	}
}

// One session of a user's day, each output pinned: what is printed is part of the interface.
func TestGolden(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"), archives.F("readme.txt", "hi"))

	r.golden(t, "01-list-nothing-installed", "list")
	r.golden(t, "02-plan", "plan")
	r.golden(t, "03-install-without-a-terminal", "install", "app")
	r.golden(t, "04-install", "install", "app", "--yes")
	r.publish("2.0.0", 5, archives.F("app.exe", "2"), archives.F("readme.txt", "hi"))
	r.golden(t, "05-check", "check")
	r.golden(t, "06-check-json", "check", "--json")
	r.golden(t, "07-update", "update", "--yes")
	r.golden(t, "08-list", "list")
	r.golden(t, "09-list-json", "list", "--json")
	r.golden(t, "10-explain", "explain", "app")
	if err := os.WriteFile(filepath.Join(r.dir(), "readme.txt"), []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.golden(t, "11-verify-a-changed-file", "verify")
	r.golden(t, "12-rollback", "rollback", "app", "--yes")
	r.publishBadSums("3.0.0")
	r.golden(t, "13-hash-mismatch", "update", "--yes")
	r.golden(t, "14-usage-error", "update", "--version", "3.0.0", "--yes")
	r.golden(t, "15-log", "log", "--pkg", "app")

	lk, err := runlock.Acquire(filepath.Join(r.stateDir(), "run.lock"), runlock.Holder{PID: 4242, Version: "test", Since: r.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	r.golden(t, "16-locked", "check")
	_ = lk.Release()

	r.writeFile("schema_version = 1\n\n[package.app]\nrepo = \"o/app\"\nasset = \"app.zip\"\ncolour = \"blue\"\n")
	r.golden(t, "17-package-file-problem", "list")
}
