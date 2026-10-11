package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/journal"
	"github.com/enls/gremloader/internal/lock"
	"github.com/enls/gremloader/internal/platform"
	"github.com/enls/gremloader/internal/source/github"
	"github.com/enls/gremloader/internal/state"
	"github.com/enls/gremloader/internal/testutil/archives"
	"github.com/enls/gremloader/internal/testutil/fakerelease"
)

func TestPlanInstallsAMissingPackageWithoutTheAPI(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("app", "0.9.0", 40, archives.F("app.exe", "v0.9"))
	r.publish("app", "1.0.0", 10, archives.F("app.exe", "v1.0"))

	p := r.plan(r.engine(), Request{InstallMissing: true})
	it := item(t, p, "app")
	if it.Action != ActInstall || it.To != "1.0.0" || it.Result != ResultPending || it.Err != nil {
		t.Fatalf("item = %+v", it)
	}
	if it.Asset.Name != "app-1.0.0.zip" || it.Checksums == nil || it.Checksums.Name != "SHA256SUMS.txt" || it.Sidecar {
		t.Errorf("asset = %+v, checksums = %+v", it.Asset, it.Checksums)
	}
	if it.Dir != filepath.Join(r.root, "App") {
		t.Errorf("dir = %s", it.Dir)
	}
	// A plain run resolves through the website: no API quota is spent (plan finding 2).
	if n := r.srv.RequestCount("/api/"); n != 0 {
		t.Errorf("%d API requests, want 0", n)
	}
	if got := p.ExitCode(true); got != exitcode.UpdatesAvailable {
		t.Errorf("exit code (check) = %d, want 2", got)
	}
	if got := p.ExitCode(false); got != exitcode.OK {
		t.Errorf("exit code = %d, want 0", got)
	}

	// What the check learned is saved, so `list` needs no network.
	rec := r.state().Packages["app"]
	if rec == nil || rec.Version != "" || rec.LatestSeen == nil || rec.LatestSeen.Version != "1.0.0" || rec.LastCheck == nil {
		t.Errorf("state record = %+v", rec)
	}
}

func TestPlanWithoutInstallMissingSkipsAMissingPackage(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("app", "1.0.0", 10, archives.F("app.exe", "x"))
	it := item(t, r.plan(r.engine(), Request{}), "app")
	if it.Action != ActNone || it.Result != ResultSkipped {
		t.Errorf("item = %+v", it)
	}
}

func TestPlanUpdate(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("app", "1.0.0", 10, archives.F("app.exe", "x"))
	r.installed("app", "0.9.0", filepath.Join(r.root, "App"))
	it := item(t, r.plan(r.engine(), Request{}), "app")
	if it.Action != ActUpdate || it.From != "0.9.0" || it.To != "1.0.0" {
		t.Errorf("item = %+v", it)
	}
}

func TestPlanUnknownPackageIsAnError(t *testing.T) {
	r := newRig(t, appFile)
	if _, err := r.engine().Plan(context.Background(), Request{Packages: []string{"nope"}}); err == nil {
		t.Error("planned a package that is not in the file")
	}
	if _, err := r.engine().Plan(context.Background(), Request{Want: "1.0.0"}); err == nil {
		t.Error("an explicit version was accepted for every package at once")
	}
}

