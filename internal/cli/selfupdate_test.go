package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/attest"
	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/platform"
	"github.com/enls/gremloader/internal/runlock"
	"github.com/enls/gremloader/internal/state"
	"github.com/enls/gremloader/internal/testutil/archives"
	"github.com/enls/gremloader/internal/testutil/fakerelease"
)

// fakeAttester answers every attestation check with res and err, and records the policies asked.
type fakeAttester struct {
	res   attest.Result
	err   error
	asked []attest.Policy
}

func (f *fakeAttester) Check(_ context.Context, _, _ string, p attest.Policy) (attest.Result, error) {
	f.asked = append(f.asked, p)
	return f.res, f.err
}

const selfSigner = ".github/workflows/release.yml@refs/tags/v0.0.2"

var (
	winAmd64   = platform.Platform{OS: "windows", Arch: "amd64"}
	linuxAmd64 = platform.Platform{OS: "linux", Arch: "amd64"}
)

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// selfRig: GremLoader cur running from <root>/bin on pl, with the README of its release and the
// user's own file beside it (a shared folder), and a passing attestation check.
func selfRig(t *testing.T, pl platform.Platform, cur string) *rig {
	t.Helper()
	r := newRig(t, appFile)
	r.platform = pl
	name := selfName
	if pl.OS == "windows" {
		name += ".exe"
	}
	r.selfExe, r.selfVersion = filepath.Join(r.root, "bin", name), cur
	noErr(t, os.MkdirAll(filepath.Dir(r.selfExe), 0o755))
	for p, c := range map[string]string{name: "gremloader " + cur, "README.md": "readme " + cur, "notes.txt": "mine"} {
		noErr(t, os.WriteFile(filepath.Join(filepath.Dir(r.selfExe), p), []byte(c), 0o600))
	}
	r.attest = &fakeAttester{res: attest.Result{Outcome: attest.Passed, Workflow: selfSigner}}
	return r
}

// publishSelf adds release v of enls/gremloader for the rig's platform, laid out as the release
// script lays it out, with SHA256SUMS.txt.
func (r *rig) publishSelf(v string, prerelease bool, daysAgo int) {
	r.t.Helper()
	pl := r.platform
	top := "gremloader-" + v + "-" + pl.OS + "-" + pl.Arch
	bin, name, pack := "gremloader", top+".tar.gz", archives.TarGz
	if pl.OS == "windows" {
		bin, name, pack = "gremloader.exe", top+".zip", archives.Zip
	}
	data := pack(archives.X(top+"/"+bin, "gremloader "+v), archives.F(top+"/README.md", "readme "+v),
		archives.F(top+"/LICENSE-MIT", "mit"), archives.F(top+"/LICENSE-APACHE", "apache"))
	r.srv.AddRelease("enls", "gremloader", fakerelease.Release{
		Tag: "v" + v, Prerelease: prerelease, PublishedAt: r.clock.Now().Add(-time.Duration(daysAgo) * 24 * time.Hour),
		Assets: []fakerelease.Asset{
			{Name: name, Data: data},
			{Name: "SHA256SUMS.txt", Data: []byte(shaOf(data) + "  " + name + "\n")},
		},
	})
}

func (r *rig) binary() string {
	r.t.Helper()
	b, err := os.ReadFile(r.selfExe)
	if err != nil {
		return ""
	}
	return string(b)
}

func (r *rig) selfItem(args ...string) (int, jsonItem) {
	r.t.Helper()
	code, res := r.runJSON(append([]string{"self-update"}, args...)...)
	var it jsonItem
	if len(res.Items) > 0 {
		res.item(r.t, 0, &it)
	}
	return code, it
}

