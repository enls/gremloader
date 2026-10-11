package engine

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/journal"
	"github.com/enls/gremloader/internal/proc"
	"github.com/enls/gremloader/internal/state"
	"github.com/enls/gremloader/internal/testutil/archives"
)

// The two versions used by the update tests. v2 replaces a file, keeps one identical, adds a file
// in a new folder, drops a file and with it a folder that becomes empty.
var (
	v1Files = []archives.Entry{
		archives.X("app.exe", "app v1"),
		archives.F("lib/old.dll", "old lib"),
		archives.F("data/readme.txt", "readme"),
		archives.F("olddir/x.txt", "x"),
		archives.F("olddir/sub/z.txt", "z"),
		archives.D("empty/"),
		archives.D("keep-empty/"),
	}
	v2Files = []archives.Entry{
		archives.X("app.exe", "app v2"),
		archives.F("lib/new.dll", "new lib"),
		archives.F("data/readme.txt", "readme"),
		archives.F("newdir/deep/y.txt", "y"),
		archives.D("keep-empty/"),
	}
	// What the install folder holds after each version, with the user's own files beside them.
	v1Tree = map[string]string{
		"app.exe": "app v1", "lib/": "", "lib/old.dll": "old lib", "data/": "", "data/readme.txt": "readme",
		"olddir/": "", "olddir/x.txt": "x", "olddir/sub/": "", "olddir/sub/z.txt": "z",
		"empty/": "", "keep-empty/": "", "user.cfg": "mine", "data/my-notes.txt": "notes",
	}
	v2Tree = map[string]string{
		"app.exe": "app v2", "lib/": "", "lib/new.dll": "new lib", "data/": "", "data/readme.txt": "readme",
		"newdir/": "", "newdir/deep/": "", "newdir/deep/y.txt": "y", "keep-empty/": "", "user.cfg": "mine", "data/my-notes.txt": "notes",
	}
)

func (r *rig) dir() string { return filepath.Join(r.root, "App") }

// apply plans and applies in one engine, as one run does.
func (r *rig) apply(e *Engine, rq Request) *Plan {
	r.t.Helper()
	p := r.plan(e, rq)
	if err := e.Apply(context.Background(), p, ApplyOptions{}); err != nil {
		r.t.Fatalf("Apply: %v", err)
	}
	return p
}

