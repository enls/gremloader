package engine

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/journal"
	"github.com/enls/gremloader/internal/state"
)

// Bug check of M5, suspicion 1: the journal records `step state` before the record step. If that
// step fails (an error, not a crash), the change is undone in-process; if the undo fails part-way,
// the journal is left with `step state` in it, and repair finishes FORWARD over a half-restored
// folder. Repair must leave exactly the old or exactly the new version.
func TestFailedUndoAfterTheStateStepIsNotFinishedForward(t *testing.T) {
	r := atV1(t)
	e := r.engine()
	undos := 0
	e.fault = func(point string) error {
		switch point {
		case "record":
			return errors.New("injected at record")
		case "undo":
			if undos++; undos == 2 {
				return errors.New("file locked during undo")
			}
		}
		return nil
	}
	p := r.plan(e, Request{})
	if err := e.Apply(context.Background(), p, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if it := item(t, p, "app"); it.Result != ResultNeedsRepair {
		t.Fatalf("item = %+v", it)
	}
	for _, ri := range r.engine().Repair() {
		if ri.Result != ResultRepaired {
			t.Errorf("repair = %+v", ri)
		}
	}
	got := tree(t, r.dir())
	v := r.state().Packages["app"].Version
	old := maps.Equal(got, v1Tree) && v == "1.0.0"
	if !old && (!maps.Equal(got, v2Tree) || v != "2.0.0") {
		t.Fatalf("state says %s and the folder holds:\n%s", v, show(got))
	}
}

// If giving up cannot be recorded, nothing is undone: every file is in place and the journal says
// so, so repair finishes forward to exactly the new version.
func TestUnrecordedAbortIsFinishedForwardByRepair(t *testing.T) {
	r := atV1(t)
	e := r.engine()
	undos := 0
	e.fault = func(point string) error {
		switch point {
		case "record":
			return errors.New("injected at record")
		case "abort":
			return errors.New("journal disk full")
		case "undo":
			undos++
		}
		return nil
	}
	p := r.plan(e, Request{})
	if err := e.Apply(context.Background(), p, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if it := item(t, p, "app"); it.Result != ResultNeedsRepair {
		t.Fatalf("item = %+v", it)
	}
	if undos != 0 {
		t.Errorf("%d undo steps ran although the abort was not recorded", undos)
	}
	for _, ri := range r.engine().Repair() {
		if ri.Result != ResultRepaired {
			t.Errorf("repair = %+v", ri)
		}
	}
	if got, v := tree(t, r.dir()), r.state().Packages["app"].Version; !maps.Equal(got, v2Tree) || v != "2.0.0" {
		t.Fatalf("state says %s and the folder holds:\n%s", v, show(got))
	}
}

// Bug check of M5, from suspicion 6: a read-only installed file (set by the user or by whatever
// installed an adopted folder; extraction never makes one). Windows refuses to replace or delete
// it. The update may fail, but it must leave exactly the old version and not need repair.
func TestReadOnlyInstalledFile(t *testing.T) {
	r := atV1(t)
	app := filepath.Join(r.dir(), "app.exe")
	if err := os.Chmod(app, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(app, 0o644) })
	it := item(t, r.apply(r.engine(), Request{}), "app")
	got, v := tree(t, r.dir()), r.state().Packages["app"].Version
	switch {
	case it.Result == ResultUpdated && maps.Equal(got, v2Tree) && v == "2.0.0":
	case it.Result == ResultFailed && maps.Equal(got, v1Tree) && v == "1.0.0":
	default:
		t.Fatalf("result %s (%v); state says %s and the folder holds:\n%s", it.Result, it.Err, v, show(got))
	}
	t.Logf("result %s: %v", it.Result, it.Err)
	r.assertClean()
}

// Bug check of M5, suspicion 10: a same-version reinstall keeps the update's rollback record, whose
// "created" files the reinstall has since replaced (same paths, new files). Rolling back must still
// give exactly the old version, and forward again exactly the new one.
func TestRollbackAfterAReinstall(t *testing.T) {
	r := atV1(t)
	if it := item(t, r.apply(r.engine(), Request{}), "app"); it.Result != ResultUpdated {
		t.Fatalf("updating to v2: %+v", it)
	}
	// Damage a file of v2 that v1 also has, so the reinstall has something to put right.
	if err := os.WriteFile(filepath.Join(r.dir(), "app.exe"), []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if it := item(t, r.apply(r.engine(), Request{Force: true}), "app"); it.Action != ActReinstall || it.Result != ResultUpdated {
		t.Fatalf("reinstall: %+v", it)
	}
	assertTree(t, r.dir(), v2Tree)

	if it := r.engine().Rollback(context.Background(), "app"); it.Result != ResultRolledBack || it.To != "1.0.0" {
		t.Fatalf("rollback = %+v", it)
	}
	assertTree(t, r.dir(), v1Tree)
	if v := r.state().Packages["app"].Version; v != "1.0.0" {
		t.Errorf("state says %s", v)
	}
	r.assertClean()

	if it := r.engine().Rollback(context.Background(), "app"); it.Result != ResultRolledBack || it.To != "2.0.0" {
		t.Fatalf("forward again = %+v", it)
	}
	assertTree(t, r.dir(), v2Tree)
	r.assertClean()
}

// Bug check of M5 (sub-agent finding 2): journal.Create makes the directory, then the log, then
// writes the begin record. Dying anywhere in there changed nothing in the install, so neither
// repair nor the next run may treat it as a damaged journal or trip over it.
func TestJournalThatNeverBegan(t *testing.T) {
	for _, c := range []struct {
		name string
		log  *string // nil: no log file at all
	}{
		{"no log", nil},
		{"empty log", ptr("")},
		{"torn begin record", ptr(`{"op":"begin","id":"r1","pkg":"app","act`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, viaRepair := range []bool{true, false} {
				r := atV1(t)
				jdir := r.paths.Journal("app")
				if err := os.MkdirAll(filepath.Join(jdir, "b"), 0o700); err != nil {
					t.Fatal(err)
				}
				if c.log != nil {
					if err := os.WriteFile(filepath.Join(jdir, "undo.jsonl"), []byte(*c.log), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if viaRepair {
					for _, ri := range r.engine().Repair() {
						if ri.Result != ResultRepaired {
							t.Errorf("repair = %+v (%v)", ri, ri.Err)
						}
					}
				}
				// The next run updates as if nothing had happened.
				if it := item(t, r.apply(r.engine(), Request{}), "app"); it.Result != ResultUpdated {
					t.Fatalf("repair first %v: update = %+v (%v)", viaRepair, it, it.Err)
				}
				assertTree(t, r.dir(), v2Tree)
				r.assertClean()
			}
		})
	}
}

// Bug check of M5 (sub-agent finding 3): an update dies part-way, and repair runs while the install
// folder is not there (a drive not connected). Undo finds nothing to remove and nothing to restore
// to; it must not call that success and throw the journal away. Once the drive is back, repair
// gives exactly the old version.
func TestRepairWithTheInstallFolderAway(t *testing.T) {
	r := atV1(t)
	e := r.engine()
	fsPoints := 0
	e.fault = func(point string) error {
		if point == "fs" {
			if fsPoints++; fsPoints == 4 {
				return errCrash
			}
		}
		return nil
	}
	if err := e.Apply(context.Background(), r.plan(e, Request{}), ApplyOptions{}); !errors.Is(err, errCrash) {
		t.Fatalf("Apply = %v", err)
	}
	away := r.dir() + ".away"
	if err := os.Rename(r.dir(), away); err != nil {
		t.Fatal(err)
	}
	for _, ri := range r.engine().Repair() {
		if ri.Result != ResultNeedsRepair {
			t.Errorf("repair without the folder = %+v (%v)", ri, ri.Err)
		}
	}
	if err := os.Rename(away, r.dir()); err != nil {
		t.Fatal(err)
	}
	for _, ri := range r.engine().Repair() {
		if ri.Result != ResultRepaired {
			t.Errorf("repair = %+v (%v)", ri, ri.Err)
		}
	}
	assertTree(t, r.dir(), v1Tree)
	if v := r.state().Packages["app"].Version; v != "1.0.0" {
		t.Errorf("state says %s", v)
	}
	r.assertClean()
}

// Bug check of M5 (sub-agent finding 4): `remove --delete-dir` renames the folder aside, then
// deletes it and forgets the package, unjournaled. After a crash in between, the package was
// recorded at a folder that no longer existed and every later remove failed on the rename. A
// second `remove --delete-dir` must finish the job.
func TestDeleteDirAfterACrash(t *testing.T) {
	r := newRig(t, appFile)
	write(t, filepath.Join(r.dir(), "app.exe"), "x")
	r.engine().Adopt(context.Background(), "app", AdoptOptions{Version: "1.0.0"})
	aside := filepath.Join(r.root, ".App.gremloader-0.tmp")
	if err := os.Rename(r.dir(), aside); err != nil {
		t.Fatal(err)
	}
	if it := r.engine().Remove(context.Background(), "app", RemoveOptions{DeleteDir: true}); it.Result != ResultRemoved {
		t.Fatalf("remove = %+v (%v)", it, it.Err)
	}
	if rec := r.state().Packages["app"]; rec != nil && rec.Version != "" {
		t.Errorf("still recorded as installed: %+v", rec)
	}
	if ents, _ := os.ReadDir(r.root); len(ents) != 0 {
		t.Errorf("left behind: %v", ents)
	}
}

// A folder that is missing together with the folder holding it is a drive not connected, not an
// earlier delete: the package stays.
func TestDeleteDirWithTheDriveAway(t *testing.T) {
	r := newRig(t, appFile)
	write(t, filepath.Join(r.dir(), "app.exe"), "x")
	r.engine().Adopt(context.Background(), "app", AdoptOptions{Version: "1.0.0"})
	if err := os.Rename(r.root, r.root+".away"); err != nil {
		t.Fatal(err)
	}
	if it := r.engine().Remove(context.Background(), "app", RemoveOptions{DeleteDir: true}); it.Result != ResultFailed {
		t.Fatalf("remove = %+v", it)
	}
	if v := r.state().Packages["app"].Version; v != "1.0.0" {
		t.Errorf("state says %q", v)
	}
}

// Bug check of M5 (sub-agent finding 5): across volumes a file is copied to a temporary name beside
// its target and renamed over it. That temporary file is not journaled, so a failure or a crash
// between the two must still leave exactly the old or the new version. Here every rename out of
// staging is refused as cross-device, and the update is swept like rollback and remove.
func TestUpdateAcrossVolumesUnderFailuresAndCrashes(t *testing.T) {
	staging := string(filepath.Separator) + "staging" + string(filepath.Separator)
	osRename = func(from, to string) error {
		if strings.Contains(from, staging) {
			return errCrossDevice
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { osRename = os.Rename })
	sweep(t, atV1, func(e *Engine) (*Item, error) {
		p, err := e.Plan(context.Background(), Request{})
		if err != nil {
			return nil, err
		}
		err = e.Apply(context.Background(), p, ApplyOptions{})
		return p.Items[0], err
	}, v1Tree, v2Tree, nil)
}

func TestTempOf(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "app.v2.exe")
	tmp, err := tempName(dst)
	if err != nil {
		t.Fatal(err)
	}
	if base, ok := tempOf(filepath.Base(tmp)); !ok || base != "app.v2.exe" {
		t.Errorf("tempOf(%q) = %q, %v", filepath.Base(tmp), base, ok)
	}
	for _, name := range []string{"app.exe", ".app.exe.gremloader-.tmp", ".app.exe.gremloader-1a.tmp",
		"app.exe.gremloader-1.tmp", ".app.exe.gremloader-1.tmpx", ".gremloader-1.tmp"} {
		if base, ok := tempOf(name); ok && base != "" {
			t.Errorf("tempOf(%q) = %q, true", name, base)
		}
	}
}

// Bug check of M5 (sub-agent finding 6): the state records inside a journal's begin record are
// opaque JSON to the journal parser. A damaged or edited journal whose "after" record names a
// manifest outside the state directory must not make repair write there.
func TestRepairChecksTheJournalsStateRecords(t *testing.T) {
	r := atV1(t)
	rec := *r.state().Packages["app"]
	after := rec
	after.Version, after.Manifest = "2.0.0", "../outside/m.json"
	raw, err := json.Marshal(changeRecord{Before: &rec, After: &after})
	if err != nil {
		t.Fatal(err)
	}
	jdir := r.paths.Journal("app")
	j, err := journal.Create(jdir, journal.Record{Op: journal.OpBegin, ID: "x", Pkg: "app", Action: "update",
		From: "1.0.0", To: "2.0.0", Dir: r.dir(), TS: time.Now(), Result: raw})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(journal.Record{Op: journal.OpStep, Name: "state"}); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	m, err := state.LoadManifest(state.Path(r.paths.State, rec.Manifest))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SaveManifest(filepath.Join(jdir, journal.ManifestName), m); err != nil {
		t.Fatal(err)
	}

	for _, ri := range r.engine().Repair() {
		if ri.Result != ResultNeedsRepair {
			t.Errorf("repair = %+v (%v)", ri, ri.Err)
		}
	}
	outside := filepath.Join(r.paths.State, "..", "outside")
	if _, err := os.Stat(outside); err == nil {
		t.Errorf("repair wrote outside the state directory: %s", outside)
	}
	if v := r.state().Packages["app"].Version; v != "1.0.0" {
		t.Errorf("state says %s", v)
	}
}

// A journal is acted on only under its own package's name.
func TestRepairChecksTheJournalsPackage(t *testing.T) {
	r := atV1(t)
	e := r.engine()
	e.fault = func(point string) error {
		if point == "append" {
			return errCrash
		}
		return nil
	}
	if err := e.Apply(context.Background(), r.plan(e, Request{}), ApplyOptions{}); !errors.Is(err, errCrash) {
		t.Fatalf("Apply = %v", err)
	}
	if err := os.Rename(r.paths.Journal("app"), r.paths.Journal("other")); err != nil {
		t.Fatal(err)
	}
	items := r.engine().Repair()
	if len(items) != 1 || items[0].Package != "other" || items[0].Result != ResultNeedsRepair {
		t.Fatalf("repair = %+v", items)
	}
	if !journal.Exists(r.paths.Journal("other")) {
		t.Error("the journal was removed")
	}
}

// Bug check of M5 (sub-agent finding 8): a quarantined download (up to 512 MiB) outlived both the
// install that accepted it and the removal of its package.
func TestQuarantineGoesWhenItIsAnswered(t *testing.T) {
	r := atV1(t)
	received := r.publishBadSums("3.0.0")
	r.apply(r.engine(), Request{})
	if _, ok := r.quarantineInfo(); !ok {
		t.Fatal("not quarantined")
	}
	p, err := r.applyWith(r.engine(), Request{Packages: []string{"app"}},
		ApplyOptions{Accept: &Acceptance{Package: "app", Version: "3.0.0", SHA256: received}})
	if err != nil {
		t.Fatal(err)
	}
	if it := item(t, p, "app"); it.Result != ResultUpdated {
		t.Fatalf("accepted install: %+v (%v)", it, it.Err)
	}
	if _, ok := r.quarantineInfo(); ok {
		t.Error("the quarantine is still there after its version was installed")
	}

	r.publishBadSums("4.0.0")
	r.apply(r.engine(), Request{})
	if _, ok := r.quarantineInfo(); !ok {
		t.Fatal("4.0.0 not quarantined")
	}
	if it := r.engine().Remove(context.Background(), "app", RemoveOptions{}); it.Result != ResultRemoved {
		t.Fatalf("remove = %+v", it)
	}
	if _, err := os.Stat(filepath.Join(r.paths.State, "quarantine", "app")); err == nil {
		t.Error("the quarantine is still there after the package was removed")
	}
}
