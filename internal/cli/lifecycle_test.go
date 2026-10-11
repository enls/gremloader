package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/lock"
	"github.com/enls/gremloader/internal/testutil/archives"
)

// atV2 installs 1.0.0 and updates to 2.0.0.
func atV2(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	r.run("install", "app", "--yes")
	r.publish("2.0.0", 5, archives.F("app.exe", "2"))
	if code, out, errOut := r.run("update", "--yes"); code != exitcode.OK || r.appExe() != "2" {
		t.Fatalf("update: exit %d\n%s%s", code, out, errOut)
	}
	return r
}

func TestRemoveIsConfirmed(t *testing.T) {
	r := atV2(t)
	if code, out, _ := r.run("remove", "app"); code != exitcode.Usage || r.appExe() != "2" || !strings.Contains(out, "remove") {
		t.Fatalf("unconfirmed remove: exit %d, app %q\n%s", code, r.appExe(), out)
	}
	code, out, errOut := r.run("remove", "app", "--yes")
	if code != exitcode.OK || r.appExe() != "" || !strings.Contains(out, "removed") {
		t.Fatalf("remove: exit %d\n%s%s", code, out, errOut)
	}
	// Not installed: the engine says so, nothing to confirm.
	if code, out, _ := r.run("remove", "app"); code != exitcode.Failed {
		t.Errorf("remove again: exit %d\n%s", code, out)
	}
}

// A package neither in the package file nor installed is unknown: exit 4 (ADR 0013), as for verify,
// install and the rest.
func TestUnknownPackageIsUsage(t *testing.T) {
	r := atV2(t)
	for _, line := range [][]string{{"remove", "nope", "--yes"}, {"rollback", "nope", "--yes"}, {"repair", "--forget", "nope", "--yes"}} {
		if code, out, errOut := r.run(line...); code != exitcode.Usage || !strings.Contains(out+errOut, "nope") {
			t.Errorf("%q: exit %d, want 4\n%s%s", line, code, out, errOut)
		}
	}
}

func TestRollback(t *testing.T) {
	r := atV2(t)
	if code, _, _ := r.run("rollback", "app"); code != exitcode.Usage || r.appExe() != "2" {
		t.Fatalf("unconfirmed rollback: exit %d", code)
	}
	code, out, errOut := r.run("rollback", "app", "--yes")
	if code != exitcode.OK || r.appExe() != "1" || !strings.Contains(out, "2.0.0 -> 1.0.0  rolled-back") {
		t.Fatalf("rollback: exit %d\n%s%s", code, out, errOut)
	}
	// 2.0.0 is now rejected: check does not offer it again.
	if code, out, _ := r.run("check"); code != exitcode.OK {
		t.Errorf("check after rollback: exit %d\n%s", code, out)
	}
}

