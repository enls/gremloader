package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/enls/gremloader/internal/events"
	"github.com/enls/gremloader/internal/journal"
	"github.com/enls/gremloader/internal/paths"
	"github.com/enls/gremloader/internal/pkgfile"
	"github.com/enls/gremloader/internal/state"
)

// Repair finishes or reverses every interrupted change (plan E.3.3). A change whose files were all
// in place (its journal reached the "state" step) is finished forward; any other is undone. A
// committed change only has its bookkeeping completed. Repair is idempotent: run it again after
// any failure, including one of its own.
func (e *Engine) Repair(ctx context.Context) []*Item {
	// The run lock is held, so no staging area belongs to a running change: a crash during a
	// download or an unpack (before any journal exists) leaves one behind.
	if err := os.RemoveAll(filepath.Join(e.cfg.Paths.State, "staging")); err != nil {
		return []*Item{{Package: "(all)", Result: ResultFailed, Err: err}}
	}
	ents, err := os.ReadDir(filepath.Join(e.cfg.Paths.State, "journal"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return []*Item{{Package: "(all)", Result: ResultFailed, Err: err}}
	}
	var out []*Item
	for _, d := range ents {
		if d.IsDir() {
			out = append(out, e.repairOne(ctx, d.Name()))
		}
	}
	return out
}

func (e *Engine) repairOne(ctx context.Context, name string) *Item {
	it := &Item{Package: name}
	jdir := e.cfg.Paths.Journal(name)
	_ = os.RemoveAll(e.cfg.Paths.Staging(name))
	if !journal.Begun(jdir) {
		// No log, or no complete begin record: Create died before its first record. Nothing changed.
		if err := os.RemoveAll(jdir); err != nil {
			it.fail(err)
			return it
		}
		it.Result, it.Reason = ResultRepaired, "removed a journal that was never begun"
		return it
	}
	l, err := journal.Read(jdir)
	if err != nil {
		it.Result, it.Err = ResultNeedsRepair, fmt.Errorf("%w; it needs a look by hand: %s, or `gremloader repair --forget %s` gives the change up", err, jdir, name)
		return it
	}
	b := l.Begin()
	if b.Pkg != name {
		it.Result, it.Err = ResultNeedsRepair, fmt.Errorf("the journal in %s is about package %q; it needs a look by hand", jdir, b.Pkg)
		return it
	}
	it.Action, it.From, it.To, it.Dir = Action(b.Action), b.From, b.To, b.Dir
	if fi, err := os.Stat(b.Dir); !l.Committed() && (err != nil || !fi.IsDir()) {
		// A drive not connected, most likely. Undo would find nothing to undo and call that
		// success, or recreate the folder on whatever is mounted there now.
		it.Result, it.Err = ResultNeedsRepair, fmt.Errorf("the install folder %s is not there (a drive not connected?); "+
			"connect it and run repair again; if it is gone for good, `gremloader repair --forget %s` gives the change up", b.Dir, name)
		return it
	}

	forward := false
	switch {
	case l.Committed():
		if err := e.tidy(name); err != nil {
			it.fail(err)
			return it
		}
		it.Result, it.Reason = ResultRepaired, "finished the bookkeeping of a completed change"
	case l.HasStep("state") && !l.Aborted():
		if err := e.finishForward(name, l); err != nil {
			it.Result, it.Err = ResultNeedsRepair, err
			return it
		}
		it.Result, it.Reason = ResultRepaired, fmt.Sprintf("every file of the %s was in place; finished it", b.Action)
		forward = true
	default:
		if err := e.undo(l); err != nil {
			it.Result, it.Err = ResultNeedsRepair, fmt.Errorf("undoing the interrupted %s: %w", b.Action, err)
			return it
		}
		if err := os.RemoveAll(jdir); err != nil {
			it.Result, it.Err = ResultNeedsRepair, err
			return it
		}
		it.Result, it.Reason = ResultRepaired, fmt.Sprintf("undid the interrupted %s; %s is as it was", b.Action, b.Dir)
	}
	it.Action = ActNone
	e.emit(events.Event{Level: events.Info, Event: "repair", Package: name, From: b.From, To: b.To, Msg: it.Reason})
	if forward {
		e.skippedHook(ctx, it, l)
	}
	return it
}

// skippedHook runs the post hook of a change that repair finished forward: the run that made the
// change died between its files and its hook (decision 4 of the M5 bug check). The hook runs as
// the action that was interrupted. A change that had already committed gets no hook from repair:
// nothing records whether its hook ran, so hooks must tolerate a rare miss or double run (ADR 0012).
func (e *Engine) skippedHook(ctx context.Context, it *Item, l *journal.Log) {
	pkg := e.cfg.Packages.Packages[it.Package]
	if pkg == nil {
		return // no longer in the package file: no hook to run
	}
	b := l.Begin()
	event := pkgfile.PostUpdate // update, downgrade, reinstall, rollback
	switch Action(b.Action) {
	case ActInstall:
		event = pkgfile.PostInstall
	case ActRemove:
		event, it.To = pkgfile.PostRemove, it.From // as remove itself runs it
	}
	if cr, err := readChange(l); err == nil && cr.After != nil {
		it.Release.Tag = cr.After.Tag
	}
	e.runHook(ctx, it, pkg, event, b.Action)
}

// finishForward completes a change whose files are all in place: the manifest and state record it
// was going to write are in the journal.
func (e *Engine) finishForward(name string, l *journal.Log) error {
	cr, err := readChange(l)
	if err != nil {
		return err
	}
	result := cr.After
	var m *state.Manifest
	// An install or update carries its new manifest; a rollback goes back to one that exists.
	if mp := filepath.Join(l.Dir, journal.ManifestName); result != nil && result.Manifest != "" && fileExists(mp) {
		var err error
		if m, err = state.LoadManifest(mp); err != nil {
			return fmt.Errorf("the journal's manifest: %w", err)
		}
	}
	j, _, err := journal.Resume(l.Dir)
	if err != nil {
		return err
	}
	if err := e.commit(name, j, result, m); err != nil {
		return err
	}
	if result == nil {
		e.forget(name)
		return e.saveState()
	}
	return nil
}

// forget removes everything GremLoader keeps about an installed package (after a remove).
func (e *Engine) forget(name string) {
	if rec := e.st.Packages[name]; rec != nil {
		e.setRecord(name, nil)
		rec.Rejected = nil
	}
	for _, d := range []string{e.cfg.Paths.Previous(name), e.cfg.Paths.Manifests(name), e.quarantineDir(name)} {
		_ = os.RemoveAll(d)
	}
}

// ResultForgotten: an interrupted change was given up (`repair --forget`).
const ResultForgotten Result = "forgotten"

// Forget gives up a package's interrupted change without finishing or undoing it (`repair
// --forget`, decision 5 of the M5 bug check). It is the way out for a journal whose install folder
// is gone for good, or that cannot be read. The journal and the staging area are deleted. When the
// recorded install folder is not there, the package is recorded as not installed; when it is,
// the record stays, and the folder may hold files of both versions. A journal that had committed
// is not given up: its bookkeeping is finished, as repair does, so the rollback record survives.
func (e *Engine) Forget(name string) *Item {
	it := &Item{Package: name}
	if !paths.ValidPackageName(name) {
		it.fail(fmt.Errorf("package name %q is not valid", name))
		return it
	}
	jdir := e.cfg.Paths.Journal(name)
	if _, err := os.Stat(jdir); err != nil {
		it.fail(fmt.Errorf("%s has no unfinished change to give up", name))
		return it
	}
	what, dir := "unfinished change", ""
	if l, err := journal.Read(jdir); err == nil {
		b := l.Begin()
		what, dir = "interrupted "+b.Action, b.Dir
		it.From, it.To, it.Dir = b.From, b.To, b.Dir
		if l.Committed() {
			// The change completed (a crash before its journal became the rollback record): there
			// is nothing to give up, and deleting the journal would lose the only rollback record.
			// Finish its bookkeeping as repair does.
			_ = os.RemoveAll(e.cfg.Paths.Staging(name))
			if err := e.tidy(name); err != nil {
				it.fail(err)
				return it
			}
			it.Result, it.Reason = ResultRepaired, "the change had completed; finished its bookkeeping instead of giving it up"
			e.emit(events.Event{Level: events.Info, Event: "repair", Package: name, From: b.From, To: b.To, Msg: it.Reason})
			return it
		}
	}
	rec := e.installed(name)
	if rec != nil {
		dir = rec.Dir
	}
	if err := os.RemoveAll(jdir); err != nil {
		it.fail(err)
		return it
	}
	_ = os.RemoveAll(e.cfg.Paths.Staging(name))

	there := false
	if fi, err := os.Stat(dir); dir != "" && err == nil && fi.IsDir() {
		there = true
	}
	switch {
	case rec == nil:
		it.Reason = fmt.Sprintf("gave up the %s; %s was not installed", what, name)
		if there {
			it.Reason += fmt.Sprintf(", and %s may hold some of its files", dir)
		}
	case !there:
		e.forget(name)
		if err := e.saveState(); err != nil {
			it.fail(err)
			return it
		}
		it.Reason = fmt.Sprintf("gave up the %s; %s is not there, so %s is now recorded as not installed", what, dir, name)
	default:
		it.Reason = fmt.Sprintf("gave up the %s; %s stays recorded as %s, but %s may hold files of both versions "+
			"(`gremloader verify %s` lists them)", what, name, rec.Version, dir, name)
	}
	it.Result = ResultForgotten
	e.emit(events.Event{Level: events.Warn, Event: "repair", Package: name, Result: string(it.Result), Msg: it.Reason})
	return it
}