// atV1 publishes v1 and v2, installs v1 and adds the user's files: the starting point of an update.
func atV1(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, appFile)
	r.publish("app", "1.0.0", 30, v1Files...)
	it := item(t, r.apply(r.engine(), Request{InstallMissing: true}), "app")
	if it.Result != ResultInstalled {
		t.Fatalf("installing v1: %+v", it)
	}
	for p, c := range map[string]string{"user.cfg": "mine", "data/my-notes.txt": "notes"} {
		if err := os.WriteFile(filepath.Join(r.dir(), filepath.FromSlash(p)), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.publish("app", "2.0.0", 20, v2Files...)
	return r
}

func assertTree(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	if got := tree(t, dir); !maps.Equal(got, want) {
		t.Fatalf("install folder:\n%swant:\n%s", show(got), show(want))
	}
}

// assertClean checks that no journal or staging is left behind.
func (r *rig) assertClean() {
	r.t.Helper()
	if journal.Exists(r.paths.Journal("app")) {
		r.t.Error("a journal was left behind")
	}
	if _, err := os.Stat(r.paths.Staging("app")); err == nil {
		r.t.Error("staging was left behind")
	}
}

func TestInstall(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("app", "1.0.0", 10, v1Files...)
	p := r.apply(r.engine(), Request{InstallMissing: true})
	it := item(t, p, "app")
	if it.Result != ResultInstalled || it.Err != nil {
		t.Fatalf("item = %+v", it)
	}
	want := maps.Clone(v1Tree)
	delete(want, "user.cfg")
	delete(want, "data/my-notes.txt")
	assertTree(t, r.dir(), want)
	r.assertClean()

	rec := r.state().Packages["app"]
	if rec.Version != "1.0.0" || rec.Tag != "v1.0.0" || rec.Dir != r.dir() || rec.Asset != "app-1.0.0.zip" ||
		rec.Previous != nil || rec.Adopted || len(rec.VerifiedBy) != 1 || rec.VerifiedBy[0] != "checksum" || rec.InstalledAt == nil {
		t.Errorf("state = %+v", rec)
	}
	m, err := state.LoadManifest(state.Path(r.paths.State, rec.Manifest))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 5 || m.Files[0].Path != "app.exe" || !m.Files[0].Exec ||
		strings.Join(m.Dirs, ",") != "data,empty,keep-empty,lib,olddir,olddir/sub" {
		t.Errorf("manifest = %+v", m)
	}
	// Nothing to roll back to after a first install.
	if _, err := os.Stat(r.paths.Previous("app")); err == nil {
		t.Error("previous/ exists after a fresh install")
	}
	// A second run has nothing to do.
	if it := item(t, r.plan(r.engine(), Request{InstallMissing: true}), "app"); it.Action != ActNone || it.Result != ResultUnchanged {
		t.Errorf("second plan = %+v", it)
	}
}

func TestUpdate(t *testing.T) {
	r := atV1(t)
	assertTree(t, r.dir(), v1Tree)
	it := item(t, r.apply(r.engine(), Request{}), "app")
	if it.Result != ResultUpdated || it.From != "1.0.0" || it.To != "2.0.0" {
		t.Fatalf("item = %+v", it)
	}
	assertTree(t, r.dir(), v2Tree)
	r.assertClean()

	rec := r.state().Packages["app"]
	if rec.Version != "2.0.0" || rec.Previous == nil || rec.Previous.Version != "1.0.0" ||
		rec.Previous.Undo != "previous/app/undo.jsonl" || rec.Previous.Manifest != state.ManifestRel("app", "1.0.0") {
		t.Fatalf("state = %+v / %+v", rec, rec.Previous)
	}
	// The rollback record holds the replaced and removed files, by hash.
	l, err := journal.Read(r.paths.Previous("app"))
	if err != nil {
		t.Fatal(err)
	}
	saved := map[string]string{}
	for _, rr := range l.Records {
		if rr.Op == journal.OpBackup || rr.Op == journal.OpRemoved {
			b, err := os.ReadFile(l.BackupPath(rr))
			if err != nil {
				t.Fatal(err)
			}
			saved[rr.Op+" "+rr.Path] = string(b)
		}
	}
	wantSaved := map[string]string{
		"backup app.exe": "app v1", "backup data/readme.txt": "readme",
		"removed lib/old.dll": "old lib", "removed olddir/x.txt": "x", "removed olddir/sub/z.txt": "z",
	}
	if !maps.Equal(saved, wantSaved) {
		t.Errorf("saved = %v, want %v", saved, wantSaved)
	}
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if _, err := os.Stat(state.Path(r.paths.State, state.ManifestRel("app", v))); err != nil {
			t.Errorf("manifest of %s: %v", v, err)
		}
	}
}

// A third version replaces the rollback record and prunes the manifest nobody points at.
func TestSecondUpdateReplacesThePreviousRecord(t *testing.T) {
	r := atV1(t)
	r.apply(r.engine(), Request{})
	r.publish("app", "3.0.0", 5, archives.X("app.exe", "app v3"))
	it := item(t, r.apply(r.engine(), Request{}), "app")
	if it.Result != ResultUpdated {
		t.Fatalf("item = %+v", it)
	}
	if rec := r.state().Packages["app"]; rec.Previous.Version != "2.0.0" {
		t.Errorf("previous = %+v", rec.Previous)
	}
	if _, err := os.Stat(state.Path(r.paths.State, state.ManifestRel("app", "1.0.0"))); err == nil {
		t.Error("the manifest of 1.0.0 was kept")
	}
	if _, err := os.Stat(r.paths.Previous("app") + ".old"); err == nil {
		t.Error("previous.old left behind")
	}
}

func TestRunningAppIsLeftAlone(t *testing.T) {
	r := atV1(t)
	r.running = []proc.Process{{PID: 4242, Path: filepath.Join(r.dir(), "app.exe")}}
	it := item(t, r.apply(r.engine(), Request{}), "app")
	if it.Result != ResultSkipped || !strings.Contains(it.Reason, "app.exe (pid 4242)") {
		t.Fatalf("item = %+v", it)
	}
	assertTree(t, r.dir(), v1Tree)
	r.assertClean()
}

