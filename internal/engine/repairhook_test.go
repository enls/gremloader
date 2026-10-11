package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/journal"
)

// Decision 4 of the M5 bug check: a change that repair finishes forward never ran its post hook
// (the run died first), so repair runs it, as the action that was interrupted.

const hookedFile = appFile + `post_install = [["register", "{name}", "{version}"]]
post_update  = [["register", "{name}", "{version}"]]
post_remove  = [["unregister", "{name}", "{version}"]]
`

// crashAt returns a fault function that "kills the process" at the named point.
func crashAt(point string) func(string) error {
	return func(p string) error {
		if p == point {
			return errCrash
		}
		return nil
	}
}

// hookedAt installs v1 (and with v2, updates to it) with hooks on every event, then forgets the
// hooks run so far.
func hookedAt(t *testing.T, v2 bool) *rig {
	t.Helper()
	r := newRig(t, hookedFile)
	r.publish("app", "1.0.0", 30, v1Files...)
	if it := item(t, r.apply(r.engine(), Request{InstallMissing: true}), "app"); it.Result != ResultInstalled {
		t.Fatalf("install: %+v", it)
	}
	r.publish("app", "2.0.0", 20, v2Files...)
	if v2 {
		if it := item(t, r.apply(r.engine(), Request{}), "app"); it.Result != ResultUpdated {
			t.Fatalf("update: %+v", it)
		}
	}
	r.hooks.ran = nil
	return r
}

func TestForwardRepairRunsTheSkippedHook(t *testing.T) {
	type want struct{ event, action, version, previous, cmd string }
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) *rig
		crash func(r *rig, e *Engine) error
		want  want
	}{
		{"install", func(t *testing.T) *rig {
			r := newRig(t, hookedFile)
			r.publish("app", "1.0.0", 30, v1Files...)
			return r
		}, func(r *rig, e *Engine) error {
			return e.Apply(context.Background(), r.plan(e, Request{InstallMissing: true}), ApplyOptions{})
		}, want{"post_install", "install", "1.0.0", "", "register app 1.0.0"}},

		{"update", func(t *testing.T) *rig { return hookedAt(t, false) }, func(r *rig, e *Engine) error {
			return e.Apply(context.Background(), r.plan(e, Request{}), ApplyOptions{})
		}, want{"post_update", "update", "2.0.0", "1.0.0", "register app 2.0.0"}},

		{"rollback", func(t *testing.T) *rig { return hookedAt(t, true) }, func(_ *rig, e *Engine) error {
			_, err := e.rollback(context.Background(), "app")
			return err
		}, want{"post_update", "rollback", "1.0.0", "2.0.0", "register app 1.0.0"}},

		{"remove", func(t *testing.T) *rig { return hookedAt(t, false) }, func(_ *rig, e *Engine) error {
			_, err := e.remove(context.Background(), "app", RemoveOptions{})
			return err
		}, want{"post_remove", "remove", "1.0.0", "1.0.0", "unregister app 1.0.0"}},
	} {
		for _, point := range []string{"record", "commit"} {
			t.Run(tc.name+"/"+point, func(t *testing.T) {
				r := tc.setup(t)
				e := r.engine()
				e.fault = crashAt(point)
				if err := tc.crash(r, e); !errors.Is(err, errCrash) {
					t.Fatalf("no crash: %v", err)
				}
				for _, h := range r.hooks.ran {
					if h.Event == tc.want.event {
						t.Fatalf("the %s hook ran before the crash point: %+v", h.Event, h)
					}
				}
				r.hooks.ran = nil

				items := r.engine().Repair(context.Background())
				if len(items) != 1 || items[0].Result != ResultRepaired {
					t.Fatalf("repair = %+v", items)
				}
				if len(r.hooks.ran) != 1 {
					t.Fatalf("hooks after repair = %+v, want one %s", r.hooks.ran, tc.want.event)
				}
				h := r.hooks.ran[0]
				got := want{h.Event, h.Action, h.Version, h.Previous, strings.Join(h.Commands[0], " ")}
				if got != tc.want || h.Dir != r.dir() {
					t.Errorf("hook = %+v, want %+v in %s", got, tc.want, r.dir())
				}
				// Repair again: nothing left to do, and the hook does not run twice.
				r.hooks.ran = nil
				if items := r.engine().Repair(context.Background()); len(items) != 0 || len(r.hooks.ran) != 0 {
					t.Errorf("second repair = %+v, hooks %+v", items, r.hooks.ran)
				}
			})
		}
	}
}

