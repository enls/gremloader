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
	"github.com/enls/gremloader/internal/state"
	"github.com/enls/gremloader/internal/ver"
)

// AdoptOptions say how an existing folder's version is established.
type AdoptOptions struct {
	// Version is trusted as given (adopt --version).
	Version string
	// Verify downloads that version's release and records the folder only if every file in it
	// matches (adopt --verify). That also yields a manifest, so a later remove knows the files.
	Verify bool
	// Self adopts the running GremLoader (self-update, ADR 0015) at Version: a shared folder needs no
	// file list (the package is never removed), and a record that no longer describes the running
	// binary (another version, another folder) is replaced.
	Self bool
}

// Adopt records an existing install folder as an installed package without touching a file
// (plan E.3.3, P3-5). The version comes from --version or from the package's version_from_exe
// (a Windows executable's product version).
func (e *Engine) Adopt(ctx context.Context, name string, o AdoptOptions) *Item {
	it := &Item{Package: name, Result: ResultFailed}
	pkg := e.cfg.Packages.Packages[name]
	if pkg == nil {
		it.fail(fmt.Errorf("package %q is not in %s", name, e.cfg.Packages.File))
		return it
	}
	if e.installed(name) != nil && !o.Self {
		it.fail(fmt.Errorf("%s is already installed", name))
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
	if ents, err := os.ReadDir(dir); err != nil || len(ents) == 0 {
		it.fail(fmt.Errorf("there is nothing to adopt in %s", dir))
		return it
	}
	// In a shared folder only the release itself says which files are this package's; without
	// that list the package could never be removed (a shared folder is not deleted whole).
	shared := pkg.SharedDir(e.cfg.Platform.OS)
	if shared && !o.Verify && !o.Self {
		it.fail(fmt.Errorf("%s installs into %s, a shared folder (shared_dir): `gremloader adopt %s --verify` "+
			"compares it with the release and records which files are its own", name, dir, name))
		return it
	}

	v, err := e.adoptVersion(pkg.Name, o, dir)
	if err != nil {
		it.fail(err)
		return it
	}
	it.To = v
	now := e.now()
	rec := &state.Package{Version: v, Dir: dir, InstalledAt: &now, Adopted: true, SharedDir: shared}

	if o.Verify {
		fit, st, err := e.fetchVersion(ctx, name, v)
		defer func() { _ = os.RemoveAll(e.cfg.Paths.Staging(name)) }()
		if err != nil {
			it.fail(err)
			return it
		}
		missing, modified := compareTree(st, dir)
		if len(missing)+len(modified) > 0 {
			it.fail(fmt.Errorf("%s does not hold release %s as published: %s; "+
				"`adopt --version %s` trusts what is there instead", dir, v, describeDiff(missing, modified), v))
			return it
		}
		m := manifestOf(st.ar, dir, nil, shared)
		rec.Tag, rec.Asset, rec.AssetSHA256, rec.VerifiedBy = fit.Release.Tag, fit.Asset.Name, st.got.SHA256, st.levels
		rec.Attested = slices.Contains(st.levels, levelAttestation)
		rec.Manifest = state.ManifestRel(name, v)
		p := state.Path(e.cfg.Paths.State, rec.Manifest)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			it.fail(err)
			return it
		}
		if err := state.SaveManifest(p, m); err != nil {
			it.fail(err)
			return it
		}
	}
	replaced := e.installed(name) != nil
	e.setRecord(name, rec)
	if err := e.saveState(); err != nil {
		it.fail(err)
		return it
	}
	if replaced {
		// The replaced record's file lists and rollback copy describe a binary that is gone.
		e.pruneManifests(name)
		_ = os.RemoveAll(e.cfg.Paths.Previous(name))
	}
	it.Result, it.Err = ResultAdopted, nil
	if !o.Verify && !o.Self {
		it.notef("adopted without a file list: an update keeps files the new version does not have, and remove needs --delete-dir")
	}
	e.emit(events.Event{Level: events.Info, Event: "adopt", Package: name, Version: v, Msg: dir})
	return it
}

func (e *Engine) adoptVersion(name string, o AdoptOptions, dir string) (string, error) {
	s, from := o.Version, "--version"
	if s == "" {
		rel, ok, err := e.cfg.Packages.Packages[name].VersionFromExe(e.cfg.Platform.OS)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("give the installed version with --version (the package sets no version_from_exe for %s)", e.cfg.Platform.OS)
		}
		exe := filepath.Join(dir, filepath.FromSlash(rel))
		if s, err = e.cfg.ExeVersion(exe); err != nil {
			return "", fmt.Errorf("reading the version of %s: %w; give it with --version", exe, err)
		}
		from = exe
	}
	v, err := ver.ParseLoose(s)
	if err != nil {
		return "", fmt.Errorf("version %q (from %s): %w", s, from, err)
	}
	return v.String(), nil
}

