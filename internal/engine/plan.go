package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/enls/gremloader/internal/events"
	"github.com/enls/gremloader/internal/journal"
	"github.com/enls/gremloader/internal/lock"
	"github.com/enls/gremloader/internal/pkgfile"
	"github.com/enls/gremloader/internal/source/github"
	"github.com/enls/gremloader/internal/state"
	"github.com/enls/gremloader/internal/ver"
)

// ResultUnmanaged marks a package that state.json knows and the package file does not (P3-6). It
// is reported and never touched.
const ResultUnmanaged Result = "unmanaged"

// Request says what to plan.
type Request struct {
	Packages       []string // empty: every package in the file
	InstallMissing bool     // install packages that are not installed (apply, install), not only update
	Want           string   // an explicit version; only with exactly one package
	Force          bool     // reinstall the installed version
	IncludeHeld    bool
	AllowDowngrade bool
	IgnoreCooldown bool // --now
	Overlay        bool // allow installing into a non-empty folder GremLoader does not manage
	CreateDir      bool // allow creating a missing parent folder
	Locked         *lock.File

	// forFetch: resolve and pick assets only, for adopt --verify and verify --remote, which read
	// a release without installing it (no folder rules, no drift).
	forFetch bool
}

// Plan resolves each package and says what applying would do. It touches no install, but it does
// record what it learned (latest_seen, last_check, last_error) in state.json, so `list` needs no
// network.
func (e *Engine) Plan(ctx context.Context, rq Request) (*Plan, error) {
	names := rq.Packages
	all := len(names) == 0
	if all {
		names = e.cfg.Packages.Names()
	}
	for _, n := range names {
		if e.cfg.Packages.Packages[n] == nil {
			return nil, fmt.Errorf("package %q is not in %s", n, e.cfg.Packages.File)
		}
	}
	if rq.Want != "" && len(rq.Packages) != 1 {
		return nil, errors.New("an explicit version needs exactly one package, named")
	}

	p := &Plan{}
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.Items = append(p.Items, e.planOne(ctx, n, rq))
	}
	if all {
		for _, n := range e.unmanaged() {
			p.Items = append(p.Items, &Item{Package: n, Result: ResultUnmanaged, From: e.st.Packages[n].Version,
				Reason: "installed by GremLoader but no longer in the package file; left as it is"})
		}
	}
	if err := e.saveState(); err != nil {
		return nil, err
	}
	return p, nil
}