// An undone change did not happen: no hook.
func TestUndoingRepairRunsNoHook(t *testing.T) {
	r := hookedAt(t, false)
	e := r.engine()
	e.fault = crashAt("append")
	if err := e.Apply(context.Background(), r.plan(e, Request{}), ApplyOptions{}); !errors.Is(err, errCrash) {
		t.Fatalf("no crash: %v", err)
	}
	r.hooks.ran = nil
	items := r.engine().Repair(context.Background())
	if len(items) != 1 || items[0].Result != ResultRepaired || !strings.Contains(items[0].Reason, "undid") {
		t.Fatalf("repair = %+v", items)
	}
	if len(r.hooks.ran) != 0 {
		t.Errorf("hooks = %+v", r.hooks.ran)
	}
}

// After the commit record nothing can tell whether the hook ran (the crash may have come before
// it or after it), so repair only tidies, and runs no hook (ADR 0012).
func TestCommittedChangeGetsNoHookFromRepair(t *testing.T) {
	r := hookedAt(t, false)
	e := r.engine()
	e.fault = crashAt("promote")
	if err := e.Apply(context.Background(), r.plan(e, Request{}), ApplyOptions{}); !errors.Is(err, errCrash) {
		t.Fatalf("no crash: %v", err)
	}
	r.hooks.ran = nil
	items := r.engine().Repair(context.Background())
	if len(items) != 1 || items[0].Result != ResultRepaired || !strings.Contains(items[0].Reason, "bookkeeping") {
		t.Fatalf("repair = %+v", items)
	}
	if len(r.hooks.ran) != 0 {
		t.Errorf("hooks = %+v", r.hooks.ran)
	}
}

func TestHookFailureDuringRepair(t *testing.T) {
	for _, mode := range []string{"warn", "fail"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t, hookedFile+"hook_fail = \""+mode+"\"\n")
			r.publish("app", "1.0.0", 30, v1Files...)
			e := r.engine()
			e.fault = crashAt("record")
			if err := e.Apply(context.Background(), r.plan(e, Request{InstallMissing: true}), ApplyOptions{}); !errors.Is(err, errCrash) {
				t.Fatalf("no crash: %v", err)
			}
			r.hooks.fail = map[string]error{"post_install": errors.New("exit status 3")}
			items := r.engine().Repair(context.Background())
			if len(items) != 1 {
				t.Fatalf("repair = %+v", items)
			}
			it := items[0]
			// The repair itself is done either way: the files and state are the new version's.
			if r.state().Packages["app"].Version != "1.0.0" || journal.Exists(r.paths.Journal("app")) {
				t.Errorf("not repaired: state %+v", r.state().Packages["app"])
			}
			if mode == "warn" && (it.Result != ResultRepaired || !strings.Contains(strings.Join(it.Notes, "\n"), "exit status 3")) {
				t.Errorf("item = %+v", it)
			}
			if mode == "fail" && (it.Result != ResultFailed || it.Err == nil || !strings.Contains(it.Err.Error(), "post_install hook failed")) {
				t.Errorf("item = %+v", it)
			}
		})
	}
}

// A package taken out of the package file is finished forward without a hook (there is none to
// run), not refused.
func TestForwardRepairOfAPackageNoLongerInTheFile(t *testing.T) {
	r := hookedAt(t, false)
	e := r.engine()
	e.fault = crashAt("record")
	if err := e.Apply(context.Background(), r.plan(e, Request{}), ApplyOptions{}); !errors.Is(err, errCrash) {
		t.Fatalf("no crash: %v", err)
	}
	r.writeFile("schema_version = 1\n")
	r.hooks.ran = nil
	items := r.engine().Repair(context.Background())
	if len(items) != 1 || items[0].Result != ResultRepaired || len(r.hooks.ran) != 0 {
		t.Fatalf("repair = %+v, hooks %+v", items, r.hooks.ran)
	}
	if v := r.state().Packages["app"].Version; v != "2.0.0" {
		t.Errorf("state says %s", v)
	}
}
