package engine

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/journal"
	"github.com/enls/gremloader/internal/proc"
	"github.com/enls/gremloader/internal/state"
	"github.com/enls/gremloader/internal/testutil/archives"
)

// atV2 is atV1 followed by the update to 2.0.0: there is something to roll back to.
func atV2(t *testing.T) *rig {
	t.Helper()
	r := atV1(t)
	if it := item(t, r.apply(r.engine(), Request{}), "app"); it.Result != ResultUpdated {
		t.Fatalf("updating to v2: %+v", it)
	}
	return r
}

func TestRollbackAndForwardAgain(t *testing.T) {
	r := atV2(t)
	v1Rec := r.state().Packages["app"].Previous

	it := r.engine().Rollback(context.Background(), "app")
	if it.Result != ResultRolledBack || it.From != "2.0.0" || it.To != "1.0.0" {
		t.Fatalf("rollback = %+v", it)
	}
	assertTree(t, r.dir(), v1Tree)
	r.assertClean()
	rec := r.state().Packages["app"]
	// The whole record of 1.0.0 comes back, not just its number.
	if rec.Version != "1.0.0" || rec.Tag != "v1.0.0" || rec.Asset != "app-1.0.0.zip" || len(rec.VerifiedBy) == 0 ||
		rec.Manifest != v1Rec.Manifest || rec.Previous == nil || rec.Previous.Version != "2.0.0" ||
		strings.Join(rec.Rejected, ",") != "2.0.0" || rec.LatestSeen == nil {
		t.Fatalf("state after rollback = %+v / %+v", rec, rec.Previous)
	}
	// The scheduled run does not put 2.0.0 straight back (P3-2).
	if it := item(t, r.plan(r.engine(), Request{}), "app"); it.Action != ActNone || it.Result != ResultRejected {
		t.Errorf("plan after rollback = %+v", it)
	}

	// Rolling back the rollback goes forward again and lifts the rejection.
	it = r.engine().Rollback(context.Background(), "app")
	if it.Result != ResultRolledBack || it.To != "2.0.0" {
		t.Fatalf("second rollback = %+v", it)
	}
	assertTree(t, r.dir(), v2Tree)
	rec = r.state().Packages["app"]
	if rec.Version != "2.0.0" || len(rec.Rejected) != 1 || rec.Rejected[0] != "1.0.0" || rec.Previous.Version != "1.0.0" {
		t.Errorf("state after rolling forward = %+v", rec)
	}
	// Both manifests survive the round trip: the installed version's and the rollback target's.
	for _, m := range []string{rec.Manifest, rec.Previous.Manifest} {
		if _, err := state.LoadManifest(state.Path(r.paths.State, m)); err != nil {
			t.Errorf("manifest %s: %v", m, err)
		}
	}
	r.assertClean()
}

// Property (plan E.5, M5): update, rollback, update again ends exactly where the first update did,
// and a further plan has nothing to do.
func TestUpdateRollbackUpdateIsIdempotent(t *testing.T) {
	r := atV2(t)
	if it := r.engine().Rollback(context.Background(), "app"); it.Result != ResultRolledBack {
		t.Fatalf("rollback = %+v", it)
	}
	if it := item(t, r.apply(r.engine(), Request{Packages: []string{"app"}, Want: "2.0.0"}), "app"); it.Result != ResultUpdated {
		t.Fatalf("update again = %+v", it)
	}
	assertTree(t, r.dir(), v2Tree)
	if it := item(t, r.plan(r.engine(), Request{}), "app"); it.Action != ActNone || it.Result != ResultUnchanged {
		t.Errorf("plan = %+v", it)
	}
	if rej := r.state().Packages["app"].Rejected; len(rej) != 0 {
		t.Errorf("rejected = %v", rej)
	}
}

