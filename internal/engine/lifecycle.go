package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/enls/gremloader/internal/events"
	"github.com/enls/gremloader/internal/journal"
	"github.com/enls/gremloader/internal/pkgfile"
	"github.com/enls/gremloader/internal/state"
)

// ActRollback and ActRemove are the actions of Rollback and Remove (journal actions too).
const (
	ActRollback Action = "rollback"
	ActRemove   Action = "remove"
)

// preflight is what every change to an installed package checks first. It returns the record, or
// marks it and returns nil.
func (e *Engine) preflight(it *Item) *state.Package {
	rec := e.installed(it.Package)
	if rec == nil {
		it.fail(fmt.Errorf("%s is not installed", it.Package))
		return nil
	}
	it.From, it.Dir, it.Shared = rec.Version, rec.Dir, rec.SharedDir
	if err := e.tidy(it.Package); err != nil {
		it.fail(err)
		return nil
	}
	if err := e.unfinished(it.Package); err != nil {
		it.fail(err)
		return nil
	}
	var own []string
	if rec.SharedDir {
		var err error
		if own, err = e.ownFiles(rec); err != nil {
			it.fail(err)
			return nil
		}
	}
	procs, err := e.running(rec.Dir, rec.SharedDir, own)
	if err != nil {
		it.fail(fmt.Errorf("checking for running programs: %w", err))
		return nil
	}
	if len(procs) > 0 {
		skipRunning(it, procs)
		return nil
	}
	return rec
}

// Rollback returns a package to the version its last update replaced (plan E.3.3): the update's
// journal, read backwards, becomes a change of its own, which is in turn the record that rolls
// forward again. The version rolled away from is marked rejected so the next scheduled run does
// not reinstall it (P3-2); rolling forward lifts that.
func (e *Engine) Rollback(ctx context.Context, name string) *Item {
	it, _ := e.rollback(ctx, name)
	return it
}

// rollback returns a simulated crash (tests only) as an error; every other outcome is on the item.
func (e *Engine) rollback(ctx context.Context, name string) (*Item, error) {
	it := &Item{Package: name, Action: ActRollback}
	rec := e.preflight(it)
	if rec == nil {
		return it, nil
	}
	if rec.Previous == nil {
		it.fail(fmt.Errorf("%s %s has no earlier version to go back to", name, rec.Version))
		return it, nil
	}
	l, err := journal.Read(e.cfg.Paths.Previous(name))
	if err != nil {
		it.fail(fmt.Errorf("the rollback record cannot be read: %w", err))
		return it, nil
	}
	cr, err := readChange(l)
	if err != nil || cr.Before == nil || cr.Before.Version != rec.Previous.Version || !l.Committed() {
		it.fail(fmt.Errorf("the rollback record in %s does not describe %s %s", l.Dir, name, rec.Previous.Version))
		return it, nil
	}
	// P3-3: every saved file must still be what was saved, before anything is touched.
	for _, r := range l.Records {
		if r.Op != journal.OpBackup && r.Op != journal.OpRemoved {
			continue
		}
		if sum, _, err := hashFile(l.BackupPath(r)); err != nil || sum != r.SHA256 {
			it.fail(&VerifyError{Package: name, Msg: fmt.Sprintf("the saved copy of %s (%s) is missing or damaged; "+
				"rollback refused, the installed %s is untouched", r.Path, l.BackupPath(r), rec.Version)})
			return it, nil
		}
	}

	target := *cr.Before
	it.To = target.Version
	now := e.now()
	target.InstalledAt = &now
	target.Previous = &state.Previous{Version: rec.Version, Undo: previousUndo(name), Manifest: rec.Manifest}
	target.Rejected = nil
	for _, s := range rec.Rejected {
		if s != target.Version {
			target.Rejected = append(target.Rejected, s)
		}
	}
	if !slices.Contains(target.Rejected, rec.Version) {
		target.Rejected = append(target.Rejected, rec.Version)
	}

	ops := inverse(l)
	var placed []string
	for _, o := range ops {
		if o.kind == opPlace {
			placed = append(placed, o.path)
		}
	}
	if err := e.ownedByOthers(name, rec.Dir, placed); err != nil {
		it.fail(err)
		return it, nil
	}
	if err := e.change(ctx, it, &target, nil, ops); err != nil {
		if errors.Is(err, errCrash) {
			return it, err
		}
		it.fail(err)
		return it, nil
	}
	it.Result = ResultRolledBack
	e.emit(events.Event{Level: events.Info, Event: "rollback", Package: name, From: it.From, To: it.To})
	if pkg := e.cfg.Packages.Packages[name]; pkg != nil {
		it.Release.Tag = target.Tag
		e.runHook(ctx, it, pkg, pkgfile.PostUpdate, string(ActRollback))
	}
	return it, nil
}

