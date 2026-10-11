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
