package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/journal"
	"github.com/enls/gremloader/internal/testutil/archives"
)

// The test binary doubles as a hook: with GLHOOKTEST=1 it appends what it was told to a file.
func TestMain(m *testing.M) {
	if os.Getenv("GLHOOKTEST") == "1" {
		os.Exit(hookHelper(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func hookHelper(args []string) int {
	if len(args) != 2 || args[0] != "record" {
		return 2
	}
	f, err := os.OpenFile(args[1], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 3
	}
	defer func() { _ = f.Close() }()
	cwd, _ := os.Getwd()
	_, _ = fmt.Fprintf(f, "%s %s %s<-%s %s\n", os.Getenv("GREMLOADER_ACTION"), os.Getenv("GREMLOADER_PACKAGE"),
		os.Getenv("GREMLOADER_VERSION"), os.Getenv("GREMLOADER_PREVIOUS"), filepath.Base(cwd))
	return 0
}

// The whole life of a package through the real command line, with the real hook runner running
// a real program for every hook (plan E.5, M6: hooks end to end).
func TestLifeOfAPackageWithHooks(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GLHOOKTEST", "1") // inherited by the hooks only: this process checked it at start
	logFile := filepath.Join(t.TempDir(), "hooks.log")
	hook := fmt.Sprintf("[['%s', 'record', '%s']]", self, logFile)
	r := newRig(t, appFile+"post_install = "+hook+"\npost_update = "+hook+"\npost_remove = "+hook+"\n")

	step := func(want int, args ...string) {
		t.Helper()
		if code, out, errOut := r.run(args...); code != want {
			t.Fatalf("%q: exit %d, want %d\n%s%s", args, code, want, out, errOut)
		}
	}
	r.publish("1.0.0", 30, archives.F("app.exe", "1"))
	step(exitcode.OK, "install", "app", "--yes")
	r.publish("2.0.0", 20, archives.F("app.exe", "2"))
	step(exitcode.OK, "update", "--yes")
	step(exitcode.OK, "rollback", "app", "--yes")
	step(exitcode.OK, "update", "--yes") // 2.0.0 was rolled away from: nothing to do
	if r.appExe() != "1" {
		t.Fatalf("after rollback and update: app %q, want 1 (2.0.0 is rejected)", r.appExe())
	}
	r.publish("3.0.0", 10, archives.F("app.exe", "3"))
	step(exitcode.OK, "update", "--yes")
	step(exitcode.OK, "remove", "app", "--yes")
	if r.appExe() != "" {
		t.Errorf("app.exe still there after remove")
	}

	got, _ := os.ReadFile(logFile)
	want := "install app 1.0.0<- App\n" +
		"update app 2.0.0<-1.0.0 App\n" +
		"rollback app 1.0.0<-2.0.0 App\n" +
		"update app 3.0.0<-1.0.0 App\n" +
		"remove app 3.0.0<-3.0.0 App\n" // the emptied install folder stays: it is not in the manifest
	if string(got) != want {
		t.Errorf("hooks ran:\n%s\nwant:\n%s", got, want)
	}
}

// Exit 6: an interrupted change blocks its package until repair.
func TestNeedsRepairIsExit6(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	r.run("install", "app", "--yes")
	j, err := journal.Create(filepath.Join(r.stateDir(), "journal", "app"), journal.Record{Op: journal.OpBegin, ID: "x",
		Pkg: "app", Action: "update", From: "1.0.0", To: "2.0.0", Dir: r.dir(), TS: r.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	r.publish("2.0.0", 5, archives.F("app.exe", "2"))
	code, out, _ := r.run("update", "--yes")
	if code != exitcode.NeedsRepair || !strings.Contains(out, "gremloader repair") || r.appExe() != "1" {
		t.Fatalf("update: exit %d\n%s", code, out)
	}
	if _, out, _ := r.run("list"); !strings.Contains(out, "needs repair") {
		t.Errorf("list:\n%s", out)
	}
	if code, out, errOut := r.run("repair"); code != exitcode.OK {
		t.Fatalf("repair: exit %d\n%s%s", code, out, errOut)
	}
	if code, _, _ := r.run("update", "--yes"); code != exitcode.OK || r.appExe() != "2" {
		t.Errorf("update after repair: exit %d, app %q", code, r.appExe())
	}
}

// Exit 7: nothing could be resolved because GitHub could not be reached.
func TestOfflineIsExit7(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	r.run("install", "app", "--yes")
	d := r.deps()
	d.apiBase, d.webBase = "http://127.0.0.1:1", "http://127.0.0.1:1" // nothing listens on port 1
	var out, errb strings.Builder
	code := runWith([]string{"check", "--root", r.root}, IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb}, d)
	if code != exitcode.Offline {
		t.Errorf("exit %d, want 7\n%s%s", code, out.String(), errb.String())
	}

	// A disabled package was not resolved either: it does not turn offline into an ordinary failure.
	r.writeFile(appFile + "\n[package.other]\nrepo = \"o/other\"\nasset = \"other.zip\"\nverify = { require = [] }\ndir = \"{root}/Other\"\nenabled = false\n")
	out.Reset()
	errb.Reset()
	code = runWith([]string{"check", "--root", r.root}, IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb}, d)
	if code != exitcode.Offline {
		t.Errorf("with a disabled package: exit %d, want 7\n%s%s", code, out.String(), errb.String())
	}
}
