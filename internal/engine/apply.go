package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/enls/gremloader/internal/archive"
	"github.com/enls/gremloader/internal/checksum"
	"github.com/enls/gremloader/internal/download"
	"github.com/enls/gremloader/internal/events"
	"github.com/enls/gremloader/internal/journal"
	"github.com/enls/gremloader/internal/pkgfile"
	"github.com/enls/gremloader/internal/state"
	"github.com/enls/gremloader/internal/ver"
)

// maxChecksumFile bounds a checksum file download (the parser's own limit).
const maxChecksumFile = 4 << 20

// ApplyOptions are choices for a whole apply.
type ApplyOptions struct {
	// Accept lets one download through whose hash differs from the trusted one (plan E.3.4). It
	// comes only from the command line, after the user checked the hash elsewhere.
	Accept *Acceptance
}

// Acceptance is `install|update pkg@VER --accept-sha256 HASH`: for one package and one version,
// and only if HASH is exactly the hash of the bytes received.
type Acceptance struct {
	Package string
	Version string
	SHA256  string
}

// Apply carries out the plan's items in order, updating each with its outcome. Packages are
// independent: one failing does not stop the next. state.json is saved after every package.
func (e *Engine) Apply(ctx context.Context, p *Plan, o ApplyOptions) error {
	if a := o.Accept; a != nil {
		var acting []*Item
		for _, it := range p.Items {
			if it.Action != ActNone {
				acting = append(acting, it)
			}
		}
		if len(acting) != 1 || acting[0].Package != a.Package || acting[0].To != a.Version {
			return fmt.Errorf("--accept-sha256 applies to exactly one package and version (%s %s); this run would change %d package(s)",
				a.Package, a.Version, len(acting))
		}
	}
	for _, it := range p.Items {
		if it.Action == ActNone {
			continue
		}
		if err := ctx.Err(); err != nil {
			it.fail(fmt.Errorf("not started: %w", err))
			continue
		}
		if err := e.applyOne(ctx, it, o); errors.Is(err, errCrash) {
			return err // tests: the process "died" here
		}
	}
	return nil
}

