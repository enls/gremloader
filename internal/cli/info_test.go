package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/runlock"
	"github.com/enls/gremloader/internal/testutil/archives"
)

func TestCheckWithNothingInstalled(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))

	// Not installed: check only updates installed packages, so there is nothing to do.
	code, out, errOut := r.run("check")
	if code != exitcode.OK || !strings.Contains(out, "app") {
		t.Fatalf("check: exit %d\n%s%s", code, out, errOut)
	}
}

func TestCheckFindsAnUpdate(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	r.installed(t, "1.0.0")
	r.publish("2.0.0", 5, archives.F("app.exe", "2"))

	code, out, errOut := r.run("check")
	if code != exitcode.UpdatesAvailable || !strings.Contains(out, "app  1.0.0 -> 2.0.0  update") {
		t.Fatalf("check: exit %d\nstdout:\n%sstderr:\n%s", code, out, errOut)
	}

	// Flags anywhere, JSON.
	code, res := r.runJSON("--quiet", "check", "app")
	var it jsonItem
	res.item(t, 0, &it)
	if code != exitcode.UpdatesAvailable || res.Command != "check" || it.Package != "app" || it.Action != "update" ||
		it.From != "1.0.0" || it.To != "2.0.0" || it.Result != "pending" {
		t.Errorf("json: exit %d, %+v, item %+v", code, res, it)
	}

	// list now knows the latest version without the network.
	code, out, errOut = r.run("list")
	if code != exitcode.OK || !strings.Contains(out, "app") || !strings.Contains(out, "2.0.0") || !strings.Contains(out, "update available") {
		t.Errorf("list: exit %d\n%s%s", code, out, errOut)
	}
}

func TestCheckOfAnUnknownPackageIsAUsageError(t *testing.T) {
	r := newRig(t, appFile)
	code, _, errOut := r.run("check", "nope")
	if code != exitcode.Usage || !strings.Contains(errOut, `"nope"`) {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestCheckTakesTheRunLock(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	if err := os.MkdirAll(r.stateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	lk, err := runlock.Acquire(filepath.Join(r.stateDir(), "run.lock"), runlock.Holder{PID: 4242, Version: "test", Since: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lk.Release() }()
	code, _, errOut := r.run("check")
	if code != exitcode.Locked || !strings.Contains(errOut, "4242") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	code, res := r.runJSON("check")
	if code != exitcode.Locked || !strings.Contains(res.Error, "4242") {
		t.Errorf("json: %+v", res)
	}
	// list reads without the lock.
	if code, _, errOut := r.run("list"); code != exitcode.OK {
		t.Errorf("list under someone else's lock: exit %d %s", code, errOut)
	}
}

func TestListStatuses(t *testing.T) {
	r := newRig(t, appFile+`
[package.off]
repo    = "o/off"
asset   = "off.zip"
verify  = { require = [] }
dir     = "{root}/Off"
enabled = false
`)
	r.installedAs(t, "gone", "3.0.0") // in state, not in the file
	code, res := r.runJSON("list")
	if code != exitcode.OK || len(res.Items) != 3 {
		t.Fatalf("list: exit %d, %+v", code, res)
	}
	want := map[string]string{"app": "not installed", "gone": "unmanaged", "off": "disabled"}
	for i := range res.Items {
		var row listRow
		res.item(t, i, &row)
		if !strings.HasPrefix(row.Status, want[row.Package]) {
			t.Errorf("%s: status %q, want %q", row.Package, row.Status, want[row.Package])
		}
	}
	code, out, _ := r.run("list")
	if code != exitcode.OK || !strings.HasPrefix(out, "PACKAGE") {
		t.Errorf("list text:\n%s", out)
	}
}

func TestExplainShowsWhereEachSettingComesFrom(t *testing.T) {
	r := newRig(t, appFile)
	code, out, errOut := r.run("explain", "app")
	if code != exitcode.OK || !strings.Contains(out, "repo") || !strings.Contains(out, `"o/app"`) ||
		!strings.Contains(out, "gremloader.toml:4") {
		t.Fatalf("explain: exit %d\n%s%s", code, out, errOut)
	}
	code, res := r.runJSON("explain", "app")
	var s setting
	found := false
	for i := range res.Items {
		res.item(t, i, &s)
		if s.Key == "repo" {
			found = s.Line == 4 && strings.HasSuffix(s.File, "gremloader.toml")
		}
	}
	if code != exitcode.OK || !found {
		t.Errorf("json: %+v", res)
	}
	if code, _, _ := r.run("explain", "nope"); code != exitcode.Usage {
		t.Errorf("explain of an unknown package: exit %d", code)
	}
	if code, _, _ := r.run("explain"); code != exitcode.Usage {
		t.Errorf("explain without a package: exit %d", code)
	}
}

func TestLogFilters(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	r.installed(t, "1.0.0")
	r.run("check")
	r.clock.Advance(2 * time.Hour)
	r.run("check", "app", "--now")

	code, out, errOut := r.run("log")
	if code != exitcode.OK || strings.Count(out, "run.start") != 2 || !strings.Contains(out, "check.ok app") {
		t.Fatalf("log: exit %d\n%s%s", code, out, errOut)
	}
	_, out, _ = r.run("log", "--since", "1h")
	if strings.Count(out, "run.start") != 1 {
		t.Errorf("--since 1h:\n%s", out)
	}
	_, out, _ = r.run("log", "--pkg", "app")
	if strings.Contains(out, "run.start") || !strings.Contains(out, "check.ok") {
		t.Errorf("--pkg app:\n%s", out)
	}
	code, res := r.runJSON("log", "--pkg", "app")
	if code != exitcode.OK || len(res.Items) == 0 {
		t.Errorf("json: %+v", res)
	}
	if code, _, _ := r.run("log", "--since", "yesterday-ish"); code != exitcode.Usage {
		t.Errorf("bad --since: exit %d", code)
	}
}

func TestPackageFileProblems(t *testing.T) {
	r := newRig(t, "schema_version = 1\n\n[package.app]\nrepo = \"o/app\"\nbogus = 1\n")
	code, _, errOut := r.run("list")
	if code != exitcode.Failed || !strings.Contains(errOut, "bogus") || !strings.Contains(errOut, ":5") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}

	r.env["GREMLOADER_FILE"] = filepath.Join(r.cwd, "missing.toml")
	code, res := r.runJSON("list")
	if code != exitcode.Usage || !strings.Contains(res.Error, "missing.toml") {
		t.Errorf("json: exit %d, %+v", code, res)
	}
}
