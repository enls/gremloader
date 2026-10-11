package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"

	"github.com/enls/gremloader/internal/attest"
	"github.com/enls/gremloader/internal/attest/sigstore"
	"github.com/enls/gremloader/internal/clock"
	"github.com/enls/gremloader/internal/download"
	"github.com/enls/gremloader/internal/events"
	"github.com/enls/gremloader/internal/paths"
	"github.com/enls/gremloader/internal/pkgfile"
	"github.com/enls/gremloader/internal/proc"
	"github.com/enls/gremloader/internal/source/github"
	"github.com/enls/gremloader/internal/state"
	"github.com/enls/gremloader/internal/testutil/archives"
	"github.com/enls/gremloader/internal/testutil/fakerelease"
	"github.com/enls/gremloader/internal/testutil/fixtures"
)

// appFile is a package file with one package, "app", installed into <root>/App from zip assets
// verified by a SHA256SUMS.txt.
const appFile = `schema_version = 1

[package.app]
repo      = "o/app"
asset     = "app-{version}.zip"
checksums = "SHA256SUMS.txt"
dir       = "{root}/App"
`

// rig is one isolated world: a fake GitHub, a state directory and an install root in temp dirs.
type rig struct {
	t          *testing.T
	srv        *fakerelease.Server
	clock      *clock.Fake
	root       string // {root}
	paths      paths.Paths
	file       string // gremloader.toml
	running    []proc.Process
	hooks      *fakeHooks
	token      string
	free       func(string) (uint64, error) // nil: plenty
	wrap       func(Fetcher) Fetcher        // nil: the real download client
	exeVersion func(string) (string, error) // nil: none
	attest     Attester                     // nil: a real sigstore.Checker over the fake (none published unless added)
}

func newRig(t *testing.T, packageFile string) *rig {
	t.Helper()
	base := t.TempDir()
	r := &rig{
		t:     t,
		clock: clock.NewFake(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)),
		root:  filepath.Join(base, "root"),
		paths: paths.Paths{
			Config: filepath.Join(base, "config"),
			State:  filepath.Join(base, "state"),
			Cache:  filepath.Join(base, "cache"),
		},
		file:  filepath.Join(base, "config", "gremloader.toml"),
		hooks: &fakeHooks{},
	}
	r.srv = fakerelease.New(t, fakerelease.WithClock(r.clock))
	for _, d := range []string{r.root, r.paths.Config, r.paths.State} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.writeFile(packageFile)
	return r
}