// fetchVersion downloads, verifies and unpacks one exact version of a package into staging,
// without installing it.
func (e *Engine) fetchVersion(ctx context.Context, name, version string) (*Item, *staged, error) {
	it := e.planOne(ctx, name, Request{Packages: []string{name}, Want: version, Force: true, InstallMissing: true, forFetch: true})
	if it.Err != nil {
		return it, nil, it.Err
	}
	if it.Action == ActNone {
		return it, nil, fmt.Errorf("release %s of %s cannot be fetched: %s", version, name, it.Reason)
	}
	work := filepath.Join(e.cfg.Paths.Staging(name), "fetch")
	if err := os.RemoveAll(e.cfg.Paths.Staging(name)); err != nil {
		return it, nil, err
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return it, nil, err
	}
	st, err := e.stage(ctx, it, e.cfg.Packages.Packages[name], work, ApplyOptions{fetchOnly: true})
	return it, st, err
}

// compareTree compares the release's files with what dir holds. Files dir has beyond the release
// are not its business.
func compareTree(st *staged, dir string) (missing, modified []string) {
	for _, f := range st.ar.Files {
		sum, _, err := hashFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			missing = append(missing, f.Path)
		case err != nil || sum != f.SHA256:
			modified = append(modified, f.Path)
		}
	}
	slices.Sort(missing)
	slices.Sort(modified)
	return missing, modified
}

func describeDiff(missing, modified []string) string {
	var parts []string
	list := func(n string, ps []string) {
		if len(ps) == 0 {
			return
		}
		shown := ps[:min(len(ps), 5)]
		s := fmt.Sprintf("%d %s (%s", len(ps), n, strings.Join(shown, ", "))
		if len(ps) > len(shown) {
			s += fmt.Sprintf(" and %d more", len(ps)-len(shown))
		}
		parts = append(parts, s+")")
	}
	list("missing", missing)
	list("modified", modified)
	return strings.Join(parts, ", ")
}

// Check is the result of verifying one installed package.
type Check struct {
	Package    string
	Version    string
	Dir        string
	Files      int      // files compared
	Missing    []string // in the manifest (or the release), not on disk
	Modified   []string // on disk with other bytes
	NoManifest bool     // adopted without a file list: nothing to compare locally
	Quarantine string   // a failed download kept for inspection, if any
	Err        error
}

// OK reports whether everything compared was as it should be.
func (c *Check) OK() bool {
	return c.Err == nil && len(c.Missing) == 0 && len(c.Modified) == 0
}

// Verify re-hashes installed files (plan E.3.3): against the manifest, without the network, or
// with remote against the release itself, downloaded again and verified like an install. Packages
// default to every installed one.
func (e *Engine) Verify(ctx context.Context, names []string, remote bool) []*Check {
	if len(names) == 0 {
		for n, rec := range e.st.Packages {
			if rec.Version != "" {
				names = append(names, n)
			}
		}
		slices.Sort(names)
	}
	var out []*Check
	for _, n := range names {
		out = append(out, e.verifyOne(ctx, n, remote))
	}
	return out
}

func (e *Engine) verifyOne(ctx context.Context, name string, remote bool) *Check {
	c := &Check{Package: name}
	if q := e.quarantineDir(name); fileExists(filepath.Join(q, "info.json")) {
		c.Quarantine = q
	}
	rec := e.installed(name)
	if rec == nil {
		c.Err = fmt.Errorf("%s is not installed", name)
		return c
	}
	c.Version, c.Dir = rec.Version, rec.Dir
	if remote {
		if e.cfg.Packages.Packages[name] == nil {
			c.Err = fmt.Errorf("%s is not in the package file, so its release is not known", name)
			return c
		}
		_, st, err := e.fetchVersion(ctx, name, rec.Version)
		defer func() { _ = os.RemoveAll(e.cfg.Paths.Staging(name)) }()
		if err != nil {
			c.Err = err
			return c
		}
		c.Files = len(st.ar.Files)
		c.Missing, c.Modified = compareTree(st, rec.Dir)
		return c
	}
	if rec.Manifest == "" {
		c.NoManifest = true
		return c
	}
	m, err := state.LoadManifest(state.Path(e.cfg.Paths.State, rec.Manifest))
	if err != nil {
		c.Err = err
		return c
	}
	for _, f := range m.Files {
		c.Files++
		sum, size, err := hashFile(filepath.Join(rec.Dir, filepath.FromSlash(f.Path)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			c.Missing = append(c.Missing, f.Path)
		case err != nil || size != f.Size || sum != f.SHA256:
			c.Modified = append(c.Modified, f.Path)
		}
	}
	return c
}