func TestHashMismatchInstallsNothing(t *testing.T) {
	r := atV1(t)
	// The checksum file of 3.0.0 names a hash that is not the asset's.
	r.srv.AddRelease("o", "app", fakeRelease("v3.0.0", "app-3.0.0.zip", archives.Zip(archives.F("app-3.0.0/app.exe", "evil")),
		strings.Repeat("ab", 32)+"  app-3.0.0.zip\n", r.clock.Now()))
	p := r.apply(r.engine(), Request{})
	it := item(t, p, "app")
	var ve *VerifyError
	if it.Result != ResultFailed || !errors.As(it.Err, &ve) || !strings.Contains(ve.Msg, "DIFFERS") {
		t.Fatalf("item = %+v", it)
	}
	if got := p.ExitCode(false); got != exitcode.VerifyFailed {
		t.Errorf("exit = %d, want 5", got)
	}
	assertTree(t, r.dir(), v1Tree)
	r.assertClean()
	if rec := r.state().Packages["app"]; rec.Version != "1.0.0" {
		t.Errorf("state = %+v", rec)
	}
}

// An archive with no files must never replace a working install (plan E.9).
func TestEmptyArchiveInstallsNothing(t *testing.T) {
	r := atV1(t)
	r.srv.AddRelease("o", "app", fakeRelease("v3.0.0", "app-3.0.0.zip", archives.Zip(), "", r.clock.Now()))
	it := item(t, r.apply(r.engine(), Request{}), "app")
	if it.Result != ResultFailed || !strings.Contains(it.Err.Error(), "no files") {
		t.Fatalf("item = %+v", it)
	}
	assertTree(t, r.dir(), v1Tree)
	r.assertClean()
}

func TestOverlayInstallKeepsWhatItReplaced(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("app", "1.0.0", 10, archives.F("app.exe", "new"), archives.F("other.txt", "o"))
	if err := os.MkdirAll(r.dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir(), "app.exe"), []byte("someone else's"), 0o600); err != nil {
		t.Fatal(err)
	}
	it := item(t, r.apply(r.engine(), Request{InstallMissing: true, Overlay: true}), "app")
	if it.Result != ResultInstalled {
		t.Fatalf("item = %+v", it)
	}
	l, err := journal.Read(r.paths.Previous("app"))
	if err != nil {
		t.Fatalf("the replaced file was not kept: %v", err)
	}
	found := false
	for _, rr := range l.Records {
		if rr.Op == journal.OpBackup && rr.Path == "app.exe" {
			b, _ := os.ReadFile(l.BackupPath(rr))
			found = string(b) == "someone else's"
		}
	}
	if !found {
		t.Error("no backup of the overwritten app.exe")
	}
	if r.state().Packages["app"].Previous != nil {
		t.Error("an overlay install is not something to roll back to")
	}
}

func TestHooksRunAfterInstallAndUpdate(t *testing.T) {
	r := newRig(t, appFile+`post_install = [["register", "{name}"]]
post_update = [["register", "{name}", "again"]]
`)
	r.publish("app", "1.0.0", 30, v1Files...)
	r.apply(r.engine(), Request{InstallMissing: true})
	r.publish("app", "2.0.0", 20, v2Files...)
	r.apply(r.engine(), Request{})
	if len(r.hooks.ran) != 2 {
		t.Fatalf("hooks = %+v", r.hooks.ran)
	}
	h := r.hooks.ran[1]
	if h.Event != "post_update" || h.Action != "update" || h.Version != "2.0.0" || h.Previous != "1.0.0" ||
		h.Dir != r.dir() || strings.Join(h.Commands[0], " ") != "register app again" {
		t.Errorf("hook = %+v", h)
	}
}

func TestHookFailure(t *testing.T) {
	for _, mode := range []string{"warn", "fail"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t, appFile+"post_install = [[\"x\"]]\nhook_fail = \""+mode+"\"\n")
			r.publish("app", "1.0.0", 30, v1Files...)
			r.hooks.fail = map[string]error{"post_install": errors.New("exit status 3")}
			it := item(t, r.apply(r.engine(), Request{InstallMissing: true}), "app")
			// The files stay installed either way; only the reported result differs.
			if r.state().Packages["app"].Version != "1.0.0" {
				t.Error("not installed")
			}
			if mode == "warn" && (it.Result != ResultInstalled || len(it.Notes) == 0) {
				t.Errorf("item = %+v", it)
			}
			if mode == "fail" && (it.Result != ResultFailed || !strings.Contains(it.Err.Error(), "installed, but")) {
				t.Errorf("item = %+v", it)
			}
		})
	}
}