// applyOne runs plan E.3.2 for one package. It returns only errCrash; every other outcome is
// recorded on the item.
func (e *Engine) applyOne(ctx context.Context, it *Item, o ApplyOptions) error {
	pkg := e.cfg.Packages.Packages[it.Package]
	started := e.now()
	e.emit(events.Event{Level: events.Info, Event: "apply.begin", Package: it.Package, From: it.From, To: it.To,
		Step: string(it.Action)})
	stage := e.cfg.Paths.Staging(it.Package)
	failed := func(err error) error {
		if errors.Is(err, errCrash) {
			return err
		}
		_ = os.RemoveAll(stage)
		it.fail(err)
		e.emit(events.Event{Level: events.Error, Event: "apply.abort", Package: it.Package, To: it.To,
			Result: string(it.Result), Msg: err.Error(), DurationMS: e.now().Sub(started).Milliseconds()})
		return nil
	}

	// 0. Preflight. Nothing on disk changes until every check has passed.
	if err := e.at("preflight"); err != nil {
		return failed(err)
	}
	if err := e.tidy(it.Package); err != nil {
		return failed(err)
	}
	if err := e.unfinished(it.Package); err != nil {
		return failed(err)
	}
	if procs, err := e.cfg.Running(it.Dir); err != nil {
		return failed(fmt.Errorf("checking for running programs: %w", err))
	} else if len(procs) > 0 {
		var names []string
		for _, p := range procs {
			names = append(names, fmt.Sprintf("%s (pid %d)", filepath.Base(p.Path), p.PID))
		}
		it.Action, it.Result = ActNone, ResultSkipped
		it.Reason = "running: " + strings.Join(names, ", ") + "; close it and run again"
		e.emit(events.Event{Level: events.Warn, Event: "apply.abort", Package: it.Package, Result: string(it.Result), Msg: it.Reason})
		return nil
	}
	if err := e.checkSpace(e.cfg.Paths.State, it.Asset.Size); err != nil {
		return failed(err)
	}

	work := filepath.Join(stage, it.To)
	if err := os.RemoveAll(stage); err != nil {
		return failed(err)
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return failed(err)
	}

	// 1-3. Download, verify, extract into staging; once more after a hash mismatch or a damaged
	// archive that nothing had verified (plan E.3.4, E.9).
	st, err := e.stage(ctx, it, pkg, work, o)
	if err != nil {
		return failed(err)
	}
	got, verifiedBy, ar, tree := st.got, st.levels, st.ar, st.tree

	// 4-6. Change the install, journaled.
	rec := e.record(it.Package)
	var oldFiles, oldDirs []string
	if it.From != "" && rec.Manifest != "" {
		m, err := state.LoadManifest(state.Path(e.cfg.Paths.State, rec.Manifest))
		if err != nil {
			return failed(fmt.Errorf("the installed version's manifest: %w", err))
		}
		for _, f := range m.Files {
			oldFiles = append(oldFiles, f.Path)
		}
		oldDirs = m.Dirs
	}
	createdDir, err := ensureDir(it.Dir)
	if err != nil {
		return failed(err)
	}
	newManifest := manifestOf(ar, it.Dir, oldDirs)
	result := e.installedRecord(it, rec, verifiedBy, got.SHA256)
	if err := e.change(ctx, it, result, newManifest, manifestOps(fileList(ar), ar.Dirs, tree, oldFiles, oldDirs)); err != nil {
		if errors.Is(err, errCrash) {
			return err
		}
		if createdDir && isEmptyDir(it.Dir) {
			_ = os.Remove(it.Dir)
		}
		return failed(err)
	}
	_ = os.RemoveAll(stage)

	it.Result = ResultUpdated
	event := pkgfile.PostUpdate
	if it.Action == ActInstall {
		it.Result, event = ResultInstalled, pkgfile.PostInstall
	}
	e.emit(events.Event{Level: events.Info, Event: "apply.commit", Package: it.Package, From: it.From, To: it.To,
		Result: string(it.Result), Bytes: got.Size, DurationMS: e.now().Sub(started).Milliseconds()})

	// 8. Hooks.
	if err := e.at("hook"); err != nil && errors.Is(err, errCrash) {
		return err
	}
	e.runHook(ctx, it, pkg, event, string(it.Action))
	return nil
}

// change journals and applies ops to it.Dir and records the result (steps 4 to 7). On failure the
// journal is undone; if the undo fails too, the error is a RepairError and the journal stays.
func (e *Engine) change(ctx context.Context, it *Item, result *state.Package, m *state.Manifest, ops []op) error {
	var raw json.RawMessage
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			return err
		}
		raw = b
	}
	jdir := e.cfg.Paths.Journal(it.Package)
	j, err := journal.Create(jdir, journal.Record{
		Op: journal.OpBegin, ID: newID(), Pkg: it.Package, Action: string(it.Action), From: it.From, To: it.To,
		Dir: it.Dir, TS: e.now(), Result: raw,
	})
	if err != nil {
		return err
	}
	if m != nil {
		if err := state.SaveManifest(filepath.Join(jdir, journal.ManifestName), m); err != nil {
			_ = j.Close()
			_ = os.RemoveAll(jdir)
			return err
		}
	}

	x := &executor{e: e, j: j, dir: it.Dir}
	err = x.run(ctx, ops)
	if err == nil {
		err = x.append(journal.Record{Op: journal.OpStep, Name: "state"})
	}
	if err != nil {
		_ = j.Close()
		if errors.Is(err, errCrash) {
			return err
		}
		return e.rollbackChange(it.Package, err)
	}

	// 7. Record: from here on the change is complete, and an interruption finishes forward.
	if err := e.at("record"); err != nil {
		_ = j.Close()
		if errors.Is(err, errCrash) {
			return err
		}
		return e.rollbackChange(it.Package, err)
	}
	if err := e.commit(it.Package, j, result, m); err != nil {
		return err
	}
	return nil
}