func TestRollbackRefusals(t *testing.T) {
	t.Run("nothing to go back to", func(t *testing.T) {
		r := atV1(t)
		if it := r.engine().Rollback(context.Background(), "app"); it.Result != ResultFailed || !strings.Contains(it.Err.Error(), "no earlier version") {
			t.Errorf("item = %+v", it)
		}
		assertTree(t, r.dir(), v1Tree)
	})
	t.Run("not installed", func(t *testing.T) {
		r := newRig(t, appFile)
		if it := r.engine().Rollback(context.Background(), "app"); it.Result != ResultFailed {
			t.Errorf("item = %+v", it)
		}
	})
	t.Run("running", func(t *testing.T) {
		r := atV2(t)
		r.running = []proc.Process{{PID: 7, Path: filepath.Join(r.dir(), "app.exe")}}
		if it := r.engine().Rollback(context.Background(), "app"); it.Result != ResultSkipped {
			t.Errorf("item = %+v", it)
		}
		assertTree(t, r.dir(), v2Tree)
	})
	// P3-3: a damaged saved copy would restore garbage over a working install.
	t.Run("damaged rollback record", func(t *testing.T) {
		r := atV2(t)
		l, err := journal.Read(r.paths.Previous("app"))
		if err != nil {
			t.Fatal(err)
		}
		for _, rr := range l.Records {
			if rr.Op == journal.OpBackup && rr.Path == "app.exe" {
				write(t, l.BackupPath(rr), "bit rot")
			}
		}
		it := r.engine().Rollback(context.Background(), "app")
		var ve *VerifyError
		if it.Result != ResultFailed || !errors.As(it.Err, &ve) || !strings.Contains(ve.Msg, "app.exe") {
			t.Fatalf("item = %+v", it)
		}
		assertTree(t, r.dir(), v2Tree)
		r.assertClean()
	})
}

func TestRollbackRunsThePostUpdateHook(t *testing.T) {
	r := newRig(t, appFile+"post_update = [[\"register\", \"{name}\", \"{version}\"]]\n")
	r.publish("app", "1.0.0", 30, v1Files...)
	r.apply(r.engine(), Request{InstallMissing: true})
	r.publish("app", "2.0.0", 20, v2Files...)
	r.apply(r.engine(), Request{})
	r.engine().Rollback(context.Background(), "app")
	last := r.hooks.ran[len(r.hooks.ran)-1]
	if last.Event != "post_update" || last.Action != "rollback" || last.Version != "1.0.0" || last.Previous != "2.0.0" ||
		strings.Join(last.Commands[0], " ") != "register app 1.0.0" {
		t.Errorf("hook = %+v", last)
	}
}

// An adopted install has no file list: the update keeps the files it does not replace, and the
// rollback still restores exactly what was adopted (plan scenario S13).
func TestAdoptedInstallUpdateAndRollback(t *testing.T) {
	r := newRig(t, appFile)
	adopted := map[string]string{"app.exe": "app v1", "lib/": "", "lib/old.dll": "old lib", "stale.txt": "from 0.x"}
	for p, c := range adopted {
		if !strings.HasSuffix(p, "/") {
			write(t, filepath.Join(r.dir(), filepath.FromSlash(p)), c)
		}
	}
	if it := r.engine().Adopt(context.Background(), "app", AdoptOptions{Version: "v1.0"}); it.Result != ResultAdopted || it.To != "1.0.0" {
		t.Fatalf("adopt = %+v", it)
	}
	r.publish("app", "2.0.0", 20, v2Files...)
	if it := item(t, r.apply(r.engine(), Request{}), "app"); it.Result != ResultUpdated {
		t.Fatalf("update = %+v", it)
	}
	want := maps.Clone(v2Tree)
	delete(want, "user.cfg")
	delete(want, "data/my-notes.txt")
	want["lib/old.dll"], want["stale.txt"] = "old lib", "from 0.x" // not known to be ours: kept
	assertTree(t, r.dir(), want)

	if it := r.engine().Rollback(context.Background(), "app"); it.Result != ResultRolledBack {
		t.Fatalf("rollback = %+v", it)
	}
	assertTree(t, r.dir(), adopted)
	if rec := r.state().Packages["app"]; !rec.Adopted || rec.Manifest != "" || rec.Version != "1.0.0" {
		t.Errorf("state = %+v", rec)
	}
}

