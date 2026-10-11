package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/lock"
	"github.com/enls/gremloader/internal/platform"
	"github.com/enls/gremloader/internal/testutil/archives"
)

func TestInstallThenUpdate(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))

	code, out, errOut := r.run("install", "app", "--yes")
	if code != exitcode.OK || r.appExe() != "1" || !strings.Contains(out, "app  1.0.0  installed") {
		t.Fatalf("install: exit %d\n%s%s", code, out, errOut)
	}
	r.publish("2.0.0", 5, archives.F("app.exe", "2"))
	if code, _, _ := r.run("check"); code != exitcode.UpdatesAvailable {
		t.Errorf("check: exit %d", code)
	}
	code, out, errOut = r.run("--yes", "update")
	if code != exitcode.OK || r.appExe() != "2" || !strings.Contains(out, "app  1.0.0 -> 2.0.0  updated") {
		t.Fatalf("update: exit %d\n%s%s", code, out, errOut)
	}
	code, out, _ = r.run("update", "--yes")
	if code != exitcode.OK || !strings.Contains(out, "up to date") {
		t.Errorf("second update: exit %d\n%s", code, out)
	}
	code, res := r.runJSON("list")
	var row listRow
	res.item(t, 0, &row)
	if code != exitcode.OK || row.Installed != "2.0.0" || row.Status != "up to date" {
		t.Errorf("list: %+v", row)
	}
}

func TestPlanAndApply(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	code, out, _ := r.run("plan")
	if code != exitcode.OK || !strings.Contains(out, "app  1.0.0  install") || r.appExe() != "" {
		t.Fatalf("plan: exit %d, installed %q\n%s", code, r.appExe(), out)
	}
	if code, out, errOut := r.run("apply", "--yes"); code != exitcode.OK || r.appExe() != "1" {
		t.Fatalf("apply: exit %d\n%s%s", code, out, errOut)
	}
}

// Q10: nothing changes without a confirmation. No terminal and no --yes: refuse, exit 4.
func TestNoTerminalNoChange(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	code, out, errOut := r.run("install", "app")
	if code != exitcode.Usage || !strings.Contains(out, "install") || !strings.Contains(errOut, "--yes") || r.appExe() != "" {
		t.Fatalf("exit %d, installed %q\nstdout:\n%sstderr:\n%s", code, r.appExe(), out, errOut)
	}
	code, res := r.runJSON("apply")
	if code != exitcode.Usage || !strings.Contains(res.Error, "--yes") || r.appExe() != "" {
		t.Errorf("json: %+v", res)
	}
}

func TestPromptOnATerminal(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	r.tty = true
	for _, answer := range []string{"", "n\n", "no\n", "maybe\n"} {
		r.stdin = answer
		code, _, errOut := r.run("install", "app")
		if code != exitcode.OK || !strings.Contains(errOut, "[y/N]") || !strings.Contains(errOut, "nothing was changed") || r.appExe() != "" {
			t.Fatalf("answer %q: exit %d, installed %q, stderr %q", answer, code, r.appExe(), errOut)
		}
	}
	r.stdin = "Y\n"
	if code, _, errOut := r.run("install", "app"); code != exitcode.OK || r.appExe() != "1" {
		t.Fatalf("answer y: exit %d, stderr %q", code, errOut)
	}
}