// commit writes the manifest and state, then the commit record, then promotes the journal. A
// failure after the state is written is reported but leaves a journal that repair finishes.
func (e *Engine) commit(name string, j *journal.Journal, result *state.Package, m *state.Manifest) error {
	defer func() { _ = j.Close() }()
	if result != nil && m != nil {
		p := state.Path(e.cfg.Paths.State, result.Manifest)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return &RepairError{Package: name, Msg: err.Error()}
		}
		if err := state.SaveManifest(p, m); err != nil {
			return &RepairError{Package: name, Msg: err.Error()}
		}
	}
	e.setRecord(name, result)
	if err := e.saveState(); err != nil {
		return &RepairError{Package: name, Msg: fmt.Sprintf("the files are in place but state.json could not be written (%v); run `gremloader repair`", err)}
	}
	if err := e.at("commit"); err != nil {
		if errors.Is(err, errCrash) {
			return err
		}
		return &RepairError{Package: name, Msg: err.Error()}
	}
	if err := j.Append(journal.Record{Op: journal.OpCommit}); err != nil {
		return &RepairError{Package: name, Msg: err.Error()}
	}
	_ = j.Close()
	if err := e.at("promote"); err != nil && errors.Is(err, errCrash) {
		return err
	}
	if err := e.tidy(name); err != nil {
		// Committed: the install is complete. The next run finishes the bookkeeping.
		e.emit(events.Event{Level: events.Warn, Event: "apply.step", Package: name, Step: "promote", Msg: err.Error()})
	}
	e.pruneManifests(name)
	return nil
}

// rollbackChange undoes an uncommitted journal after a failure. The cause is returned when the
// undo worked; a RepairError when it did not.
func (e *Engine) rollbackChange(name string, cause error) error {
	jdir := e.cfg.Paths.Journal(name)
	l, err := journal.Read(jdir)
	if err != nil {
		return &RepairError{Package: name, Msg: fmt.Sprintf("%v; and its journal cannot be read to undo it: %v", cause, err)}
	}
	if err := e.undo(l); err != nil {
		if errors.Is(err, errCrash) {
			return err
		}
		e.emit(events.Event{Level: events.Error, Event: "apply.abort", Package: name, Step: "undo", Msg: err.Error()})
		return &RepairError{Package: name, Msg: fmt.Sprintf("%v; undoing it failed too (%v): run `gremloader repair`", cause, err)}
	}
	if err := os.RemoveAll(jdir); err != nil {
		return &RepairError{Package: name, Msg: fmt.Sprintf("%v; undone, but its journal could not be removed: %v", cause, err)}
	}
	return cause
}

// tidy finishes the bookkeeping of a committed journal: it becomes the package's rollback record
// (previous/), or is discarded when there is nothing to roll back to. Safe to repeat at any point.
func (e *Engine) tidy(name string) error {
	jdir, pdir := e.cfg.Paths.Journal(name), e.cfg.Paths.Previous(name)
	old := pdir + ".old"
	if journal.Exists(jdir) {
		l, err := journal.Read(jdir)
		if err != nil || !l.Committed() {
			return nil // not ours to tidy: unfinished() reports it
		}
		if !keepsJournal(l) {
			if err := os.RemoveAll(jdir); err != nil {
				return err
			}
		} else {
			if _, err := os.Stat(pdir); err == nil {
				if err := os.RemoveAll(old); err != nil {
					return err
				}
				if err := rename(pdir, old); err != nil {
					return err
				}
			}
			if err := os.MkdirAll(filepath.Dir(pdir), 0o700); err != nil {
				return err
			}
			if err := rename(jdir, pdir); err != nil {
				return err
			}
		}
	}
	return os.RemoveAll(old)
}