// Remove deletes what the package installed and the folders it made, and nothing of the user's
// (scenario S10).
func TestRemoveKeepsUserData(t *testing.T) {
	r := atV2(t)
	it := r.engine().Remove(context.Background(), "app", RemoveOptions{})
	if it.Result != ResultRemoved {
		t.Fatalf("item = %+v", it)
	}
	assertTree(t, r.dir(), map[string]string{"user.cfg": "mine", "data/": "", "data/my-notes.txt": "notes"})
	r.assertClean()
	rec := r.state().Packages["app"]
	if rec == nil || rec.Version != "" || rec.Previous != nil || len(rec.Rejected) != 0 || rec.LatestSeen == nil {
		t.Errorf("state = %+v", rec)
	}
	for _, d := range []string{r.paths.Previous("app"), r.paths.Manifests("app")} {
		if _, err := os.Stat(d); err == nil {
			t.Errorf("%s left behind", d)
		}
	}
}

func TestRemoveAdoptedNeedsDeleteDir(t *testing.T) {
	r := newRig(t, appFile)
	write(t, filepath.Join(r.dir(), "app.exe"), "x")
	r.engine().Adopt(context.Background(), "app", AdoptOptions{Version: "1.0.0"})
	if it := r.engine().Remove(context.Background(), "app", RemoveOptions{}); it.Result != ResultFailed || !strings.Contains(it.Err.Error(), "--delete-dir") {
		t.Fatalf("item = %+v", it)
	}
	if _, err := os.Stat(filepath.Join(r.dir(), "app.exe")); err != nil {
		t.Fatal("removed without --delete-dir")
	}
	if it := r.engine().Remove(context.Background(), "app", RemoveOptions{DeleteDir: true}); it.Result != ResultRemoved {
		t.Fatalf("item = %+v", it)
	}
	if _, err := os.Stat(r.dir()); err == nil {
		t.Error("the folder is still there")
	}
	if ents, _ := os.ReadDir(r.root); len(ents) != 0 {
		t.Errorf("left beside it: %v", ents)
	}
}

// A folder that existed before GremLoader (adopted, then updated: the CraftWerk path) is never
// claimed, but once remove has emptied it, it goes too (user, 2026-10-11). One with the user's
// files in it stays.
func TestRemoveDeletesTheInstallFolderItEmptied(t *testing.T) {
	for _, userFile := range []bool{false, true} {
		r := newRig(t, appFile)
		write(t, filepath.Join(r.dir(), "app.exe"), "app v1")
		if userFile {
			write(t, filepath.Join(r.dir(), "user.cfg"), "mine")
		}
		if it := r.engine().Adopt(context.Background(), "app", AdoptOptions{Version: "1.0.0"}); it.Result != ResultAdopted {
			t.Fatalf("adopt = %+v (%v)", it, it.Err)
		}
		r.publish("app", "2.0.0", 20, archives.X("app.exe", "app v2"))
		if it := item(t, r.apply(r.engine(), Request{}), "app"); it.Result != ResultUpdated {
			t.Fatalf("update = %+v (%v)", it, it.Err)
		}
		if it := r.engine().Remove(context.Background(), "app", RemoveOptions{}); it.Result != ResultRemoved {
			t.Fatalf("remove = %+v (%v)", it, it.Err)
		}
		_, err := os.Stat(r.dir())
		if userFile {
			assertTree(t, r.dir(), map[string]string{"user.cfg": "mine"})
		} else if err == nil {
			t.Error("the emptied install folder is still there")
		}
	}
}

