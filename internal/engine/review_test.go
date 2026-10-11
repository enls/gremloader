package engine

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
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
