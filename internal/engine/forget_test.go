package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/journal"
)

// Decision 5 of the M5 bug check: an interrupted change whose install folder is gone for good
// (or whose journal cannot be read) kept its package needs-repair forever. `repair --forget`
// gives the change up.

// crashedUpdate leaves atV1's update to 2.0.0 interrupted mid-way (a journal, a mixed folder).
func crashedUpdate(t *testing.T) *rig {
	t.Helper()
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
	if !journal.Exists(r.paths.Journal("app")) {
		t.Fatal("no journal")
	}
	return r
}

func TestForgetWithTheFolderGone(t *testing.T) {
	r := crashedUpdate(t)
	if err := os.RemoveAll(r.dir()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.paths.Staging("app"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Repair refuses, and says how to give up.
	items := r.engine().Repair(context.Background())
	if len(items) != 1 || items[0].Result != ResultNeedsRepair || !strings.Contains(items[0].Err.Error(), "gremloader repair --forget app") {
		t.Fatalf("repair = %+v", items)
	}

	it := r.engine().Forget("app")
	if it.Result != ResultForgotten || it.Err != nil || !strings.Contains(it.Reason, "not installed") {
		t.Fatalf("forget = %+v (%v)", it, it.Err)
	}
	r.assertClean()
	if rec := r.state().Packages["app"]; rec != nil && rec.Version != "" {
		t.Errorf("still recorded as installed: %+v", rec)
	}
	for _, d := range []string{r.paths.Previous("app"), r.paths.Manifests("app")} {
		if _, err := os.Stat(d); err == nil {
			t.Errorf("%s kept", d)
		}
	}
	// The package is an ordinary missing package now.
	if it := item(t, r.plan(r.engine(), Request{InstallMissing: true, CreateDir: true}), "app"); it.Action != ActInstall {
		t.Errorf("next plan = %+v (%v)", it, it.Err)
	}
}

func TestForgetWithTheFolderThere(t *testing.T) {
	r := crashedUpdate(t)
	it := r.engine().Forget("app")
	if it.Result != ResultForgotten || it.Err != nil || !strings.Contains(it.Reason, "may hold") {
		t.Fatalf("forget = %+v (%v)", it, it.Err)
	}
	r.assertClean()
	if v := r.state().Packages["app"].Version; v != "1.0.0" {
		t.Errorf("state says %q, want the record kept", v)
	}
	if it := item(t, r.plan(r.engine(), Request{}), "app"); it.Result == ResultNeedsRepair {
		t.Errorf("still needs repair: %+v", it)
	}
}

func TestForgetADamagedJournal(t *testing.T) {
	r := crashedUpdate(t)
	if err := os.WriteFile(filepath.Join(r.paths.Journal("app"), journal.LogName), []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	items := r.engine().Repair(context.Background())
	if len(items) != 1 || items[0].Result != ResultNeedsRepair || !strings.Contains(items[0].Err.Error(), "--forget app") {
		t.Fatalf("repair = %+v", items)
	}
	if it := r.engine().Forget("app"); it.Result != ResultForgotten {
		t.Fatalf("forget = %+v (%v)", it, it.Err)
	}
	r.assertClean()
}

func TestForgetWithNothingToForget(t *testing.T) {
	r := atV1(t)
	it := r.engine().Forget("app")
	if it.Result != ResultFailed || it.Err == nil || !strings.Contains(it.Err.Error(), "no unfinished change") {
		t.Errorf("forget = %+v", it)
	}
	if v := r.state().Packages["app"].Version; v != "1.0.0" {
		t.Errorf("state says %q", v)
	}
	if it := r.engine().Forget("../x"); it.Result != ResultFailed {
		t.Errorf("forget of an invalid name = %+v", it)
	}
}

// A crashed first install: nothing to un-record; the folder may hold some of its files.
func TestForgetAnInterruptedInstall(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("app", "1.0.0", 30, v1Files...)
	e := r.engine()
	e.fault = crashAt("record")
	if err := e.Apply(context.Background(), r.plan(e, Request{InstallMissing: true}), ApplyOptions{}); !errors.Is(err, errCrash) {
		t.Fatalf("Apply = %v", err)
	}
	it := r.engine().Forget("app")
	if it.Result != ResultForgotten || !strings.Contains(it.Reason, "may hold") {
		t.Fatalf("forget = %+v (%v)", it, it.Err)
	}
	r.assertClean()
}