// inverse is the change that undoes a committed journal, done as a change of its own: saved files
// are placed back as links or copies (the record must stay whole until this change commits), and
// whatever they replace is saved in turn.
func inverse(l *journal.Log) []op {
	var ops []op
	for _, r := range slices.Backward(l.Records) {
		switch r.Op {
		case journal.OpCreated:
			ops = append(ops, op{kind: opRemove, path: r.Path})
		case journal.OpBackup, journal.OpRemoved:
			ops = append(ops, op{kind: opPlace, path: r.Path, src: l.BackupPath(r)})
		case journal.OpMkdir:
			ops = append(ops, op{kind: opRmdir, path: r.Path})
		case journal.OpRmdir:
			ops = append(ops, op{kind: opMkdir, path: r.Path})
		}
	}
	return ops
}

// RemoveOptions are the choices of a remove.
type RemoveOptions struct {
	// DeleteDir deletes the whole install folder of a package installed without a file list
	// (adopted): GremLoader cannot otherwise tell its files from the user's.
	DeleteDir bool
}

// Remove uninstalls a package (plan E.3.3): pre_remove hook, then the files of its manifest and
// the folders GremLoader created that are now empty, then post_remove. Everything else in the
// folder (settings, data) stays. What GremLoader kept about the package (rollback record,
// manifests, rejections) is cleared.
func (e *Engine) Remove(ctx context.Context, name string, o RemoveOptions) *Item {
	it, _ := e.remove(ctx, name, o)
	return it
}