func (r *rig) writeFile(content string) {
	r.t.Helper()
	if err := os.WriteFile(r.file, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// engine builds an engine over the rig, as a fresh process would: everything is read from disk.
func (r *rig) engine() *Engine {
	r.t.Helper()
	return r.engineAt(r.srv.APIBase(), r.srv.WebBase())
}

// engineAt builds the engine against a fake GitHub at the given bases (a child process uses its
// parent's).
func (r *rig) engineAt(apiBase, webBase string) *Engine {
	r.t.Helper()
	cfg, err := pkgfile.Load(r.file, pkgfile.Options{Vars: map[string]string{"root": r.root}})
	if err != nil {
		r.t.Fatalf("package file: %v", err)
	}
	api, _ := url.Parse(apiBase)
	client := download.New(download.Config{
		Policy: download.Policy{Hosts: []string{"127.0.0.1"}, TokenHosts: []string{api.Host}, AllowHTTP: true},
		Token:  r.token,
		Sleep:  func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		Jitter: func(time.Duration) time.Duration { return 0 },
	})
	free := r.free
	if free == nil {
		free = func(string) (uint64, error) { return 1 << 50, nil }
	}
	var fetcher Fetcher = client
	if r.wrap != nil {
		fetcher = r.wrap(client)
	}
	log, err := events.Open(r.paths.EventsLog(), events.WithClock(r.clock))
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = log.Close() })
	src := github.New(client, github.Options{APIBase: apiBase, WebBase: webBase})
	att := r.attest
	if att == nil {
		att = &sigstore.Checker{
			List: func(ctx context.Context, repo, sum string) ([]attest.Raw, error) {
				bs, err := src.Attestations(ctx, repo, sum, client, sigstore.MaxBundle)
				var raws []attest.Raw
				for _, b := range bs {
					raws = append(raws, attest.Raw{Data: b.Data, Snappy: b.Snappy})
				}
				return raws, err
			},
			Root: func(context.Context) (root.TrustedMaterial, error) {
				return root.NewTrustedRootFromJSON(fixtures.Read(r.t, "sigstore/trusted_root.json"))
			},
		}
	}
	e, err := New(Config{
		Paths:     r.paths,
		Packages:  cfg,
		Source:    src,
		Attest:    att,
		Fetcher:   fetcher,
		FreeSpace: free,
		ExeVersion: func(p string) (string, error) {
			if r.exeVersion == nil {
				return "", errors.New("no version resource")
			}
			return r.exeVersion(p)
		},
		Events:  log,
		Hooks:   r.hooks,
		Clock:   r.clock,
		Home:    filepath.Join(r.root, "home"),
		Running: func(dir string) ([]proc.Process, error) { return r.running, nil },
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return e
}

// publish adds release v of o/<repo> with a zip of files under one top-level folder (as real
// release archives are packed) and a SHA256SUMS.txt naming it. It returns the zip.
func (r *rig) publish(repo, v string, daysAgo int, files ...archives.Entry) []byte {
	r.t.Helper()
	return r.publishAs(repo, "v"+v, "app-"+v+".zip", daysAgo, files...)
}

func (r *rig) publishAs(repo, tag, assetName string, daysAgo int, files ...archives.Entry) []byte {
	r.t.Helper()
	prefixed := make([]archives.Entry, len(files))
	for i, f := range files {
		f.Name = strings.TrimSuffix(assetName, ".zip") + "/" + f.Name
		prefixed[i] = f
	}
	zip := archives.Zip(prefixed...)
	r.srv.AddRelease("o", repo, fakerelease.Release{
		Tag:         tag,
		PublishedAt: r.clock.Now().Add(-time.Duration(daysAgo) * 24 * time.Hour),
		Assets: []fakerelease.Asset{
			{Name: assetName, Data: zip},
			{Name: "SHA256SUMS.txt", Data: []byte(sha(zip) + "  " + assetName + "\n")},
		},
	})
	return zip
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// installed writes a state record saying pkg version v is installed in dir, with no manifest
// (as if adopted).
func (r *rig) installed(pkg, v, dir string) {
	r.t.Helper()
	st, err := state.Load(r.paths.StateFile())
	if err != nil {
		r.t.Fatal(err)
	}
	if st.Packages == nil {
		st.Packages = map[string]*state.Package{}
	}
	at := r.clock.Now().Add(-48 * time.Hour)
	st.Packages[pkg] = &state.Package{Version: v, Tag: "v" + v, Dir: dir, Adopted: true, InstalledAt: &at}
	if err := state.Save(r.paths.StateFile(), st); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) state() *state.State {
	r.t.Helper()
	st, err := state.Load(r.paths.StateFile())
	if err != nil {
		r.t.Fatal(err)
	}
	return st
}

func (r *rig) plan(e *Engine, rq Request) *Plan {
	r.t.Helper()
	p, err := e.Plan(context.Background(), rq)
	if err != nil {
		r.t.Fatalf("Plan: %v", err)
	}
	return p
}

func item(t *testing.T, p *Plan, name string) *Item {
	t.Helper()
	for _, it := range p.Items {
		if it.Package == name {
			return it
		}
	}
	t.Fatalf("no item for %s in the plan", name)
	return nil
}

// fakeHooks records the hooks the engine asks to run; fail makes the named event fail.
type fakeHooks struct {
	ran  []Hook
	fail map[string]error
}

func (f *fakeHooks) Run(_ context.Context, h Hook) error {
	f.ran = append(f.ran, h)
	return f.fail[h.Event]
}

// events reads events.jsonl.
func (r *rig) events() []events.Event {
	r.t.Helper()
	f, err := os.Open(r.paths.EventsLog())
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	evs, _, err := events.Scan(f)
	if err != nil {
		r.t.Fatal(err)
	}
	return evs
}

// tree lists every file under dir with its content, "/"-separated, for exact comparisons.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." {
				out[rel+"/"] = ""
			}
			return nil
		}
		b, err := os.ReadFile(p) //nolint:gosec // test
		out[rel] = string(b)
		return err
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return out
}

func show(m map[string]string) string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s = %q\n", k, m[k])
	}
	return b.String()
}

// fakeRelease is a release with one zip asset and a checksum file with the given content ("" for
// none).
func fakeRelease(tag, assetName string, zip []byte, sums string, published time.Time) fakerelease.Release {
	rel := fakerelease.Release{Tag: tag, PublishedAt: published, Assets: []fakerelease.Asset{{Name: assetName, Data: zip}}}
	if sums == "" {
		sums = sha(zip) + "  " + assetName + "\n"
	}
	rel.Assets = append(rel.Assets, fakerelease.Asset{Name: "SHA256SUMS.txt", Data: []byte(sums)})
	return rel
}