// keepsJournal says whether a committed change becomes the rollback record. An update keeps it (it
// restores the version before); an install keeps it only when it replaced files (--overlay), so
// the user's overwritten files are not lost; a reinstall or a remove does not.
func keepsJournal(l *journal.Log) bool {
	switch l.Begin().Action {
	case string(ActUpdate), string(ActDowngrade), "rollback":
		return true
	case string(ActInstall):
		return slices.ContainsFunc(l.Records, func(r journal.Record) bool {
			return r.Op == journal.OpBackup || r.Op == journal.OpRemoved
		})
	}
	return false
}

// installedRecord is the state record a successful install of it will leave.
func (e *Engine) installedRecord(it *Item, rec *state.Package, verifiedBy []string, assetSHA string) *state.Package {
	now := e.now()
	r := &state.Package{
		Version: it.To, Tag: it.Release.Tag, Dir: it.Dir, Asset: it.Asset.Name, AssetSHA256: assetSHA,
		InstalledAt: &now, VerifiedBy: verifiedBy, Manifest: state.ManifestRel(it.Package, it.To),
	}
	switch it.Action {
	case ActUpdate, ActDowngrade:
		r.Previous = &state.Previous{Version: it.From, Undo: previousUndo(it.Package), Manifest: rec.Manifest}
	case ActReinstall:
		r.Previous = rec.Previous
	}
	// A rejection is lifted once a version at or past it is installed on purpose.
	if v, err := ver.Parse(it.To); err == nil {
		for _, s := range rec.Rejected {
			if rv, err := ver.Parse(s); err == nil && rv.Compare(v) > 0 {
				r.Rejected = append(r.Rejected, s)
			}
		}
	}
	return r
}

func previousUndo(pkg string) string { return "previous/" + pkg + "/" + journal.LogName }

// setRecord replaces the install fields of the package's record with r (nil: not installed) and
// keeps what checks learned.
func (e *Engine) setRecord(name string, r *state.Package) {
	cur := e.record(name)
	keep := state.Package{LatestSeen: cur.LatestSeen, LastCheck: cur.LastCheck, LastError: cur.LastError}
	if r != nil {
		keep.Version, keep.Tag, keep.Dir, keep.Asset, keep.AssetSHA256 = r.Version, r.Tag, r.Dir, r.Asset, r.AssetSHA256
		keep.InstalledAt, keep.Adopted, keep.VerifiedBy, keep.Attested = r.InstalledAt, r.Adopted, r.VerifiedBy, r.Attested
		keep.Manifest, keep.Previous, keep.Rejected = r.Manifest, r.Previous, r.Rejected
	}
	*cur = keep
}

// pruneManifests deletes the manifests of versions that are neither installed nor the rollback
// target.
func (e *Engine) pruneManifests(name string) {
	rec := e.st.Packages[name]
	keep := map[string]bool{}
	if rec != nil {
		keep[rec.Manifest] = true
		if rec.Previous != nil {
			keep[rec.Previous.Manifest] = true
		}
	}
	dir := e.cfg.Paths.Manifests(name)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, d := range ents {
		if !keep["manifests/"+name+"/"+d.Name()] {
			_ = os.Remove(filepath.Join(dir, d.Name()))
		}
	}
}

// manifestOf describes the extracted tree. Its folders are those GremLoader may later delete when
// empty: the ones that do not exist yet, plus those an earlier version already created.
func manifestOf(ar *archive.Result, dir string, oldDirs []string) *state.Manifest {
	m := &state.Manifest{SchemaVersion: state.SchemaVersion}
	for _, f := range ar.Files {
		m.Files = append(m.Files, state.ManifestFile{Path: f.Path, Size: f.Size, SHA256: f.SHA256, Exec: f.Mode&0o111 != 0})
	}
	slices.SortFunc(m.Files, func(a, b state.ManifestFile) int { return strings.Compare(a.Path, b.Path) })
	for _, d := range ar.Dirs {
		if slices.Contains(oldDirs, d) {
			m.Dirs = append(m.Dirs, d)
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(d))); errors.Is(err, fs.ErrNotExist) {
			m.Dirs = append(m.Dirs, d)
		}
	}
	return m
}

