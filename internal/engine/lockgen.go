package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/enls/gremloader/internal/checksum"
	"github.com/enls/gremloader/internal/lock"
	"github.com/enls/gremloader/internal/pkgfile"
	"github.com/enls/gremloader/internal/platform"
)

// ResultLocked: the package's lock entry was made (`gremloader lock`).
const ResultLocked Result = "locked"

// LockOptions say what to lock.
type LockOptions struct {
	Packages []string // empty: every package in the file
	// Want locks an exact version of a package (apply --update-lock locks what it just installed).
	Want map[string]string
}

// Lock resolves packages and returns the lock entry of each for every platform it is used on
// (plan E.2.3): the release, and per platform the asset, its download URL, size and SHA-256.
//
// The lock is shared between machines, so nothing machine-local plays a part: the version is the
// one a fresh install would pick (pin, channel and cool-down apply; the installed version, holds
// and rejected versions do not). Releases are always read through the GitHub API, the only source
// of every platform's asset list and sizes (one request per package). Hashes come from the
// package's checksum file (downloaded once per release, or one sidecar per platform) and GitHub's
// digest; where both exist they must agree. Nothing is installed and state.json is not changed.
func (e *Engine) Lock(ctx context.Context, o LockOptions) (map[string]*lock.Package, []*Item, error) {
	names := o.Packages
	if len(names) == 0 {
		names = e.cfg.Packages.Names()
	}
	for _, n := range names {
		if e.cfg.Packages.Packages[n] == nil {
			return nil, nil, fmt.Errorf("package %q is not in %s", n, e.cfg.Packages.File)
		}
	}
	out := map[string]*lock.Package{}
	var items []*Item
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		it, lp := e.lockOne(ctx, n, o.Want[n])
		items = append(items, it)
		if lp != nil {
			out[n] = lp
		}
	}
	return out, items, nil
}

func (e *Engine) lockOne(ctx context.Context, name, want string) (*Item, *lock.Package) {
	pkg := e.cfg.Packages.Packages[name]
	it := &Item{Package: name}
	if !pkg.Enabled {
		it.Result, it.Reason = ResultSkipped, "disabled"
		return it, nil
	}
	q := e.query(name, pkg)
	q.NeedAPIData = false
	listing, err := e.cfg.Source.List(ctx, q)
	if err != nil {
		it.fail(err)
		return it, nil
	}
	it.Via = listing.Via
	it.Notes = append(it.Notes, listing.Warnings...)
	d := resolve(resolveIn{
		Releases: listing.Releases, Channel: pkg.Channel, CooldownDays: pkg.CooldownDays,
		CooldownUnknown: pkg.CooldownUnknown, Now: e.now(), Pin: pkg.Pin, Want: want, InstallMissing: true,
	})
	if d.Err != nil {
		it.fail(d.Err)
		return it, nil
	}
	if d.Release == nil {
		it.Result, it.Reason = d.Result, d.Reason
		if it.Result == "" || it.Result == ResultUnchanged {
			it.Result = ResultSkipped
		}
		return it, nil
	}
	it.Release, it.To = *d.Release, d.Release.Version.String()

	stage := e.cfg.Paths.Staging(name)
	if err := os.RemoveAll(stage); err != nil {
		it.fail(err)
		return it, nil
	}
	defer func() { _ = os.RemoveAll(stage) }()

	lp := &lock.Package{Repo: pkg.Repo, Tag: it.Release.Tag, Version: it.To, Platforms: map[string]*lock.Asset{}}
	if !it.Release.Published.IsZero() {
		lp.PublishedAt = ptr(it.Release.Published.UTC())
	}
	platforms := pkg.Platforms()
	if len(platforms) == 0 {
		platforms = []platform.Platform{e.cfg.Platform}
	}
	sums := map[string]*checksum.File{} // by checksum file name: one download per file
	for _, pl := range platforms {
		a, err := e.lockAsset(ctx, it, pkg, pl, filepath.Join(stage, "lock"), sums)
		if err != nil {
			it.fail(fmt.Errorf("%s: %w", pl, err))
			return it, nil
		}
		lp.Platforms[pl.String()] = a
	}
	it.Result, it.Reason = ResultLocked, fmt.Sprintf("%s for %s", it.To, strings.Join(slices.Sorted(maps.Keys(lp.Platforms)), ", "))
	return it, lp
}

// lockAsset makes one platform's entry.
func (e *Engine) lockAsset(ctx context.Context, it *Item, pkg *pkgfile.Package, pl platform.Platform, work string,
	sums map[string]*checksum.File) (*lock.Asset, error) {
	v := pkgfile.Values{Version: it.To, Tag: it.Release.Tag, Platform: pl}
	pattern, err := pkg.AssetPattern(v)
	if err != nil {
		return nil, err
	}
	pi := &Item{Package: it.Package, To: it.To, Release: it.Release}
	if pi.Asset, err = e.cfg.Source.SelectAsset(it.Release, pattern); err != nil {
		return nil, err
	}
	var file *checksum.File
	csName, ok, err := pkg.ChecksumsName(v, pi.Asset.Name)
	if err != nil {
		return nil, err
	}
	if ok {
		c, err := e.cfg.Source.SelectAsset(it.Release, csName)
		if err != nil {
			return nil, fmt.Errorf("checksum file: %w", err)
		}
		pi.Checksums, pi.Sidecar = &c, pkg.Checksums.Uses("asset")
		if file = sums[c.Name]; file == nil {
			if err := os.MkdirAll(work, 0o700); err != nil {
				return nil, err
			}
			p := filepath.Join(work, "checksums-"+strconv.Itoa(len(sums)))
			if _, err := e.fetch(ctx, it, c.URL, p, maxChecksumFile); err != nil {
				return nil, fmt.Errorf("checksum file %s: %w", c.Name, err)
			}
			if file, err = parseChecksums(p); err != nil {
				return nil, &VerifyError{Package: it.Package, Msg: fmt.Sprintf("checksum file %s: %v", c.Name, err)}
			}
			sums[c.Name] = file
		}
	}
	if err := e.checkVerifiable(pi, pkg); err != nil {
		return nil, err
	}
	exp, err := expectations(pi, pkg, file)
	if err != nil {
		return nil, err
	}
	it.Notes = append(it.Notes, pi.Notes...)

	var hash, from string
	for _, t := range exp {
		switch {
		case hash == "":
			hash, from = t.hash, t.source
		case !checksum.Equal(hash, t.hash):
			return nil, &VerifyError{Package: it.Package, Msg: fmt.Sprintf("%s and %s disagree about the SHA-256 of %s (%s, %s); "+
				"nothing was locked", from, t.source, pi.Asset.Name, hash, t.hash)}
		}
	}
	if hash == "" {
		return nil, &VerifyError{Package: it.Package, Msg: fmt.Sprintf("nothing states the SHA-256 of %s (no checksum file entry, "+
			"no GitHub digest), so it cannot be locked", pi.Asset.Name)}
	}
	if pi.Asset.Size <= 0 {
		return nil, errors.New("GitHub did not report the size of " + pi.Asset.Name)
	}
	return &lock.Asset{Asset: pi.Asset.Name, URL: pi.Asset.URL, SHA256: strings.ToLower(hash), Size: pi.Asset.Size}, nil
}