func TestSelfUpdate(t *testing.T) {
	for _, pl := range []platform.Platform{winAmd64, linuxAmd64} {
		t.Run(pl.OS, func(t *testing.T) {
			r := selfRig(t, pl, "0.0.1")
			r.publishSelf("0.0.1", false, 30)
			r.publishSelf("0.0.2", false, 10)

			code, it := r.selfItem("--yes")
			if code != exitcode.OK || it.Result != "updated" || it.From != "0.0.1" || it.To != "0.0.2" || it.Signer != selfSigner {
				t.Fatalf("exit %d: %+v", code, it)
			}
			bin := filepath.Dir(r.selfExe)
			if r.binary() != "gremloader 0.0.2" {
				t.Errorf("the binary holds %q", r.binary())
			}
			for p, want := range map[string]string{"README.md": "readme 0.0.2", "LICENSE-MIT": "mit", "LICENSE-APACHE": "apache", "notes.txt": "mine"} {
				if got, _ := os.ReadFile(filepath.Join(bin, p)); string(got) != want {
					t.Errorf("%s holds %q", p, got)
				}
			}
			want := attest.Policy{Repo: "enls/gremloader", SignerWorkflow: ".github/workflows/release.yml"}
			if a := r.attest.(*fakeAttester); len(a.asked) != 1 || a.asked[0] != want {
				t.Errorf("attestation policies asked: %+v", a.asked)
			}

			// The new binary runs: nothing newer.
			r.selfVersion = "0.0.2"
			if code, it := r.selfItem("--yes"); code != exitcode.OK || it.Action != "" || it.From != "0.0.2" {
				t.Errorf("second run: exit %d %+v", code, it)
			}
			// Its state is its own: the package list does not show it.
			self, err := state.Load(filepath.Join(r.stateDir(), "self", "state.json"))
			if err != nil || self.Packages[selfName] == nil || self.Packages[selfName].Version != "0.0.2" {
				t.Errorf("self state: %+v %v", self, err)
			}
			main, err := state.Load(filepath.Join(r.stateDir(), "state.json"))
			if err != nil || main.Packages[selfName] != nil {
				t.Errorf("the main state knows gremloader: %v", err)
			}
			if _, out, _ := r.run("list"); strings.Contains(out, selfName) {
				t.Errorf("list shows it:\n%s", out)
			}
			if _, out, _ := r.run("log", "--pkg", selfName); !strings.Contains(out, "0.0.2") {
				t.Errorf("the event log has no self-update:\n%s", out)
			}
		})
	}
}

func TestSelfUpdateCheckChangesNothing(t *testing.T) {
	r := selfRig(t, winAmd64, "0.0.1")
	r.publishSelf("0.0.2", false, 10)
	if code, it := r.selfItem("--check"); code != exitcode.UpdatesAvailable || it.To != "0.0.2" {
		t.Fatalf("exit %d: %+v", code, it)
	}
	if r.binary() != "gremloader 0.0.1" {
		t.Error("--check changed the binary")
	}
	// Without a terminal and without --yes nothing changes either.
	if code, _, _ := r.run("self-update"); code != exitcode.Usage || r.binary() != "gremloader 0.0.1" {
		t.Errorf("unconfirmed: exit %d, binary %q", code, r.binary())
	}
}

// Pre-releases: a stable binary sees them only with --prerelease; an rc sees them always, and
// moves on to the final release.
func TestSelfUpdateChannel(t *testing.T) {
	r := selfRig(t, linuxAmd64, "0.0.1")
	r.publishSelf("0.0.1", false, 30)
	r.publishSelf("0.0.2-rc.1", true, 10)
	if code, it := r.selfItem("--check"); code != exitcode.OK || it.Action != "" {
		t.Errorf("stable binary: exit %d %+v", code, it)
	}
	if code, it := r.selfItem("--check", "--prerelease"); code != exitcode.UpdatesAvailable || it.To != "0.0.2-rc.1" {
		t.Errorf("--prerelease: exit %d %+v", code, it)
	}

	r = selfRig(t, linuxAmd64, "0.0.2-rc.1")
	r.publishSelf("0.0.2-rc.1", true, 10)
	r.publishSelf("0.0.2-rc.2", true, 5)
	if code, it := r.selfItem("--yes"); code != exitcode.OK || it.To != "0.0.2-rc.2" || r.binary() != "gremloader 0.0.2-rc.2" {
		t.Fatalf("rc to rc: exit %d %+v", code, it)
	}
	r.selfVersion = "0.0.2-rc.2"
	r.publishSelf("0.0.2", false, 1)
	if code, it := r.selfItem("--yes"); code != exitcode.OK || it.From != "0.0.2-rc.2" || it.To != "0.0.2" {
		t.Fatalf("rc to final: exit %d %+v", code, it)
	}
}