// --yes confirms the plan; it never stands in for a safety flag (plan E.4).
func TestYesIsNotASafetyFlag(t *testing.T) {
	t.Run("overlay", func(t *testing.T) {
		r := newRig(t, appFile)
		r.publish("1.0.0", 10, archives.F("app.exe", "1"))
		if err := os.MkdirAll(r.dir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.dir(), "theirs.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, _ := r.run("install", "app", "--yes")
		if code != exitcode.Failed || !strings.Contains(out, "--overlay") || r.appExe() != "" {
			t.Fatalf("without --overlay: exit %d\n%s", code, out)
		}
		if code, out, _ := r.run("install", "app", "--yes", "--overlay"); code != exitcode.OK || r.appExe() != "1" {
			t.Fatalf("with --overlay: exit %d\n%s", code, out)
		}
	})
	t.Run("downgrade", func(t *testing.T) {
		r := newRig(t, appFile)
		r.publish("1.0.0", 10, archives.F("app.exe", "1"))
		r.publish("2.0.0", 5, archives.F("app.exe", "2"))
		r.run("install", "app", "--yes")
		// The engine reports a refused downgrade as "skipped", exit 0 (open question in the handoff:
		// a script asking for a version it does not get may want exit 1).
		code, out, _ := r.run("update", "app", "--version", "1.0.0", "--yes")
		if r.appExe() != "2" || code != exitcode.OK || !strings.Contains(out, "skipped") || !strings.Contains(out, "--allow-downgrade") {
			t.Fatalf("without --allow-downgrade: exit %d, app %q\n%s", code, r.appExe(), out)
		}
		if code, out, _ := r.run("update", "app", "--version", "1.0.0", "--yes", "--allow-downgrade"); code != exitcode.OK || r.appExe() != "1" {
			t.Fatalf("with --allow-downgrade: exit %d\n%s", code, out)
		}
	})
	t.Run("hash", func(t *testing.T) {
		r := newRig(t, appFile)
		r.publish("1.0.0", 10, archives.F("app.exe", "1"))
		r.run("install", "app", "--yes")
		received := r.publishBadSums("3.0.0")

		if code, out, _ := r.run("update", "--yes"); code != exitcode.VerifyFailed || r.appExe() != "1" || !strings.Contains(out, received) {
			t.Fatalf("mismatch: exit %d\n%s", code, out)
		}
		wrong := strings.Repeat("cd", 32)
		if code, _, _ := r.run("update", "app", "--version", "3.0.0", "--accept-sha256", wrong, "--yes"); code != exitcode.VerifyFailed || r.appExe() != "1" {
			t.Errorf("wrong hash accepted: exit %d", code)
		}
		for _, line := range [][]string{
			{"apply", "--accept-sha256", received, "--yes"},
			{"update", "app", "--accept-sha256", received, "--yes"},
			{"install", "app", "--accept-sha256", received, "--yes"},
			{"update", "--version", "3.0.0", "--accept-sha256", received, "--yes"},
			{"update", "app", "--version", "3.0.0", "--accept-sha256", "xyz", "--yes"},
		} {
			if code, _, _ := r.run(line...); code != exitcode.Usage || r.appExe() != "1" {
				t.Errorf("%q: exit %d", line, code)
			}
		}
		if _, _, errOut := r.run("install", "app", "--accept-sha256", received, "--yes"); !strings.Contains(errOut, "needs the exact version") {
			t.Errorf("no version: %q", errOut)
		}
		code, out, errOut := r.run("update", "app", "--version", "3.0.0", "--accept-sha256", strings.ToUpper(received), "--yes")
		if code != exitcode.OK || r.appExe() != "app 3.0.0" {
			t.Fatalf("accepted: exit %d\n%s%s", code, out, errOut)
		}
		if _, err := os.Stat(r.lockFile()); err == nil {
			t.Error("an override created a lock file")
		}
	})
}

func TestUpdateLockAndLocked(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	if code, out, errOut := r.run("install", "app", "--yes", "--update-lock"); code != exitcode.OK {
		t.Fatalf("install: exit %d\n%s%s", code, out, errOut)
	}
	lf, found, err := lock.Load(r.lockFile())
	if err != nil || !found || lf.Packages["app"] == nil || lf.Packages["app"].Version != "1.0.0" ||
		lf.Packages["app"].Platforms[platform.Current().String()] == nil {
		t.Fatalf("lock = %+v, %v, %v", lf, found, err)
	}

	// A newer release does not move a locked apply.
	r.publish("2.0.0", 5, archives.F("app.exe", "2"))
	if code, out, _ := r.run("apply", "--locked", "--yes"); code != exitcode.OK || r.appExe() != "1" {
		t.Errorf("locked apply: exit %d, app %q\n%s", code, r.appExe(), out)
	}
	if code, _, _ := r.run("update", "--yes", "--update-lock"); code != exitcode.OK || r.appExe() != "2" {
		t.Fatalf("update: exit %d", code)
	}
	if lf, _, _ := lock.Load(r.lockFile()); lf.Packages["app"].Version != "2.0.0" {
		t.Errorf("lock after update = %+v", lf.Packages["app"])
	}
	if code, _, _ := r.run("apply", "--locked", "--update-lock", "--yes"); code != exitcode.Usage {
		t.Errorf("--locked --update-lock: exit %d", code)
	}
	if err := os.Remove(r.lockFile()); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := r.run("apply", "--locked", "--yes"); code != exitcode.Usage || !strings.Contains(errOut, "gremloader lock") {
		t.Errorf("--locked without a lock: exit %d, %q", code, errOut)
	}
}

// An accepted hash goes into a lock that names that package at that version, marked.
func TestOverrideIsRecordedInTheLock(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	r.run("install", "app", "--yes", "--update-lock")
	received := r.publishBadSums("3.0.0")

	// Lock 3.0.0 by hand, as a lock made where the release looked different would.
	lf, _, _ := lock.Load(r.lockFile())
	key := platform.Current().String()
	lp := lf.Packages["app"]
	lp.Version, lp.Tag = "3.0.0", "v3.0.0"
	a := lp.Platforms[key]
	a.Asset, a.SHA256 = "app-3.0.0.zip", strings.Repeat("ab", 32)
	a.URL = strings.ReplaceAll(a.URL, "1.0.0", "3.0.0")
	if err := lock.Save(r.lockFile(), lf); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := r.run("update", "app", "--version", "3.0.0", "--accept-sha256", received, "--yes")
	if code != exitcode.OK || !strings.Contains(out, "verified_by") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	lf, _, _ = lock.Load(r.lockFile())
	if got := lf.Packages["app"].Platforms[key]; got.SHA256 != received || got.VerifiedBy != lock.HashOverride {
		t.Errorf("lock entry = %+v", got)
	}
}