func TestFailingPreRemoveHookRemovesNothing(t *testing.T) {
	r := newRig(t, appFile+"pre_remove = [[\"stop-service\"]]\n")
	r.publish("app", "1.0.0", 30, v1Files...)
	r.apply(r.engine(), Request{InstallMissing: true})
	r.hooks.fail = map[string]error{"pre_remove": errors.New("exit status 1")}
	before := tree(t, r.dir())
	if it := r.engine().Remove(context.Background(), "app", RemoveOptions{}); it.Result != ResultFailed {
		t.Fatalf("item = %+v", it)
	}
	assertTree(t, r.dir(), before)
	if r.state().Packages["app"].Version != "1.0.0" {
		t.Error("state changed")
	}
}

// ---- adopt and verify ----

func TestAdoptFromTheExecutableVersion(t *testing.T) {
	r := newRig(t, appFile+"version_from_exe = \"app.exe\"\n")
	write(t, filepath.Join(r.dir(), "app.exe"), "x")
	var asked string
	r.exeVersion = func(p string) (string, error) { asked = p; return "0.6.0.0", nil }
	it := r.engine().Adopt(context.Background(), "app", AdoptOptions{})
	if it.Result != ResultAdopted || it.To != "0.6.0" || asked != filepath.Join(r.dir(), "app.exe") {
		t.Fatalf("item = %+v (asked %s)", it, asked)
	}
	if len(it.Notes) != 1 || !strings.Contains(it.Notes[0], "--delete-dir") {
		t.Errorf("notes = %q", it.Notes)
	}
	rec := r.state().Packages["app"]
	if rec.Version != "0.6.0" || !rec.Adopted || rec.Dir != r.dir() || rec.InstalledAt == nil {
		t.Errorf("state = %+v", rec)
	}
	if got := read(t, filepath.Join(r.dir(), "app.exe")); got != "x" {
		t.Error("adopt changed a file")
	}
}