func TestVerify(t *testing.T) {
	r := atV2(t)
	if code, out, _ := r.run("verify"); code != exitcode.OK || !strings.Contains(out, "app 2.0.0: ok, 1 files") {
		t.Fatalf("verify: exit %d\n%s", code, out)
	}
	if err := os.WriteFile(filepath.Join(r.dir(), "app.exe"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := r.run("verify", "app")
	if code != exitcode.Failed || !strings.Contains(out, "changed:  app.exe") {
		t.Errorf("verify a changed file: exit %d\n%s", code, out)
	}
	code, res := r.runJSON("verify")
	var row checkRow
	res.item(t, 0, &row)
	if code != exitcode.Failed || row.OK || len(row.Modified) != 1 {
		t.Errorf("json: %+v", row)
	}
	// --remote compares with the release itself.
	code, out, _ = r.run("verify", "--remote")
	if code != exitcode.Failed || !strings.Contains(out, "app.exe") {
		t.Errorf("verify --remote: exit %d\n%s", code, out)
	}
	if code, _, _ := r.run("verify", "nope"); code != exitcode.Usage {
		t.Errorf("verify nope: exit %d", code)
	}
}

// verify --remote keeps the run's exit codes for the download itself (plan E.4): one that does not
// match is 5, GitHub out of reach is 7.
func TestVerifyRemoteExitCodes(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	r.run("install", "app", "--yes")
	d := r.deps()
	d.apiBase, d.webBase = "http://127.0.0.1:1", "http://127.0.0.1:1" // nothing listens on port 1
	var out, errb strings.Builder
	if code := runWith([]string{"verify", "--remote", "--root", r.root}, IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb}, d); code != exitcode.Offline {
		t.Errorf("offline: exit %d, want 7\n%s%s", code, out.String(), errb.String())
	}

	received := r.publishBadSums("3.0.0")
	if code, out, errOut := r.run("install", "app@3.0.0", "--yes", "--accept-sha256", received); code != exitcode.OK {
		t.Fatalf("install 3.0.0: exit %d\n%s%s", code, out, errOut)
	}
	if code, out, errOut := r.run("verify", "--remote"); code != exitcode.VerifyFailed {
		t.Errorf("a download that does not match: exit %d, want 5\n%s%s", code, out, errOut)
	}
}

// The command verify --remote suggests for a mismatched download of the installed version does
// what it says: it reinstalls that version (which needs --force), not a no-op.
func TestVerifyRemoteHintReinstalls(t *testing.T) {
	r := newRig(t, appFile)
	received := r.publishBadSums("3.0.0")
	if code, out, errOut := r.run("install", "app@3.0.0", "--yes", "--accept-sha256", received); code != exitcode.OK {
		t.Fatalf("install 3.0.0: exit %d\n%s%s", code, out, errOut)
	}
	if err := os.WriteFile(filepath.Join(r.dir(), "app.exe"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, errOut := r.run("verify", "--remote")
	_, hint, ok := strings.Cut(out+errOut, "`gremloader ")
	hint, _, _ = strings.Cut(hint, "`")
	if !ok || !strings.HasPrefix(hint, "install app@3.0.0 ") {
		t.Fatalf("no install hint\n%s%s", out, errOut)
	}
	if code, out, errOut := r.run(append(strings.Fields(hint), "--yes")...); code != exitcode.OK || r.appExe() != "app 3.0.0" {
		t.Errorf("gremloader %s: exit %d, app.exe %q\n%s%s", hint, code, r.appExe(), out, errOut)
	}
}

func TestAdopt(t *testing.T) {
	for _, line := range [][]string{{"adopt", "app", "--version", "1.0.0"}, {"install", "app@1.0.0", "--adopt"}} {
		t.Run(line[0], func(t *testing.T) {
			r := newRig(t, appFile)
			r.publish("1.0.0", 10, archives.F("app.exe", "1"))
			if err := os.MkdirAll(r.dir(), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(r.dir(), "app.exe"), []byte("1"), 0o600); err != nil {
				t.Fatal(err)
			}
			code, out, errOut := r.run(line...)
			if code != exitcode.OK || !strings.Contains(out, "adopted") {
				t.Fatalf("exit %d\n%s%s", code, out, errOut)
			}
			_, res := r.runJSON("list")
			var row listRow
			res.item(t, 0, &row)
			if row.Installed != "1.0.0" {
				t.Errorf("list: %+v", row)
			}
			if code, _, _ := r.run("adopt", "nope", "--version", "1.0.0"); code != exitcode.Usage {
				t.Errorf("adopt nope: exit %d", code)
			}
		})
	}
}

func TestRepairAndForget(t *testing.T) {
	r := newRig(t, appFile)
	if code, out, _ := r.run("repair"); code != exitcode.OK || !strings.Contains(out, "nothing to repair") {
		t.Errorf("repair with nothing to do: exit %d\n%s", code, out)
	}
	// A journal folder that never got its first record (a crash at its very start).
	jdir := filepath.Join(r.stateDir(), "journal", "app")
	if err := os.MkdirAll(jdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := r.run("repair"); code != exitcode.OK || !strings.Contains(out, "repaired") {
		t.Errorf("repair: exit %d\n%s", code, out)
	}

	if err := os.MkdirAll(jdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := r.run("repair", "--forget", "app"); code != exitcode.Usage {
		t.Errorf("unconfirmed forget: exit %d", code)
	}
	if _, err := os.Stat(jdir); err != nil {
		t.Fatal("an unconfirmed forget deleted the journal")
	}
	code, out, errOut := r.run("repair", "--forget", "app", "--yes")
	if code != exitcode.OK || !strings.Contains(out, "forgotten") {
		t.Errorf("forget: exit %d\n%s%s", code, out, errOut)
	}
	if _, err := os.Stat(jdir); err == nil {
		t.Error("the journal is still there")
	}
	if code, _, _ := r.run("repair", "--forget", "app", "--yes"); code != exitcode.Failed {
		t.Errorf("forget with nothing to forget: exit %d", code)
	}
}

func TestPinAndHold(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	r.run("install", "app", "--yes")
	r.publish("2.0.0", 5, archives.F("app.exe", "2"))

	if code, out, _ := r.run("pin", "app", "1.0.0"); code != exitcode.OK || !strings.Contains(out, "pinned to 1.0.0") {
		t.Fatalf("pin: exit %d\n%s", code, out)
	}
	local, _ := os.ReadFile(filepath.Join(r.home, "config", "gremloader.local.toml"))
	if !strings.Contains(string(local), `pin = '1.0.0'`) && !strings.Contains(string(local), `pin = "1.0.0"`) {
		t.Errorf("local file:\n%s", local)
	}
	if code, out, _ := r.run("check"); code != exitcode.OK {
		t.Errorf("check while pinned: exit %d\n%s", code, out)
	}
	if _, out, _ := r.run("list"); !strings.Contains(out, "pinned to 1.0.0") {
		t.Errorf("list while pinned:\n%s", out)
	}
	r.run("unpin", "app")
	if code, _, _ := r.run("check"); code != exitcode.UpdatesAvailable {
		t.Errorf("check after unpin: exit %d", code)
	}

	r.run("hold", "app")
	if code, out, _ := r.run("update", "--yes"); code != exitcode.OK || r.appExe() != "1" || !strings.Contains(out, "held") {
		t.Errorf("update while held: exit %d\n%s", code, out)
	}
	if code, _, _ := r.run("update", "--yes", "--include-held"); code != exitcode.OK || r.appExe() != "2" {
		t.Errorf("update --include-held: exit %d", code)
	}
	r.run("unhold", "app")
	if local, _ := os.ReadFile(filepath.Join(r.home, "config", "gremloader.local.toml")); strings.Contains(string(local), "[package.app]") {
		t.Errorf("an empty entry was kept:\n%s", local)
	}

	for _, line := range [][]string{{"pin", "app", "latest"}, {"pin", "nope", "1.0.0"}, {"pin", "app"}, {"hold"}} {
		if code, _, _ := r.run(line...); code != exitcode.Usage {
			t.Errorf("%q: exit %d", line, code)
		}
	}
}

func TestLockCommand(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	code, out, errOut := r.run("lock")
	if code != exitcode.OK || !strings.Contains(out, "locked") {
		t.Fatalf("lock: exit %d\n%s%s", code, out, errOut)
	}
	lf, found, err := lock.Load(r.lockFile())
	if err != nil || !found || lf.Packages["app"].Version != "1.0.0" || !strings.HasPrefix(lf.GeneratedBy, "gremloader ") {
		t.Fatalf("lock file: %+v, %v", lf, err)
	}

	// A package the file no longer names goes when the whole file is locked, not when one
	// package is.
	lf.Packages["old"] = lf.Packages["app"]
	if err := lock.Save(r.lockFile(), lf); err != nil {
		t.Fatal(err)
	}
	r.publish("2.0.0", 5, archives.F("app.exe", "2"))
	r.run("lock", "app")
	if lf, _, _ := lock.Load(r.lockFile()); lf.Packages["old"] == nil || lf.Packages["app"].Version != "2.0.0" {
		t.Errorf("lock app: %+v", lf.Packages)
	}
	r.run("lock")
	if lf, _, _ := lock.Load(r.lockFile()); lf.Packages["old"] != nil {
		t.Errorf("lock: the stale entry is still there")
	}
	if code, _, _ := r.run("lock", "nope"); code != exitcode.Usage {
		t.Errorf("lock nope: exit %d", code)
	}
}