func fileList(ar *archive.Result) []string {
	out := make([]string, len(ar.Files))
	for i, f := range ar.Files {
		out[i] = f.Path
	}
	return out
}

// ensureDir creates the install folder (and, if the plan allowed it, its parents). It reports
// whether it created it, so a failed first install can take it away again.
func ensureDir(dir string) (bool, error) {
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return false, fmt.Errorf("%s exists and is not a folder", dir)
		}
		return false, nil
	}
	return true, os.MkdirAll(dir, 0o755) //nolint:gosec // G301: an install folder; the programs in it must be readable
}

func (e *Engine) fetch(ctx context.Context, it *Item, url, dst string, limit int64) (download.Result, error) {
	e.emit(events.Event{Level: events.Info, Event: "download.start", Package: it.Package, Version: it.To, URL: url})
	start := e.now()
	r, err := e.cfg.Fetcher.Fetch(ctx, url, dst, download.FetchOptions{MaxBytes: limit})
	if err != nil {
		return r, err
	}
	e.emit(events.Event{Level: events.Info, Event: "download.done", Package: it.Package, Version: it.To, URL: url,
		Bytes: r.Size, DurationMS: e.now().Sub(start).Milliseconds()})
	return r, nil
}

func parseChecksums(p string) (*checksum.File, error) {
	f, err := os.Open(p) //nolint:gosec // G304: a file in staging, named by the engine
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return checksum.Parse(f)
}

// checkSpace refuses early when the volume clearly cannot hold the download and its unpacked
// tree (P3-11). The real guarantee is that running out of space at any later point cleans up.
func (e *Engine) checkSpace(dir string, size int64) error {
	if size <= 0 {
		return nil // unknown on the website path
	}
	free, err := e.cfg.FreeSpace(dir)
	if err != nil {
		return nil // not knowing is not a reason to refuse
	}
	need := uint64(size)*3 + 64<<20 //nolint:gosec // G115: size > 0
	if free < need {
		return fmt.Errorf("not enough free space on the volume of %s: %d MiB free, about %d MiB needed", dir, free>>20, need>>20)
	}
	return nil
}

func (e *Engine) runHook(ctx context.Context, it *Item, pkg *pkgfile.Package, event, action string) {
	cmds, err := pkg.Hooks(event, pkgfile.Values{Version: it.To, Tag: it.Release.Tag, Platform: e.cfg.Platform})
	if err != nil {
		it.notef("%s hook: %v", event, err)
		return
	}
	if len(cmds) == 0 {
		return
	}
	if e.cfg.Hooks == nil {
		it.notef("the %s hook was not run (this build does not run hooks)", event)
		return
	}
	h := Hook{Package: it.Package, Event: event, Action: action, Version: it.To, Previous: it.From, Dir: it.Dir,
		Commands: cmds, Timeout: pkg.HookTimeout}
	e.emit(events.Event{Level: events.Info, Event: "hook.run", Package: it.Package, Step: event})
	if err := e.cfg.Hooks.Run(ctx, h); err != nil {
		e.emit(events.Event{Level: events.Warn, Event: "hook.fail", Package: it.Package, Step: event, Msg: err.Error()})
		if pkg.HookFail == "fail" {
			it.Result, it.Err = ResultFailed, fmt.Errorf("installed, but the %s hook failed: %w", event, err)
			return
		}
		it.notef("the %s hook failed: %v", event, err)
	}
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