func TestSelfUpdateRefusals(t *testing.T) {
	failing := map[string]struct {
		res  attest.Result
		err  error
		code int
	}{
		"attestation failed": {attest.Result{Outcome: attest.Failed, Failures: []string{"another workflow"}}, nil, exitcode.VerifyFailed},
		"no attestation":     {attest.Result{Outcome: attest.None}, nil, exitcode.VerifyFailed},
		"unreachable":        {attest.Result{}, fmt.Errorf("%w: offline", attest.ErrUnreachable), exitcode.Offline},
	}
	for name, f := range failing {
		t.Run(name, func(t *testing.T) {
			r := selfRig(t, winAmd64, "0.0.1")
			r.publishSelf("0.0.2", false, 10)
			r.attest = &fakeAttester{res: f.res, err: f.err}
			if code, it := r.selfItem("--yes"); code != f.code || r.binary() != "gremloader 0.0.1" {
				t.Errorf("exit %d, binary %q: %+v", code, r.binary(), it)
			}
		})
	}

	t.Run("tampered", func(t *testing.T) {
		r := selfRig(t, winAmd64, "0.0.1")
		r.srv.AddRelease("enls", "gremloader", fakerelease.Release{Tag: "v0.0.2", PublishedAt: r.clock.Now().Add(-time.Hour),
			Assets: []fakerelease.Asset{
				{Name: "gremloader-0.0.2-windows-amd64.zip", Data: archives.Zip(archives.X("g/gremloader.exe", "evil"))},
				{Name: "SHA256SUMS.txt", Data: []byte(strings.Repeat("ab", 32) + "  gremloader-0.0.2-windows-amd64.zip\n")},
			}})
		if code, it := r.selfItem("--yes"); code != exitcode.VerifyFailed || r.binary() != "gremloader 0.0.1" {
			t.Errorf("exit %d, binary %q: %+v", code, r.binary(), it)
		}
	})

	t.Run("development build", func(t *testing.T) {
		r := selfRig(t, winAmd64, "dev")
		if code, _, errOut := r.run("self-update", "--yes"); code != exitcode.Usage || !strings.Contains(errOut, "development build") {
			t.Errorf("exit %d: %s", code, errOut)
		}
	})

	t.Run("renamed binary", func(t *testing.T) {
		r := selfRig(t, winAmd64, "0.0.1")
		r.selfExe = filepath.Join(filepath.Dir(r.selfExe), "grem.exe")
		if code, _, errOut := r.run("self-update", "--yes"); code != exitcode.Usage || !strings.Contains(errOut, "gremloader.exe") {
			t.Errorf("exit %d: %s", code, errOut)
		}
	})

	t.Run("downgrade", func(t *testing.T) {
		r := selfRig(t, winAmd64, "0.0.2")
		r.publishSelf("0.0.1", false, 30)
		r.publishSelf("0.0.2", false, 10)
		if code, it := r.selfItem("--yes", "--version", "0.0.1"); code == exitcode.OK || r.binary() != "gremloader 0.0.2" {
			t.Errorf("without --allow-downgrade: exit %d %+v", code, it)
		}
		if code, it := r.selfItem("--yes", "--version", "0.0.1", "--allow-downgrade"); code != exitcode.OK || r.binary() != "gremloader 0.0.1" {
			t.Errorf("with --allow-downgrade: exit %d %+v", code, it)
		}
	})

	t.Run("locked", func(t *testing.T) {
		r := selfRig(t, winAmd64, "0.0.1")
		r.publishSelf("0.0.2", false, 10)
		noErr(t, os.MkdirAll(r.stateDir(), 0o700))
		lk, err := runlock.Acquire(filepath.Join(r.stateDir(), "run.lock"), runlock.Holder{PID: 4242, Version: "test", Since: time.Now()})
		noErr(t, err)
		defer func() { _ = lk.Release() }()
		if code, _, _ := r.run("self-update", "--yes"); code != exitcode.Locked || r.binary() != "gremloader 0.0.1" {
			t.Errorf("exit %d", code)
		}
	})
}

// The running binary is the truth: moved to another folder, it is updated there, not where the
// record said.
func TestSelfUpdateFollowsTheRunningBinary(t *testing.T) {
	r := selfRig(t, linuxAmd64, "0.0.1")
	r.publishSelf("0.0.2", false, 10)
	if code, it := r.selfItem("--yes"); code != exitcode.OK {
		t.Fatalf("exit %d %+v", code, it)
	}
	old := r.selfExe
	r.selfExe, r.selfVersion = filepath.Join(r.root, "elsewhere", selfName), "0.0.2"
	noErr(t, os.MkdirAll(filepath.Dir(r.selfExe), 0o755))
	noErr(t, os.WriteFile(r.selfExe, []byte("gremloader 0.0.2"), 0o600))
	r.publishSelf("0.0.3", false, 1)
	if code, it := r.selfItem("--yes"); code != exitcode.OK || it.From != "0.0.2" || it.Dir != filepath.Dir(r.selfExe) {
		t.Fatalf("exit %d %+v", code, it)
	}
	if r.binary() != "gremloader 0.0.3" {
		t.Errorf("the moved binary holds %q", r.binary())
	}
	if b, _ := os.ReadFile(old); string(b) != "gremloader 0.0.2" {
		t.Errorf("the old folder was changed: %q", b)
	}
}