// P3-6: a package that vanished from the file is reported, never acted on.
func TestPlanReportsUnmanagedPackages(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("app", "1.0.0", 10, archives.F("app.exe", "x"))
	r.installed("oldapp", "2.0.0", filepath.Join(r.root, "Old"))
	p := r.plan(r.engine(), Request{InstallMissing: true})
	it := item(t, p, "oldapp")
	if it.Result != ResultUnmanaged || it.Action != ActNone || it.From != "2.0.0" {
		t.Errorf("item = %+v", it)
	}
	// A record that only holds what a check learned (never installed) is not an install.
	st := r.state()
	st.Packages["checkedonly"] = &state.Package{LastCheck: ptr(r.clock.Now())}
	if err := state.Save(r.paths.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	for _, it := range r.plan(r.engine(), Request{InstallMissing: true}).Items {
		if it.Package == "checkedonly" {
			t.Errorf("a never-installed record was reported: %+v", it)
		}
	}
	// Naming packages explicitly lists only those.
	p = r.plan(r.engine(), Request{Packages: []string{"app"}})
	if len(p.Items) != 1 {
		t.Errorf("items = %d, want 1", len(p.Items))
	}
}

// P3-4: a fresh install never lands in someone else's folder by accident.
func TestPlanInstallFolderRules(t *testing.T) {
	setup := func(t *testing.T) *rig {
		r := newRig(t, appFile)
		r.publish("app", "1.0.0", 10, archives.F("app.exe", "x"))
		return r
	}
	t.Run("non-empty folder is refused", func(t *testing.T) {
		r := setup(t)
		if err := os.MkdirAll(filepath.Join(r.root, "App"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.root, "App", "mine.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		it := item(t, r.plan(r.engine(), Request{InstallMissing: true}), "app")
		if it.Result != ResultFailed || it.Err == nil || !strings.Contains(it.Err.Error(), "adopt") {
			t.Errorf("item = %+v", it)
		}
		it = item(t, r.plan(r.engine(), Request{InstallMissing: true, Overlay: true}), "app")
		if it.Action != ActInstall || len(it.Notes) == 0 {
			t.Errorf("with --overlay: %+v", it)
		}
	})
	t.Run("empty folder is fine", func(t *testing.T) {
		r := setup(t)
		if err := os.MkdirAll(filepath.Join(r.root, "App"), 0o755); err != nil {
			t.Fatal(err)
		}
		if it := item(t, r.plan(r.engine(), Request{InstallMissing: true}), "app"); it.Action != ActInstall {
			t.Errorf("item = %+v", it)
		}
	})
	t.Run("missing parent needs --create-dir", func(t *testing.T) {
		r := setup(t)
		r.writeFile(strings.Replace(appFile, `"{root}/App"`, `"{root}/Typo/App"`, 1))
		it := item(t, r.plan(r.engine(), Request{InstallMissing: true}), "app")
		if it.Result != ResultFailed || !strings.Contains(it.Err.Error(), "--create-dir") {
			t.Errorf("item = %+v", it)
		}
		if it := item(t, r.plan(r.engine(), Request{InstallMissing: true, CreateDir: true}), "app"); it.Action != ActInstall {
			t.Errorf("with --create-dir: %+v", it)
		}
	})
}

func TestPlanNeedsRepairAfterAnUnfinishedChange(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("app", "1.0.0", 10, archives.F("app.exe", "x"))
	r.installed("app", "0.9.0", filepath.Join(r.root, "App"))
	j, err := journal.Create(r.paths.Journal("app"), journal.Record{
		Op: journal.OpBegin, ID: "x", Pkg: "app", Action: "update", From: "0.9.0", To: "1.0.0",
		Dir: filepath.Join(r.root, "App"), TS: r.clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	p := r.plan(r.engine(), Request{})
	it := item(t, p, "app")
	if it.Result != ResultNeedsRepair || it.Action != ActNone {
		t.Errorf("item = %+v", it)
	}
	if got := p.ExitCode(false); got != exitcode.NeedsRepair {
		t.Errorf("exit = %d, want 6", got)
	}

	// Once committed, the leftover is not a reason to stop.
	if err := j.Append(journal.Record{Op: journal.OpCommit}); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	if it := item(t, r.plan(r.engine(), Request{}), "app"); it.Action != ActUpdate {
		t.Errorf("after commit: %+v", it)
	}
}

// Longest prefix wins (plan E.9): two packages of one repository see only their own tags.
//
// "app" follows the prerelease channel on purpose: app-v2-3.0.0 read under "app-v" is the
// prerelease 2.0.0-3.0.0, which the stable channel would drop anyway and hide the bug.
// "app-cli" shares app's repository *and* prefix (a second asset of the same releases, as
// PdfCraft's -cli tarball): an equal prefix is not a longer one and must not exclude anything.
// "elsewhere" has a longer prefix in another repository, which is none of app's business.
func TestPlanMonorepoPrefixes(t *testing.T) {
	r := newRig(t, `schema_version = 1
[package.app]
repo = "o/mono"
tag_prefix = "app-v"
channel = "prerelease"
asset = "app-{version}.zip"
checksums = "SHA256SUMS.txt"
dir = "{root}/App"

[package.app-cli]
repo = "o/mono"
tag_prefix = "app-v"
asset = "app-{version}.zip"
checksums = "SHA256SUMS.txt"
dir = "{root}/AppCli"

[package.app2]
repo = "o/mono"
tag_prefix = "app-v2-"
asset = "app-{version}.zip"
checksums = "SHA256SUMS.txt"
dir = "{root}/App2"

[package.elsewhere]
repo = "o/other"
tag_prefix = "app-v1"
asset = "x-{version}.zip"
checksums = "SHA256SUMS.txt"
dir = "{root}/Elsewhere"
`)
	r.publishAs("mono", "app-v1.0.0", "app-1.0.0.zip", 20, archives.F("a", "1"))
	r.publishAs("mono", "app-v2-3.0.0", "app-3.0.0.zip", 10, archives.F("a", "3"))
	p := r.plan(r.engine(), Request{InstallMissing: true})
	for name, want := range map[string]string{"app": "1.0.0", "app-cli": "1.0.0", "app2": "3.0.0"} {
		if it := item(t, p, name); it.To != want {
			t.Errorf("%s = %+v, want %s", name, it, want)
		}
	}
}

// An install the package file now places elsewhere stays where it is: an update does not move it.
func TestPlanKeepsTheInstalledDirectory(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("app", "1.0.0", 10, archives.F("app.exe", "x"))
	old := filepath.Join(r.root, "OldPlace")
	r.installed("app", "0.9.0", old)
	it := item(t, r.plan(r.engine(), Request{}), "app")
	if it.Dir != old || len(it.Notes) != 1 || !strings.Contains(it.Notes[0], "stays in") {
		t.Errorf("item = %+v", it)
	}
}

// Requiring GitHub's digest means the website path (which has no digest) will not do.
func TestPlanRequiredDigestUsesTheAPI(t *testing.T) {
	r := newRig(t, appFile+"verify = { require = [\"checksum\", \"digest\"] }\n")
	r.publish("app", "1.0.0", 10, archives.F("app.exe", "x"))
	it := item(t, r.plan(r.engine(), Request{InstallMissing: true}), "app")
	if it.Action != ActInstall || it.Via != github.ModeAPI || it.Asset.SHA256 == "" {
		t.Errorf("item = %+v", it)
	}
}

func TestPlanFailureIsRecordedAndOtherPackagesContinue(t *testing.T) {
	r := newRig(t, appFile+`
[package.gone]
repo = "o/gone"
asset = "gone-{version}.zip"
checksums = "SHA256SUMS.txt"
dir = "{root}/Gone"
`)
	r.publish("app", "1.0.0", 10, archives.F("app.exe", "x"))
	p := r.plan(r.engine(), Request{InstallMissing: true})
	if it := item(t, p, "gone"); it.Result != ResultFailed || !errors.Is(it.Err, github.ErrNotFound) {
		t.Errorf("gone = %+v", it)
	}
	if it := item(t, p, "app"); it.Action != ActInstall {
		t.Errorf("app = %+v", it)
	}
	if got := p.ExitCode(false); got != exitcode.Failed {
		t.Errorf("exit = %d, want 1", got)
	}
	if rec := r.state().Packages["gone"]; rec == nil || rec.LastError == "" {
		t.Errorf("last_error not recorded: %+v", rec)
	}
	got := map[string]string{}
	for _, ev := range r.events() {
		got[ev.Package] = ev.Event + " " + ev.To
	}
	if got["gone"] != "check.fail " || got["app"] != "check.ok 1.0.0" {
		t.Errorf("events = %v", got)
	}
}

func TestPlanRateLimitedEverywhereIsExit7(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("app", "1.0.0", 10, archives.F("app.exe", "x"))
	r.srv.SetRateLimited(true)
	r.srv.Inject(fakeFeedDown())
	p := r.plan(r.engine(), Request{InstallMissing: true})
	if got := p.ExitCode(false); got != exitcode.Offline {
		t.Errorf("exit = %d, want 7 (item %+v)", got, item(t, p, "app"))
	}
}

func TestPlanFromTheLockMakesNoRequest(t *testing.T) {
	r := newRig(t, appFile)
	lf := lock.New("test", r.clock.Now())
	lf.Packages["app"] = &lock.Package{
		Repo: "o/app", Tag: "v1.0.0", Version: "1.0.0",
		Platforms: map[string]*lock.Asset{platform.Current().String(): {
			Asset: "app-1.0.0.zip", URL: r.srv.AssetURL("o", "app", "v1.0.0", "app-1.0.0.zip"),
			SHA256: strings.Repeat("ab", 32), Size: 42,
		}},
	}
	it := item(t, r.plan(r.engine(), Request{InstallMissing: true, Locked: lf}), "app")
	if it.Action != ActInstall || !it.Locked || it.LockHash != strings.Repeat("ab", 32) || it.Asset.Name != "app-1.0.0.zip" {
		t.Errorf("item = %+v", it)
	}
	if n := len(r.srv.Requests()); n != 0 {
		t.Errorf("%d requests, want 0", n)
	}
}

func TestPlanRequiredAttestationCannotBeMetYet(t *testing.T) {
	r := newRig(t, strings.Replace(appFile, `dir       = "{root}/App"`,
		`dir       = "{root}/App"`+"\nverify = { require = [\"checksum\", \"attestation\"] }", 1))
	r.publish("app", "1.0.0", 10, archives.F("app.exe", "x"))
	p := r.plan(r.engine(), Request{InstallMissing: true})
	var ve *VerifyError
	if it := item(t, p, "app"); it.Result != ResultFailed || !errors.As(it.Err, &ve) {
		t.Errorf("item = %+v", it)
	}
	if got := p.ExitCode(false); got != exitcode.VerifyFailed {
		t.Errorf("exit = %d, want 5", got)
	}
}

func TestPlanWildcardAssetUsesTheAPI(t *testing.T) {
	r := newRig(t, strings.Replace(appFile, `"app-{version}.zip"`, `"app-*.zip"`, 1))
	r.publish("app", "1.0.0", 10, archives.F("app.exe", "x"))
	it := item(t, r.plan(r.engine(), Request{InstallMissing: true}), "app")
	if it.Action != ActInstall || it.Asset.Name != "app-1.0.0.zip" || it.Via != github.ModeAPI || it.Asset.SHA256 == "" {
		t.Errorf("item = %+v", it)
	}
}

// P3-9: files changed since the install are reported, never a reason to stop.
func TestPlanNotesDrift(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("app", "1.0.0", 10, archives.F("app.exe", "x"))
	dir := filepath.Join(r.root, "App")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.exe"), []byte("edited by hand"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.installed("app", "0.9.0", dir)
	st := r.state()
	st.Packages["app"].Adopted = false
	st.Packages["app"].Manifest = state.ManifestRel("app", "0.9.0")
	if err := state.Save(r.paths.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	m := &state.Manifest{Files: []state.ManifestFile{
		{Path: "app.exe", Size: 4, SHA256: strings.Repeat("0", 64)},
		{Path: "lib.dll", Size: 1, SHA256: strings.Repeat("0", 64)},
	}}
	if err := os.MkdirAll(filepath.Dir(state.Path(r.paths.State, st.Packages["app"].Manifest)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveManifest(state.Path(r.paths.State, st.Packages["app"].Manifest), m); err != nil {
		t.Fatal(err)
	}
	it := item(t, r.plan(r.engine(), Request{}), "app")
	if it.Action != ActUpdate || len(it.Notes) != 1 || !strings.Contains(it.Notes[0], "2 installed file(s) changed") {
		t.Errorf("item = %+v", it)
	}
}

func TestPlanDisabledPackageIsSkipped(t *testing.T) {
	r := newRig(t, appFile+"enabled = false\n")
	it := item(t, r.plan(r.engine(), Request{InstallMissing: true}), "app")
	if it.Result != ResultSkipped || it.Reason != "disabled" || len(r.srv.Requests()) != 0 {
		t.Errorf("item = %+v", it)
	}
}

// Exit 7 means nothing could be resolved. When other packages resolved, an unreachable one is an
// ordinary failure.
func TestPlanOneRateLimitedPackageAmongResolvedOnesIsExit1(t *testing.T) {
	r := newRig(t, appFile+`
[package.b]
repo = "o/b"
asset = "b-{version}.zip"
checksums = "SHA256SUMS.txt"
dir = "{root}/B"
`)
	r.publish("app", "1.0.0", 10, archives.F("app.exe", "x"))
	r.srv.Inject(fakerelease.Fault{PathContains: "o/b/", Status: 429})
	p := r.plan(r.engine(), Request{InstallMissing: true})
	if it := item(t, p, "b"); it.Result != ResultFailed || !isOffline(it.Err) {
		t.Fatalf("b = %+v", it)
	}
	if got := p.ExitCode(false); got != exitcode.Failed {
		t.Errorf("exit = %d, want 1", got)
	}
}

// fakeFeedDown makes the website path fail too, so nothing is reachable.
func fakeFeedDown() fakerelease.Fault { return fakerelease.Fault{PathContains: "/web/", Status: 503} }