func (e *Engine) remove(ctx context.Context, name string, o RemoveOptions) (*Item, error) {
	it := &Item{Package: name, Action: ActRemove}
	rec := e.preflight(it)
	if rec == nil {
		return it, nil
	}
	// A rollback record with no state pointer to it is what an --overlay install keeps of the files
	// it overwrote (decision 1 of the M5 bug check); the remove discards it, so say so.
	overlay := rec.Previous == nil && journal.Exists(e.cfg.Paths.Previous(name))
	// A shared folder is never deleted whole, and a package in one must know its own files.
	switch {
	case rec.SharedDir && rec.Manifest == "":
		it.fail(fmt.Errorf("%s was recorded without a file list in %s, a shared folder (shared_dir), so its files "+
			"cannot be told from the others there; nothing was removed", name, rec.Dir))
		return it, nil
	case rec.SharedDir && o.DeleteDir:
		it.fail(fmt.Errorf("%s is a shared folder (shared_dir), so --delete-dir is refused: it would delete what other "+
			"packages and you keep there; without it, remove deletes only the files of %s", rec.Dir, name))
		return it, nil
	}
	pkg := e.cfg.Packages.Packages[name]
	if pkg != nil {
		if err := e.hookMust(ctx, it, pkg, pkgfile.PreRemove); err != nil {
			it.fail(fmt.Errorf("the pre_remove hook failed, nothing was removed: %w", err))
			return it, nil
		}
	}

	if rec.Manifest == "" {
		if !o.DeleteDir {
			it.fail(fmt.Errorf("%s was adopted without a file list, so its files cannot be told from yours; "+
				"`--delete-dir` deletes the whole folder %s", name, rec.Dir))
			return it, nil
		}
		notes, err := deleteDir(rec.Dir)
		if err != nil {
			it.fail(err)
			return it, nil
		}
		it.Notes = append(it.Notes, notes...)
		e.forget(name)
		if err := e.saveState(); err != nil {
			it.fail(err)
			return it, nil
		}
	} else {
		m, err := state.LoadManifest(state.Path(e.cfg.Paths.State, rec.Manifest))
		if err != nil {
			it.fail(fmt.Errorf("the installed version's manifest: %w", err))
			return it, nil
		}
		var ops []op
		for _, f := range m.Files {
			ops = append(ops, op{kind: opRemove, path: f.Path})
		}
		dirs := slices.Clone(m.Dirs)
		slices.SortFunc(dirs, func(a, b string) int { return -depthThenName(a, b) })
		for _, d := range dirs {
			ops = append(ops, op{kind: opRmdir, path: d})
		}
		if err := e.change(ctx, it, nil, nil, ops); err != nil {
			if errors.Is(err, errCrash) {
				return it, err
			}
			it.fail(err)
			return it, nil
		}
		e.forget(name)
		if err := e.saveState(); err != nil {
			it.fail(err)
			return it, nil
		}
	}
	switch {
	case overlay && rec.SharedDir:
		it.notef("discarded the backups of the files its install replaced in the shared folder (%s)", e.cfg.Paths.Previous(name))
	case overlay:
		it.notef("discarded the backups of the files the --overlay install overwrote (%s)", e.cfg.Paths.Previous(name))
	}
	it.Result = ResultRemoved
	e.emit(events.Event{Level: events.Info, Event: "remove", Package: name, From: it.From})
	if pkg != nil {
		it.To = it.From
		e.runHook(ctx, it, pkg, pkgfile.PostRemove, string(ActRemove))
	}
	return it, nil
}

// hookMust runs a hook whose failure stops the operation (a pre hook).
func (e *Engine) hookMust(ctx context.Context, it *Item, pkg *pkgfile.Package, event string) error {
	cmds, err := pkg.Hooks(event, pkgfile.Values{Version: it.From, Platform: e.cfg.Platform})
	if err != nil || len(cmds) == 0 {
		return err
	}
	if e.cfg.Hooks == nil {
		it.notef("the %s hook was not run (this build does not run hooks)", event)
		return nil
	}
	return e.cfg.Hooks.Run(ctx, Hook{Package: it.Package, Event: event, Action: string(it.Action), Version: it.From,
		Dir: it.Dir, Commands: cmds, Timeout: pkg.HookTimeout})
}

// deleteDir removes a folder whole: first renamed aside (so it is gone, or not, in one step), then
// deleted. Once it is aside the removal has happened, so what could not be deleted comes back as
// notes, not as an error. A folder that is already gone while the folder holding it is there was
// deleted by an earlier attempt that died before it was recorded; that attempt's leftovers go now.
func deleteDir(dir string) (notes []string, err error) {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if _, perr := os.Stat(filepath.Dir(dir)); perr != nil {
			return nil, fmt.Errorf("deleting %s: neither it nor the folder holding it is there (a drive not connected?): %w", dir, perr)
		}
	} else {
		aside, err := tempName(dir)
		if err != nil {
			return nil, err
		}
		if err := rename(dir, aside); err != nil {
			return nil, fmt.Errorf("deleting %s: %w", dir, err)
		}
	}
	parent, base := filepath.Split(dir)
	asides, _ := filepath.Glob(filepath.Join(parent, "."+base+".gremloader-*.tmp"))
	for _, a := range asides {
		if fi, err := os.Lstat(a); err == nil && fi.IsDir() {
			if err := os.RemoveAll(a); err != nil {
				notes = append(notes, fmt.Sprintf("%s could not be deleted completely (%v); delete it by hand", a, err))
			}
		}
	}
	return notes, nil
}
