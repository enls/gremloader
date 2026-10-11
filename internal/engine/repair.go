package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/enls/gremloader/internal/events"
	"github.com/enls/gremloader/internal/journal"
	"github.com/enls/gremloader/internal/state"
)

// Repair finishes or reverses every interrupted change (plan E.3.3). A change whose files were all
// in place (its journal reached the "state" step) is finished forward; any other is undone. A
// committed change only has its bookkeeping completed. Repair is idempotent: run it again after
// any failure, including one of its own.
func (e *Engine) Repair() []*Item {
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
			out = append(out, e.repairOne(d.Name()))
		}
	}
	return out
}

func (e *Engine) repairOne(name string) *Item {
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
		it.Result, it.Err = ResultNeedsRepair, fmt.Errorf("%w; it needs a look by hand: %s", err, jdir)
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
			"connect it and run repair again", b.Dir)
		return it
	}

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
	return it
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