// unmanaged lists installed packages the package file no longer names.
func (e *Engine) unmanaged() []string {
	var out []string
	for n, rec := range e.st.Packages {
		if rec.Version != "" && e.cfg.Packages.Packages[n] == nil {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

func (e *Engine) planOne(ctx context.Context, name string, rq Request) *Item {
	pkg := e.cfg.Packages.Packages[name]
	it := &Item{Package: name, Overlay: rq.Overlay, CreateDir: rq.CreateDir}
	rec := e.st.Packages[name]
	if rec != nil {
		it.From = rec.Version
	}
	if !pkg.Enabled {
		it.Result, it.Reason = ResultSkipped, reasonDisabled
		return it
	}
	if err := e.unfinished(name); err != nil {
		it.fail(err)
		return it
	}

	dir, err := pkg.Dir(e.cfg.Platform.OS, e.cfg.Home)
	if err != nil {
		it.fail(err)
		return it
	}
	it.Dir = dir
	// Once installed into a shared folder, always treated as one: that is the safe direction.
	it.Shared = pkg.SharedDir(e.cfg.Platform.OS) || rec != nil && rec.SharedDir
	if rec != nil && rec.Version != "" && rec.Dir != dir {
		// Moving an install is not something an update does. Keep working where it is.
		it.notef("the package file now says %s; the install stays in %s", dir, rec.Dir)
		it.Dir = rec.Dir
	}

	in := resolveIn{
		Channel: pkg.Channel, CooldownDays: pkg.CooldownDays, CooldownUnknown: pkg.CooldownUnknown,
		Now: e.now(), IgnoreCooldown: rq.IgnoreCooldown, Pin: pkg.Pin, Want: rq.Want,
		InstallMissing: rq.InstallMissing, Hold: pkg.Hold, IncludeHeld: rq.IncludeHeld,
		Force: rq.Force, AllowDowngrade: rq.AllowDowngrade,
	}
	if rec != nil {
		in.Installed, in.Rejected = rec.Version, rec.Rejected
	}

	if rq.Locked != nil {
		if err := e.fromLock(it, rq.Locked, &in); err != nil {
			it.fail(err)
			return it
		}
	} else {
		listing, err := e.cfg.Source.List(ctx, e.query(name, pkg))
		if err != nil {
			e.record(name).LastError = truncate(err.Error(), 4096)
			e.record(name).LastCheck = ptr(e.now())
			e.emit(events.Event{Level: events.Warn, Event: "check.fail", Package: name, Msg: err.Error()})
			it.fail(err)
			return it
		}
		in.Releases, it.Via = listing.Releases, listing.Via
		if listing.FellOver != "" {
			it.notef("%s", listing.FellOver)
		}
		it.Notes = append(it.Notes, listing.Warnings...)
	}

	d := resolve(in)
	if rq.Locked == nil {
		r := e.record(name)
		r.LastCheck, r.LastError = ptr(e.now()), ""
		if d.Newest != nil {
			seen := &state.Seen{Version: d.Newest.Version.String(), At: e.now()}
			if !d.Newest.Published.IsZero() {
				seen.PublishedAt = ptr(d.Newest.Published.UTC())
			}
			r.LatestSeen = seen
		}
		ev := events.Event{Level: events.Info, Event: "check.ok", Package: name, From: in.Installed, Result: string(d.Result)}
		if d.Newest != nil {
			ev.Version = d.Newest.Version.String()
		}
		if d.Release != nil {
			ev.To = d.Release.Version.String()
		}
		e.emit(ev)
	}
	if d.Err != nil {
		it.fail(d.Err)
		return it
	}
	it.Action, it.Result, it.Reason = d.Action, d.Result, d.Reason
	if d.Action == ActNone {
		return it
	}
	it.Release, it.To = *d.Release, d.Release.Version.String()
	it.Result = ResultPending

	if !it.Locked {
		if err := e.pickAssets(it, pkg); err != nil {
			it.fail(err)
			return it
		}
	}
	if err := e.checkVerifiable(it, pkg); err != nil {
		it.fail(err)
		return it
	}
	if rq.forFetch {
		return it
	}
	if it.From == "" {
		if err := checkInstallDir(it); err != nil {
			it.fail(err)
			return it
		}
	} else {
		e.noteDrift(it, rec)
	}
	return it
}

// unfinished reports a package whose last change did not complete (needs repair). A committed
// journal that was not yet promoted is not unfinished: Apply tidies it.
func (e *Engine) unfinished(name string) error {
	dir := e.cfg.Paths.Journal(name)
	if !journal.Begun(dir) {
		return nil
	}
	l, err := journal.Read(dir)
	if err != nil {
		return &RepairError{Package: name, Msg: fmt.Sprintf("its journal cannot be read (%v); run `gremloader repair`", err)}
	}
	if !l.Committed() {
		b := l.Begin()
		return &RepairError{Package: name, Msg: fmt.Sprintf("a %s to %s was interrupted at %s; run `gremloader repair`",
			b.Action, orNone(b.To), b.TS.Format("2006-01-02 15:04"))}
	}
	return nil
}

// query builds the source query: the package's tag prefix, minus the tags of sibling packages of
// the same repository whose prefix is longer (longest prefix wins, plan E.9).
func (e *Engine) query(name string, pkg *pkgfile.Package) github.Query {
	q := github.Query{Repo: pkg.Repo, TagPrefix: pkg.TagPrefix, Mode: github.Mode(pkg.API)}
	for _, other := range e.cfg.Packages.Names() {
		o := e.cfg.Packages.Packages[other]
		if other != name && o.Repo == pkg.Repo && len(o.TagPrefix) > len(pkg.TagPrefix) &&
			strings.HasPrefix(o.TagPrefix, pkg.TagPrefix) && !slices.Contains(q.Exclude, o.TagPrefix) {
			q.Exclude = append(q.Exclude, o.TagPrefix)
		}
	}
	// The web path cannot see GitHub's prerelease flag, an asset list (for a wildcard) or the digest.
	q.NeedAPIData = pkg.Channel == "prerelease" || slices.Contains(pkg.VerifyRequire, "digest")
	if pattern, err := pkg.AssetPattern(pkgfile.Values{Version: "0.0.0", Tag: "v0.0.0", Platform: e.cfg.Platform}); err == nil &&
		strings.ContainsAny(pattern, "*?[") {
		q.NeedAPIData = true
	}
	return q
}

// fromLock turns the lock's entry into the only candidate release, requested explicitly.
func (e *Engine) fromLock(it *Item, lf *lock.File, in *resolveIn) error {
	lp, la, err := lf.Get(it.Package, e.cfg.Platform)
	if err != nil {
		return err
	}
	v, err := ver.Parse(lp.Version)
	if err != nil {
		return fmt.Errorf("gremloader.lock: %s: %w", it.Package, err)
	}
	r := github.Release{Repo: lp.Repo, Tag: lp.Tag, Version: v}
	if lp.PublishedAt != nil {
		r.Published = *lp.PublishedAt
	}
	in.Releases, in.Want, in.Pin = []github.Release{r}, lp.Version, ""
	it.Locked, it.LockHash, it.LockSize = true, la.SHA256, la.Size
	it.Asset = github.Asset{Name: la.Asset, URL: la.URL, Size: la.Size}
	return nil
}

func (e *Engine) pickAssets(it *Item, pkg *pkgfile.Package) error {
	v := pkgfile.Values{Version: it.To, Tag: it.Release.Tag, Platform: e.cfg.Platform}
	pattern, err := pkg.AssetPattern(v)
	if err != nil {
		return err
	}
	if it.Asset, err = e.cfg.Source.SelectAsset(it.Release, pattern); err != nil {
		return err
	}
	name, ok, err := pkg.ChecksumsName(v, it.Asset.Name)
	if err != nil || !ok {
		return err
	}
	c, err := e.cfg.Source.SelectAsset(it.Release, name)
	if err != nil {
		return fmt.Errorf("checksum file: %w", err)
	}
	it.Checksums, it.Sidecar = &c, pkg.Checksums.Uses("asset")
	return nil
}

// checkVerifiable refuses up front what the verify step could not satisfy.
func (e *Engine) checkVerifiable(it *Item, pkg *pkgfile.Package) error {
	if slices.Contains(pkg.VerifyRequire, "attestation") {
		return &VerifyError{Package: it.Package, Msg: "verify.require lists attestation, which this version of GremLoader cannot check yet"}
	}
	if it.Locked {
		return nil // the lock's hash stands in for every source
	}
	if slices.Contains(pkg.VerifyRequire, "checksum") && it.Checksums == nil {
		return &VerifyError{Package: it.Package, Msg: "verify.require lists checksum but the package names no checksum file"}
	}
	if slices.Contains(pkg.VerifyRequire, "digest") && it.Asset.SHA256 == "" {
		return &VerifyError{Package: it.Package, Msg: fmt.Sprintf("verify.require lists digest but GitHub reports none for %s", it.Asset.Name)}
	}
	if len(pkg.VerifyRequire) == 0 && it.Asset.SHA256 == "" {
		it.notef("nothing will verify %s: verify.require is empty and no GitHub digest is known", it.Asset.Name)
	}
	return nil
}

// checkInstallDir applies P3-4 to a fresh install: an existing non-empty folder that GremLoader
// does not manage is refused without --overlay (a shared folder is expected to hold other
// things), and a missing parent needs --create-dir.
func checkInstallDir(it *Item) error {
	fi, err := os.Stat(it.Dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		parent := filepath.Dir(it.Dir)
		if _, perr := os.Stat(parent); errors.Is(perr, fs.ErrNotExist) && !it.CreateDir {
			return fmt.Errorf("the folder %s does not exist, nor its parent %s; check the path, or pass --create-dir", it.Dir, parent)
		}
		return nil
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("%s exists and is not a folder", it.Dir)
	}
	ents, err := os.ReadDir(it.Dir)
	if err != nil {
		return err
	}
	if len(ents) > 0 && !it.Overlay && !it.Shared {
		return fmt.Errorf("the folder %s is not empty and GremLoader did not install it; "+
			"`gremloader adopt %s` records what is there, --overlay installs over it", it.Dir, it.Package)
	}
	if len(ents) > 0 && !it.Shared {
		it.notef("installing over the existing contents of %s (--overlay); the files it overwrites are backed up, "+
			"but kept only until the next update or remove of %s", it.Dir, it.Package)
	}
	return nil
}

// noteDrift compares the install with its manifest by presence and size (P3-9). Hashing every file
// on every plan would be too slow; `gremloader verify` does that.
func (e *Engine) noteDrift(it *Item, rec *state.Package) {
	if rec.Manifest == "" {
		return
	}
	m, err := state.LoadManifest(state.Path(e.cfg.Paths.State, rec.Manifest))
	if err != nil {
		it.notef("the manifest of the installed version cannot be read: %v", err)
		return
	}
	// The new version's files are not known before it is downloaded; the installed ones are the
	// best guess (apply checks again once the archive is unpacked).
	installed := make([]string, len(m.Files))
	for i, f := range m.Files {
		installed[i] = f.Path
	}
	e.noteLongPath(it, installed, m.Dirs)
	var changed []string
	for _, f := range m.Files {
		fi, err := os.Lstat(filepath.Join(it.Dir, filepath.FromSlash(f.Path)))
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != f.Size {
			changed = append(changed, f.Path)
		}
	}
	if len(changed) > 0 {
		shown := changed[:min(len(changed), 5)]
		more := ""
		if len(changed) > len(shown) {
			more = fmt.Sprintf(" and %d more", len(changed)-len(shown))
		}
		it.notef("%d installed file(s) changed outside GremLoader (%s%s); the update replaces them and keeps a backup",
			len(changed), strings.Join(shown, ", "), more)
	}
}

// record returns the package's state record, creating an empty one (version "" = not installed).
func (e *Engine) record(name string) *state.Package {
	if e.st.Packages == nil {
		e.st.Packages = map[string]*state.Package{}
	}
	r := e.st.Packages[name]
	if r == nil {
		r = &state.Package{}
		e.st.Packages[name] = r
	}
	return r
}

func ptr[T any](v T) *T { return &v }

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