func TestAdoptRefusals(t *testing.T) {
	t.Run("no version source", func(t *testing.T) {
		r := newRig(t, appFile)
		write(t, filepath.Join(r.dir(), "app.exe"), "x")
		if it := r.engine().Adopt(context.Background(), "app", AdoptOptions{}); it.Result != ResultFailed || !strings.Contains(it.Err.Error(), "--version") {
			t.Errorf("item = %+v", it)
		}
	})
	t.Run("unreadable exe version", func(t *testing.T) {
		r := newRig(t, appFile+"version_from_exe = \"app.exe\"\n")
		write(t, filepath.Join(r.dir(), "app.exe"), "x")
		if it := r.engine().Adopt(context.Background(), "app", AdoptOptions{}); it.Result != ResultFailed {
			t.Errorf("item = %+v", it)
		}
	})
	t.Run("empty folder", func(t *testing.T) {
		r := newRig(t, appFile)
		if err := os.MkdirAll(r.dir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if it := r.engine().Adopt(context.Background(), "app", AdoptOptions{Version: "1.0.0"}); it.Result != ResultFailed {
			t.Errorf("item = %+v", it)
		}
	})
	t.Run("already installed", func(t *testing.T) {
		r := atV1(t)
		if it := r.engine().Adopt(context.Background(), "app", AdoptOptions{Version: "1.0.0"}); it.Result != ResultFailed {
			t.Errorf("item = %+v", it)
		}
	})
	t.Run("not a version", func(t *testing.T) {
		r := newRig(t, appFile)
		write(t, filepath.Join(r.dir(), "app.exe"), "x")
		if it := r.engine().Adopt(context.Background(), "app", AdoptOptions{Version: "latest"}); it.Result != ResultFailed {
			t.Errorf("item = %+v", it)
		}
		if r.state().Packages["app"] != nil && r.state().Packages["app"].Version != "" {
			t.Error("recorded anyway")
		}
	})
}

// adopt --verify downloads the release and records the folder only if it holds it, which also
// gives a manifest.
func TestAdoptVerify(t *testing.T) {
	setup := func(t *testing.T) *rig {
		r := newRig(t, appFile)
		r.publish("app", "1.0.0", 30, v1Files...)
		for _, f := range v1Files {
			if f.Kind == archives.Dir {
				if err := os.MkdirAll(filepath.Join(r.dir(), filepath.FromSlash(f.Name)), 0o755); err != nil {
					t.Fatal(err)
				}
				continue
			}
			write(t, filepath.Join(r.dir(), filepath.FromSlash(f.Name)), string(f.Data))
		}
		write(t, filepath.Join(r.dir(), "user.cfg"), "mine")
		return r
	}
	t.Run("matching", func(t *testing.T) {
		r := setup(t)
		it := r.engine().Adopt(context.Background(), "app", AdoptOptions{Version: "1.0.0", Verify: true})
		if it.Result != ResultAdopted {
			t.Fatalf("item = %+v", it)
		}
		rec := r.state().Packages["app"]
		if rec.Manifest == "" || rec.Tag != "v1.0.0" || len(rec.VerifiedBy) == 0 || rec.AssetSHA256 == "" {
			t.Fatalf("state = %+v", rec)
		}
		m, err := state.LoadManifest(state.Path(r.paths.State, rec.Manifest))
		if err != nil || len(m.Files) != 5 || len(m.Dirs) != 0 {
			t.Errorf("manifest = %+v, %v", m, err)
		}
		r.assertClean()
		// With a file list, remove now works without --delete-dir and keeps the user's file.
		if it := r.engine().Remove(context.Background(), "app", RemoveOptions{}); it.Result != ResultRemoved {
			t.Fatalf("remove = %+v", it)
		}
		if read(t, filepath.Join(r.dir(), "user.cfg")) != "mine" {
			t.Error("user file lost")
		}
	})
	t.Run("different", func(t *testing.T) {
		r := setup(t)
		write(t, filepath.Join(r.dir(), "app.exe"), "patched")
		if err := os.Remove(filepath.Join(r.dir(), "lib", "old.dll")); err != nil {
			t.Fatal(err)
		}
		it := r.engine().Adopt(context.Background(), "app", AdoptOptions{Version: "1.0.0", Verify: true})
		if it.Result != ResultFailed || !strings.Contains(it.Err.Error(), "1 missing (lib/old.dll)") ||
			!strings.Contains(it.Err.Error(), "1 modified (app.exe)") {
			t.Fatalf("item = %+v", it)
		}
		if rec := r.state().Packages["app"]; rec != nil && rec.Version != "" {
			t.Errorf("recorded anyway: %+v", rec)
		}
		r.assertClean()
	})
}

func TestVerify(t *testing.T) {
	r := atV1(t)
	for _, remote := range []bool{false, true} {
		c := r.engine().Verify(context.Background(), nil, remote)
		if len(c) != 1 || !c[0].OK() || c[0].Files != 5 {
			t.Fatalf("remote=%v clean: %+v", remote, c[0])
		}
	}
	write(t, filepath.Join(r.dir(), "app.exe"), "app v9") // same size: only the hash can tell
	if err := os.Remove(filepath.Join(r.dir(), "olddir", "x.txt")); err != nil {
		t.Fatal(err)
	}
	for _, remote := range []bool{false, true} {
		c := r.engine().Verify(context.Background(), []string{"app"}, remote)[0]
		if c.OK() || strings.Join(c.Modified, ",") != "app.exe" || strings.Join(c.Missing, ",") != "olddir/x.txt" {
			t.Errorf("remote=%v: %+v", remote, c)
		}
	}
	r.assertClean()

	adopted := newRig(t, appFile)
	write(t, filepath.Join(adopted.dir(), "app.exe"), "x")
	adopted.engine().Adopt(context.Background(), "app", AdoptOptions{Version: "1.0.0"})
	if c := adopted.engine().Verify(context.Background(), nil, false)[0]; !c.NoManifest || c.Files != 0 {
		t.Errorf("adopted: %+v", c)
	}
}

func TestVerifyListsTheQuarantine(t *testing.T) {
	r := atV1(t)
	r.publishBadSums("3.0.0")
	r.apply(r.engine(), Request{})
	if c := r.engine().Verify(context.Background(), nil, false)[0]; c.Quarantine == "" {
		t.Errorf("check = %+v", c)
	}
}

// ---- rollback and remove under failures and crashes ----

// sweep injects a failure, then (separately) a crash, at every fault point of op, and checks the
// folder ends exactly before or exactly after, never between.
func sweep(t *testing.T, setup func(*testing.T) *rig, op func(*Engine) (*Item, error), before, after map[string]string,
	check func(*testing.T, *rig)) {
	t.Helper()
	r := setup(t)
	e := r.engine()
	total := 0
	e.fault = func(string) error { total++; return nil }
	if _, err := op(e); err != nil {
		t.Fatal(err)
	}
	assertTree(t, r.dir(), after)
	for _, crash := range []bool{false, true} {
		for k := 1; k <= total; k++ {
			r := setup(t)
			e := r.engine()
			n, where := 0, ""
			e.fault = func(point string) error {
				if n++; n == k {
					where = point
					if crash {
						return errCrash
					}
					return errors.New("injected")
				}
				return nil
			}
			_, err := op(e)
			if crash != (err != nil) {
				t.Fatalf("crash=%v at %d (%s): err %v", crash, k, where, err)
			}
			for _, ri := range r.engine().Repair(context.Background()) {
				if ri.Result != ResultRepaired {
					t.Errorf("crash=%v at %d (%s): repair %+v", crash, k, where, ri)
				}
			}
			got := tree(t, r.dir())
			if !maps.Equal(got, before) && !maps.Equal(got, after) {
				t.Fatalf("crash=%v at %d (%s): the folder holds:\n%s", crash, k, where, show(got))
			}
			if check != nil {
				check(t, r)
			}
			r.assertClean()
			if it := item(t, r.plan(r.engine(), Request{}), "app"); it.Result == ResultNeedsRepair {
				t.Errorf("crash=%v at %d (%s): still needs repair", crash, k, where)
			}
		}
	}
}

func TestRollbackUnderFailuresAndCrashes(t *testing.T) {
	sweep(t, atV2, func(e *Engine) (*Item, error) { return e.rollback(context.Background(), "app") }, v2Tree, v1Tree, nil)
}

func TestRemoveUnderFailuresAndCrashes(t *testing.T) {
	sweep(t, atV2, func(e *Engine) (*Item, error) { return e.remove(context.Background(), "app", RemoveOptions{}) },
		v2Tree, map[string]string{"user.cfg": "mine", "data/": "", "data/my-notes.txt": "notes"},
		func(t *testing.T, r *rig) {
			// Removed means nothing is kept; still installed means its rollback record is intact.
			_, perr := os.Stat(r.paths.Previous("app"))
			if installed := r.state().Packages["app"].Version != ""; installed == (perr != nil) {
				t.Errorf("installed=%v but previous/: %v", installed, perr)
			}
		})
}

func TestRollbackRecordMustMatchState(t *testing.T) {
	r := atV2(t)
	st := r.state()
	st.Packages["app"].Previous.Version = "0.9.0"
	if err := state.Save(r.paths.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	if it := r.engine().Rollback(context.Background(), "app"); it.Result != ResultFailed || !strings.Contains(it.Err.Error(), "does not describe") {
		t.Errorf("item = %+v", it)
	}
	assertTree(t, r.dir(), v2Tree)
}

func TestRollbackWaitsForRepair(t *testing.T) {
	r := atV2(t)
	j, err := journal.Create(r.paths.Journal("app"), journal.Record{
		Op: journal.OpBegin, ID: "x", Pkg: "app", Action: "update", From: "2.0.0", To: "3.0.0", Dir: r.dir(), TS: r.clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	for name, it := range map[string]*Item{
		"rollback": r.engine().Rollback(context.Background(), "app"),
		"remove":   r.engine().Remove(context.Background(), "app", RemoveOptions{}),
	} {
		if it.Result != ResultNeedsRepair {
			t.Errorf("%s = %+v", name, it)
		}
	}
	assertTree(t, r.dir(), v2Tree)
}