// A failed post_update hook under hook_fail = "fail" says what was done: updated, not installed.
func TestFailedPostUpdateHookSaysUpdated(t *testing.T) {
	r := newRig(t, appFile+"post_update = [[\"x\"]]\nhook_fail = \"fail\"\n")
	r.publish("app", "1.0.0", 30, v1Files...)
	item(t, r.apply(r.engine(), Request{InstallMissing: true}), "app")
	r.publish("app", "2.0.0", 20, v2Files...)
	r.hooks.fail = map[string]error{"post_update": errors.New("exit status 3")}
	it := item(t, r.apply(r.engine(), Request{}), "app")
	if it.Result != ResultFailed || !strings.Contains(it.Err.Error(), "updated, but the post_update hook failed") {
		t.Errorf("item = %+v (%v)", it, it.Err)
	}
}

// ---- failures and crashes at every step ----

// countPoints runs an update with a fault function that only counts, and returns how many
// fault points it passed.
func countPoints(t *testing.T) int {
	t.Helper()
	r := atV1(t)
	e := r.engine()
	n := 0
	e.fault = func(string) error { n++; return nil }
	p := r.plan(e, Request{})
	if err := e.Apply(context.Background(), p, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if it := item(t, p, "app"); it.Result != ResultUpdated {
		t.Fatalf("counting run: %+v", it)
	}
	return n
}

// Any failure before the commit leaves the old version exactly as it was, with nothing left over.
func TestFailureAtEveryPointLeavesTheOldVersion(t *testing.T) {
	total := countPoints(t)
	if total < 20 {
		t.Fatalf("only %d fault points; the update should pass many", total)
	}
	for k := 1; k <= total; k++ {
		r := atV1(t)
		e := r.engine()
		n, where := 0, ""
		e.fault = func(point string) error {
			n++
			if n == k {
				where = point
				return errors.New("injected failure")
			}
			return nil
		}
		p := r.plan(e, Request{})
		if err := e.Apply(context.Background(), p, ApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		it := item(t, p, "app")
		switch where {
		case "promote", "hook":
			// After the commit: the update stands.
			if it.Result != ResultUpdated {
				t.Errorf("fault %d (%s): %+v", k, where, it)
			}
			assertTree(t, r.dir(), v2Tree)
		default:
			if it.Result != ResultFailed && (where != "commit" || it.Result != ResultNeedsRepair) {
				t.Errorf("fault %d (%s): result %s, err %v", k, where, it.Result, it.Err)
			}
			if where == "commit" {
				// The files and state.json were written; the journal finishes forward.
				for _, ri := range r.engine().Repair(context.Background()) {
					if ri.Result != ResultRepaired {
						t.Errorf("fault %d (%s): repair %+v", k, where, ri)
					}
				}
				assertTree(t, r.dir(), v2Tree)
				if v := r.state().Packages["app"].Version; v != "2.0.0" {
					t.Errorf("fault %d (%s): state says %s after repair", k, where, v)
				}
				r.assertClean()
				continue
			}
			assertTree(t, r.dir(), v1Tree)
			if v := r.state().Packages["app"].Version; v != "1.0.0" {
				t.Errorf("fault %d (%s): state says %s", k, where, v)
			}
		}
		r.assertClean()
	}
}

// The process can die at any point. Whatever the point, repair leaves exactly the old version or
// exactly the new one, state.json agrees with the files, and the next run is not blocked.
func TestCrashAtEveryPointIsRepaired(t *testing.T) {
	total := countPoints(t)
	sawOld, sawNew := false, false
	for k := 1; k <= total; k++ {
		r := atV1(t)
		e := r.engine()
		n, where := 0, ""
		e.fault = func(point string) error {
			n++
			if n == k {
				where = point
				return errCrash
			}
			return nil
		}
		p := r.plan(e, Request{})
		if err := e.Apply(context.Background(), p, ApplyOptions{}); !errors.Is(err, errCrash) {
			t.Fatalf("crash %d: Apply returned %v", k, err)
		}

		// A new process starts.
		for _, ri := range r.engine().Repair(context.Background()) {
			if ri.Result != ResultRepaired {
				t.Errorf("crash %d (%s): repair %+v", k, where, ri)
			}
		}
		got := tree(t, r.dir())
		v := r.state().Packages["app"].Version
		switch {
		case maps.Equal(got, v1Tree) && v == "1.0.0":
			sawOld = true
		case maps.Equal(got, v2Tree) && v == "2.0.0":
			sawNew = true
		default:
			t.Fatalf("crash %d (%s): state says %s and the folder holds:\n%s", k, where, v, show(got))
		}
		r.assertClean()
		if it := item(t, r.plan(r.engine(), Request{}), "app"); it.Result == ResultNeedsRepair {
			t.Errorf("crash %d (%s): still needs repair after repair", k, where)
		}
	}
	if !sawOld || !sawNew {
		t.Errorf("saw old=%v new=%v; both outcomes should occur across the crash points", sawOld, sawNew)
	}
}

// When undoing a failed change fails as well, the journal stays, the package needs repair (exit 6),
// and repair later restores the old version.
func TestFailedUndoNeedsRepair(t *testing.T) {
	r := atV1(t)
	e := r.engine()
	ops, undos := 0, 0
	e.fault = func(point string) error {
		switch point {
		case "op":
			if ops++; ops == 8 {
				return errors.New("disk full")
			}
		case "undo":
			if undos++; undos == 2 {
				return errors.New("file locked")
			}
		}
		return nil
	}
	p := r.plan(e, Request{})
	if err := e.Apply(context.Background(), p, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	it := item(t, p, "app")
	if it.Result != ResultNeedsRepair || !strings.Contains(it.Err.Error(), "disk full") || !strings.Contains(it.Err.Error(), "repair") {
		t.Fatalf("item = %+v", it)
	}
	if got := p.ExitCode(false); got != exitcode.NeedsRepair {
		t.Errorf("exit = %d, want 6", got)
	}
	if !journal.Exists(r.paths.Journal("app")) {
		t.Fatal("the journal was removed although the undo failed")
	}
	if it := item(t, r.plan(r.engine(), Request{}), "app"); it.Result != ResultNeedsRepair {
		t.Errorf("next plan = %+v", it)
	}
	for _, ri := range r.engine().Repair(context.Background()) {
		if ri.Result != ResultRepaired {
			t.Errorf("repair = %+v", ri)
		}
	}
	assertTree(t, r.dir(), v1Tree)
	r.assertClean()
}

// Ctrl+C between two file operations stops cleanly: what was done is undone (P3-12).
func TestInterruptedUpdateIsUndone(t *testing.T) {
	r := atV1(t)
	e := r.engine()
	ctx, cancel := context.WithCancel(context.Background())
	ops := 0
	e.fault = func(point string) error {
		if point == "op" {
			if ops++; ops == 5 {
				cancel()
			}
		}
		return nil
	}
	p := r.plan(e, Request{})
	if err := e.Apply(ctx, p, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if it := item(t, p, "app"); it.Result != ResultFailed || !errors.Is(it.Err, context.Canceled) {
		t.Fatalf("item = %+v", it)
	}
	assertTree(t, r.dir(), v1Tree)
	r.assertClean()
}

// A failed first install leaves no empty folder behind.
func TestFailedFirstInstallRemovesTheFolderItMade(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("app", "1.0.0", 10, v1Files...)
	e := r.engine()
	ops := 0
	e.fault = func(point string) error {
		if point == "op" {
			if ops++; ops == 3 {
				return errors.New("injected")
			}
		}
		return nil
	}
	p := r.plan(e, Request{InstallMissing: true})
	if err := e.Apply(context.Background(), p, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.dir()); err == nil {
		t.Errorf("the install folder was left: %v", tree(t, r.dir()))
	}
}

// tidy only ever touches a committed journal; an unfinished one is repair's business.
func TestTidyLeavesAnUnfinishedJournal(t *testing.T) {
	r := atV1(t)
	j, err := journal.Create(r.paths.Journal("app"), journal.Record{
		Op: journal.OpBegin, ID: "x", Pkg: "app", Action: "update", From: "1.0.0", To: "2.0.0", Dir: r.dir(), TS: r.clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	if err := r.engine().tidy("app"); err != nil {
		t.Fatal(err)
	}
	if !journal.Exists(r.paths.Journal("app")) {
		t.Error("an unfinished journal was promoted or removed")
	}
	if _, err := os.Stat(r.paths.Previous("app")); err == nil {
		t.Error("an unfinished journal became the rollback record")
	}
}

// A same-version reinstall keeps the existing rollback record (CraftWerk C10).
func TestReinstallKeepsThePreviousRecord(t *testing.T) {
	r := atV1(t)
	r.apply(r.engine(), Request{})
	it := item(t, r.apply(r.engine(), Request{Force: true}), "app")
	if it.Result != ResultUpdated || it.Action != ActReinstall {
		t.Fatalf("item = %+v", it)
	}
	rec := r.state().Packages["app"]
	if rec.Previous == nil || rec.Previous.Version != "1.0.0" {
		t.Errorf("previous = %+v", rec.Previous)
	}
	if _, err := journal.Read(r.paths.Previous("app")); err != nil {
		t.Errorf("rollback record: %v", err)
	}
	r.assertClean()
}

// What a check learned survives an install, and the manifest lists the folders an earlier
// version created as well as the new ones.
func TestUpdateKeepsCheckInfoAndFolderOwnership(t *testing.T) {
	r := atV1(t)
	r.apply(r.engine(), Request{})
	rec := r.state().Packages["app"]
	if rec.LatestSeen == nil || rec.LatestSeen.Version != "2.0.0" || rec.LastCheck == nil {
		t.Errorf("check info lost: %+v", rec)
	}
	m, err := state.LoadManifest(state.Path(r.paths.State, rec.Manifest))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.Dirs, ","); got != "data,keep-empty,lib,newdir,newdir/deep" {
		t.Errorf("dirs = %s", got)
	}
}

// Installing a rejected version on purpose lifts the rejection; newer rejections stay.
func TestExplicitInstallLiftsItsRejection(t *testing.T) {
	r := atV1(t)
	r.publish("app", "3.0.0", 10, archives.X("app.exe", "app v3"))
	st := r.state()
	st.Packages["app"].Rejected = []string{"2.0.0", "3.0.0"}
	if err := state.Save(r.paths.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	it := item(t, r.apply(r.engine(), Request{Packages: []string{"app"}, Want: "2.0.0"}), "app")
	if it.Result != ResultUpdated {
		t.Fatalf("item = %+v", it)
	}
	if got := strings.Join(r.state().Packages["app"].Rejected, ","); got != "3.0.0" {
		t.Errorf("rejected = %s, want 3.0.0", got)
	}
}

// A volume that clearly cannot take the download is refused before anything is fetched (P3-11).
func TestNotEnoughSpace(t *testing.T) {
	r := atV1(t)
	r.writeFile(appFile + "api = \"api\"\n") // the API path knows the asset's size
	r.free = func(string) (uint64, error) { return 1 << 20, nil }
	r.srv.ResetRequests()
	it := item(t, r.apply(r.engine(), Request{}), "app")
	if it.Result != ResultFailed || !strings.Contains(it.Err.Error(), "free space") {
		t.Fatalf("item = %+v", it)
	}
	if n := r.srv.RequestCount("/download/"); n != 0 {
		t.Errorf("%d downloads", n)
	}
	assertTree(t, r.dir(), v1Tree)
}

// A file of the user's where the new version needs a folder stops the update with a clear
// message, and nothing changes.
func TestFileInTheWayOfANewFolder(t *testing.T) {
	r := atV1(t)
	write(t, filepath.Join(r.dir(), "newdir"), "mine")
	it := item(t, r.apply(r.engine(), Request{}), "app")
	if it.Result != ResultFailed || !strings.Contains(it.Err.Error(), "in the way") {
		t.Fatalf("item = %+v", it)
	}
	want := maps.Clone(v1Tree)
	want["newdir"] = "mine"
	assertTree(t, r.dir(), want)
	r.assertClean()
}
