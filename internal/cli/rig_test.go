package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/clock"
	"github.com/enls/gremloader/internal/download"
	"github.com/enls/gremloader/internal/platform"
	"github.com/enls/gremloader/internal/proc"
	"github.com/enls/gremloader/internal/state"
	"github.com/enls/gremloader/internal/testutil/archives"
	"github.com/enls/gremloader/internal/testutil/fakerelease"
)

// appFile is a package file with one package, "app", installed into <root>/App.
const appFile = `schema_version = 1

[package.app]
repo      = "o/app"
asset     = "app-{version}.zip"
checksums = "SHA256SUMS.txt"
dir       = "{root}/App"
`

// rig runs the whole command line in-process against a fake GitHub, with GREMLOADER_HOME and the
// install root in temporary folders and the package file in the config folder.
type rig struct {
	t        *testing.T
	srv      *fakerelease.Server
	clock    *clock.Fake
	home     string
	root     string
	cwd      string
	env      map[string]string
	running  []proc.Process
	stdin    string            // what the user types
	tty      bool              // stdin is a terminal
	platform platform.Platform // zero: this machine
}

func newRig(t *testing.T, packageFile string) *rig {
	t.Helper()
	base := t.TempDir()
	r := &rig{
		t:     t,
		clock: clock.NewFake(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)),
		home:  filepath.Join(base, "home"),
		root:  filepath.Join(base, "root"),
		cwd:   filepath.Join(base, "cwd"),
	}
	r.srv = fakerelease.New(t, fakerelease.WithClock(r.clock))
	r.env = map[string]string{"GREMLOADER_HOME": r.home}
	for _, d := range []string{r.root, r.cwd, filepath.Join(r.home, "config")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.writeFile(packageFile)
	return r
}

func (r *rig) file() string { return filepath.Join(r.home, "config", "gremloader.toml") }

func (r *rig) stateDir() string { return filepath.Join(r.home, "state") }

func (r *rig) dir() string { return filepath.Join(r.root, "App") }

func (r *rig) writeFile(content string) {
	r.t.Helper()
	if err := os.WriteFile(r.file(), []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) deps() deps {
	api, _ := url.Parse(r.srv.APIBase())
	return deps{
		getenv:    func(k string) string { return r.env[k] },
		cwd:       func() (string, error) { return r.cwd, nil },
		home:      filepath.Join(r.root, "userhome"),
		clock:     r.clock,
		stdinTTY:  func() bool { return r.tty },
		apiBase:   r.srv.APIBase(),
		webBase:   r.srv.WebBase(),
		policy:    &download.Policy{Hosts: []string{"127.0.0.1"}, TokenHosts: []string{api.Host}, AllowHTTP: true},
		sleep:     func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		running:   func(string) ([]proc.Process, error) { return r.running, nil },
		freeSpace: func(string) (uint64, error) { return 1 << 50, nil },
		platform:  r.platform,
	}
}

// run runs one command line with --root set, empty stdin.
func (r *rig) run(args ...string) (code int, stdout, stderr string) {
	r.t.Helper()
	var out, errb bytes.Buffer
	code = runWith(append(args, "--root", r.root), IO{Stdin: strings.NewReader(r.stdin), Stdout: &out, Stderr: &errb}, r.deps())
	return code, out.String(), errb.String()
}

// runJSON runs with --json and decodes the output.
func (r *rig) runJSON(args ...string) (int, jsonResult) {
	r.t.Helper()
	code, out, errOut := r.run(append(args, "--json")...)
	var res jsonResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		r.t.Fatalf("%q: output is not one JSON object: %v\nstdout: %s\nstderr: %s", args, err, out, errOut)
	}
	if res.ExitCode != code || res.SchemaVersion != jsonSchema {
		r.t.Errorf("%q: JSON says exit %d schema %d, the process exited %d", args, res.ExitCode, res.SchemaVersion, code)
	}
	return code, res
}

type jsonResult struct {
	SchemaVersion int               `json:"schema_version"`
	Command       string            `json:"command"`
	ExitCode      int               `json:"exit_code"`
	Items         []json.RawMessage `json:"items"`
	Warnings      []string          `json:"warnings"`
	Error         string            `json:"error"`
}

func (j jsonResult) item(t *testing.T, i int, v any) {
	t.Helper()
	if i >= len(j.Items) {
		t.Fatalf("no item %d in %d", i, len(j.Items))
	}
	if err := json.Unmarshal(j.Items[i], v); err != nil {
		t.Fatal(err)
	}
}

// publish adds release v of o/app: a zip of files under one top-level folder and a SHA256SUMS.txt.
func (r *rig) publish(v string, daysAgo int, files ...archives.Entry) []byte {
	r.t.Helper()
	name := "app-" + v + ".zip"
	prefixed := make([]archives.Entry, len(files))
	for i, f := range files {
		f.Name = "app-" + v + "/" + f.Name
		prefixed[i] = f
	}
	zip := archives.Zip(prefixed...)
	sum := sha256.Sum256(zip)
	r.srv.AddRelease("o", "app", fakerelease.Release{
		Tag:         "v" + v,
		PublishedAt: r.clock.Now().Add(-time.Duration(daysAgo) * 24 * time.Hour),
		Assets: []fakerelease.Asset{
			{Name: name, Data: zip},
			{Name: "SHA256SUMS.txt", Data: []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")},
		},
	})
	return zip
}

// installed records app at version v as installed (adopted: no file list) in <root>/App.
func (r *rig) installed(t *testing.T, v string) {
	t.Helper()
	if err := os.MkdirAll(r.dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir(), "app.exe"), []byte("installed "+v), 0o600); err != nil {
		t.Fatal(err)
	}
	r.installedAs(t, "app", v)
}

// installedAs writes a state record for name at version v (test setup, as a previous run would).
func (r *rig) installedAs(t *testing.T, name, v string) {
	t.Helper()
	p := filepath.Join(r.stateDir(), "state.json")
	st, err := state.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Packages == nil {
		st.Packages = map[string]*state.Package{}
	}
	at := r.clock.Now().Add(-48 * time.Hour)
	st.Packages[name] = &state.Package{Version: v, Tag: "v" + v, Dir: filepath.Join(r.root, name), Adopted: true, InstalledAt: &at}
	if name == "app" {
		st.Packages[name].Dir = r.dir()
	}
	if err := os.MkdirAll(r.stateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(p, st); err != nil {
		t.Fatal(err)
	}
}

// publishBadSums adds v whose checksum file names a hash that is not the asset's. It returns the
// asset's real hash.
func (r *rig) publishBadSums(v string) string {
	r.t.Helper()
	name := "app-" + v + ".zip"
	zip := archives.Zip(archives.X("app-"+v+"/app.exe", "app "+v))
	r.srv.AddRelease("o", "app", fakerelease.Release{
		Tag: "v" + v, PublishedAt: r.clock.Now().Add(-24 * time.Hour),
		Assets: []fakerelease.Asset{
			{Name: name, Data: zip},
			{Name: "SHA256SUMS.txt", Data: []byte(strings.Repeat("ab", 32) + "  " + name + "\n")},
		},
	})
	sum := sha256.Sum256(zip)
	return hex.EncodeToString(sum[:])
}

func (r *rig) appExe() string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir(), "app.exe"))
	if err != nil {
		return ""
	}
	return string(b)
}

func (r *rig) lockFile() string { return filepath.Join(r.home, "config", "gremloader.lock") }
