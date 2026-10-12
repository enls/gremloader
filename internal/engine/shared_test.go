package engine

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/journal"
	"github.com/enls/gremloader/internal/proc"
	"github.com/enls/gremloader/internal/state"
	"github.com/enls/gremloader/internal/testutil/archives"
)

// sharedFile has two packages that install into one shared folder, as the Craft Linux tarballs do
// into ~/.local (decision 2 of the M5 bug check).
const sharedFile = `schema_version = 1

[preset.s]
checksums  = "SHA256SUMS.txt"
dir        = "{root}/App"
shared_dir = true

[package.app]
use   = "s"
repo  = "o/app"
asset = "app-{version}.zip"

[package.b]
use   = "s"
repo  = "o/b"
asset = "b-{version}.zip"
`

func appFiles(v string) []archives.Entry {
	return []archives.Entry{
		archives.X("bin/app", "app "+v),
		archives.F("share/doc/app/README", "readme "+v),
		archives.F("share/applications/app.desktop", "[Desktop Entry]\nExec=app\n"),
	}
}

func bFiles(v string) []archives.Entry {
	return []archives.Entry{
		archives.X("bin/b", "b "+v),
		archives.F("share/doc/b/README", "b readme "+v),
		archives.F("share/applications/b.desktop", "[Desktop Entry]\nExec=b\n"),
	}
}

// userShared is what the user (and other tools) already keep in the shared folder.
var userShared = map[string]string{"bin/": "", "bin/other-tool": "theirs", "notes.txt": "mine"}

func newSharedRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, sharedFile)
	for p, c := range userShared {
		if strings.HasSuffix(p, "/") {
			continue
		}
		f := filepath.Join(r.dir(), filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.publish("app", "1.0.0", 30, appFiles("1")...)
	r.publishAs("b", "v1.0.0", "b-1.0.0.zip", 30, bFiles("1")...)
	return r
}

// bothInstalled: app 1.0.0 and b 1.0.0 in the shared folder, app 2.0.0 published.
func bothInstalled(t *testing.T) *rig {
	t.Helper()
	r := newSharedRig(t)
	for _, it := range r.apply(r.engine(), Request{InstallMissing: true}).Items {
		if it.Result != ResultInstalled {
			t.Fatalf("installing %s: %+v (%v)", it.Package, it, it.Err)
		}
	}
	r.publish("app", "2.0.0", 20, appFiles("2")...)
	return r
}

func sharedTree(app, b string) map[string]string {
	m := maps.Clone(userShared)
	m["share/"] = ""
	m["share/doc/"] = ""
	m["share/applications/"] = ""
	if app != "" {
		maps.Copy(m, map[string]string{"bin/app": "app " + app, "share/doc/app/": "", "share/doc/app/README": "readme " + app,
			"share/applications/app.desktop": "[Desktop Entry]\nExec=app\n"})
	}
	if b != "" {
		maps.Copy(m, map[string]string{"bin/b": "b " + b, "share/doc/b/": "", "share/doc/b/README": "b readme " + b,
			"share/applications/b.desktop": "[Desktop Entry]\nExec=b\n"})
	}
	return m
}

func manifestDirs(t *testing.T, r *rig, name string) []string {
	t.Helper()
	m, err := state.LoadManifest(state.Path(r.paths.State, r.state().Packages[name].Manifest))
	if err != nil {
		t.Fatal(err)
	}
	return m.Dirs
}

func TestSharedDirTakesSeveralPackagesAndTheUsersFiles(t *testing.T) {
	r := bothInstalled(t)
	assertTree(t, r.dir(), sharedTree("1", "1"))
	rec := r.state().Packages["app"]
	if !rec.SharedDir || !r.state().Packages["b"].SharedDir {
		t.Errorf("state records do not say shared: %+v", rec)
	}
	// Top-level folders are never the package's, even when its install created them (share/).
	for _, n := range []string{"app", "b"} {
		for _, d := range manifestDirs(t, r, n) {
			if !strings.Contains(d, "/") {
				t.Errorf("%s claims the top-level folder %s", n, d)
			}
		}
	}

	// An update of one package leaves the other's files alone.
	if it := item(t, r.apply(r.engine(), Request{}), "app"); it.Result != ResultUpdated {
		t.Fatalf("update = %+v (%v)", it, it.Err)
	}
	assertTree(t, r.dir(), sharedTree("2", "1"))

	// Removing one takes exactly its files; the other's, the user's and bin/ share/ stay.
	if rm := r.engine().Remove(context.Background(), "app", RemoveOptions{}); rm.Result != ResultRemoved {
		t.Fatalf("remove = %+v (%v)", rm, rm.Err)
	}
	assertTree(t, r.dir(), sharedTree("", "1"))
	if rm := r.engine().Remove(context.Background(), "b", RemoveOptions{}); rm.Result != ResultRemoved {
		t.Fatalf("remove b = %+v (%v)", rm, rm.Err)
	}
	got := tree(t, r.dir())
	for _, keep := range []string{"bin/", "bin/other-tool", "notes.txt", "share/"} {
		if _, ok := got[keep]; !ok {
			t.Errorf("%s went with the packages:\n%s", keep, show(got))
		}
	}
	for _, gone := range []string{"bin/app", "bin/b", "share/doc/app/", "share/doc/b/", "share/applications/app.desktop",
		"share/applications/b.desktop"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s was left behind:\n%s", gone, show(got))
		}
	}
}

// Decision 1 of the M7 planning: a file another installed package owns is never overwritten, in
// a shared folder or any folder two packages use; nothing changes.
func TestSharedDirRefusesAnotherPackagesFile(t *testing.T) {
	r := newSharedRig(t)
	if it := item(t, r.apply(r.engine(), Request{Packages: []string{"app"}, InstallMissing: true}), "app"); it.Result != ResultInstalled {
		t.Fatalf("install app = %+v (%v)", it, it.Err)
	}
	before := tree(t, r.dir())
	r.publishAs("b", "v1.1.0", "b-1.1.0.zip", 20, append(bFiles("1"), archives.F("share/doc/APP/readme", "mine now"))...)
	it := item(t, r.apply(r.engine(), Request{Packages: []string{"b"}, InstallMissing: true}), "b")
	if it.Result != ResultFailed || it.Err == nil || !strings.Contains(it.Err.Error(), "share/doc/APP/readme") ||
		!strings.Contains(it.Err.Error(), "app") {
		t.Fatalf("install b over app's file = %+v (%v)", it, it.Err)
	}
	assertTree(t, r.dir(), before)
	if r.state().Packages["b"] != nil && r.state().Packages["b"].Version != "" {
		t.Error("b was recorded as installed")
	}
	if journal.Exists(r.paths.Journal("b")) {
		t.Error("b left a journal")
	}
	if _, err := os.Stat(r.paths.Staging("b")); err == nil {
		t.Error("b left staging")
	}

	// An update that would take another package's file is refused the same way.
	r = bothInstalled(t)
	r.publish("app", "3.0.0", 10, append(appFiles("3"), archives.X("bin/b", "app's b"))...)
	before = tree(t, r.dir())
	it = item(t, r.apply(r.engine(), Request{Packages: []string{"app"}, Want: "3.0.0"}), "app")
	if it.Result != ResultFailed || it.Err == nil || !strings.Contains(it.Err.Error(), "bin/b") {
		t.Fatalf("update over b's file = %+v (%v)", it, it.Err)
	}
	assertTree(t, r.dir(), before)
	r.assertClean()
	if v := r.state().Packages["app"].Version; v != "1.0.0" {
		t.Errorf("app is at %s", v)
	}
}

func TestSharedDirIsNeverDeletedWhole(t *testing.T) {
	r := bothInstalled(t)
	before := tree(t, r.dir())
	rm := r.engine().Remove(context.Background(), "app", RemoveOptions{DeleteDir: true})
	if rm.Result != ResultFailed || rm.Err == nil || !strings.Contains(rm.Err.Error(), "shared") {
		t.Fatalf("remove --delete-dir = %+v (%v)", rm, rm.Err)
	}
	assertTree(t, r.dir(), before)
	if len(r.hooks.ran) != 0 {
		t.Errorf("hooks ran for a refused remove: %+v", r.hooks.ran)
	}

	// Even a record with no file list (what an adopt without --verify would have left).
	st := r.state()
	st.Packages["b"].Manifest = ""
	if err := state.Save(r.paths.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	for _, o := range []RemoveOptions{{}, {DeleteDir: true}} {
		if rm := r.engine().Remove(context.Background(), "b", o); rm.Result != ResultFailed || rm.Err == nil ||
			!strings.Contains(rm.Err.Error(), "without a file list") {
			t.Fatalf("remove %+v of b without a file list = %+v (%v)", o, rm, rm.Err)
		}
	}
	assertTree(t, r.dir(), before)
}

func TestSharedDirAdoptNeedsVerify(t *testing.T) {
	r := newSharedRig(t)
	// app 1.0.0's files are there (put there by something else), nothing recorded.
	for p, c := range map[string]string{"bin/app": "app 1", "share/doc/app/README": "readme 1",
		"share/applications/app.desktop": "[Desktop Entry]\nExec=app\n"} {
		f := filepath.Join(r.dir(), filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The same files, released again with an empty top-level folder the folder lacks: a shared
	// folder's top level is never claimed, even then.
	r.publish("app", "1.1.0", 20, append(appFiles("1"), archives.D("var"))...)
	it := r.engine().Adopt(context.Background(), "app", AdoptOptions{Version: "1.1.0"})
	if it.Result != ResultFailed || it.Err == nil || !strings.Contains(it.Err.Error(), "--verify") {
		t.Fatalf("adopt without --verify = %+v (%v)", it, it.Err)
	}
	it = r.engine().Adopt(context.Background(), "app", AdoptOptions{Version: "1.1.0", Verify: true})
	if it.Result != ResultAdopted {
		t.Fatalf("adopt --verify = %+v (%v)", it, it.Err)
	}
	rec := r.state().Packages["app"]
	if !rec.SharedDir || rec.Manifest == "" {
		t.Errorf("adopted record = %+v", rec)
	}
	for _, d := range manifestDirs(t, r, "app") {
		if !strings.Contains(d, "/") {
			t.Errorf("adopt claims the top-level folder %s", d)
		}
	}
	if rm := r.engine().Remove(context.Background(), "app", RemoveOptions{}); rm.Result != ResultRemoved {
		t.Fatalf("remove = %+v (%v)", rm, rm.Err)
	}
	got := tree(t, r.dir())
	for _, p := range []string{"bin/app", "share/doc/app/README", "share/applications/app.desktop"} {
		if _, ok := got[p]; ok {
			t.Errorf("%s survived the remove", p)
		}
	}
	for p, c := range userShared {
		if got[p] != c {
			t.Errorf("%s = %q after the remove", p, got[p])
		}
	}
}

// In a shared folder only the package's own programs stop a change to it: one app running from
// ~/.local/bin must not hold up the other eleven.
func TestSharedDirRunningCheckLooksAtThePackagesOwnFiles(t *testing.T) {
	r := bothInstalled(t)
	r.running = []proc.Process{{PID: 7, Path: filepath.Join(r.dir(), "bin", "b")}, {PID: 8, Path: filepath.Join(r.dir(), "bin", "other-tool")}}
	if it := item(t, r.apply(r.engine(), Request{}), "app"); it.Result != ResultUpdated {
		t.Fatalf("update of app while b runs = %+v (%v)", it, it.Err)
	}
	if rb := r.engine().Rollback(context.Background(), "app"); rb.Result != ResultRolledBack {
		t.Fatalf("rollback of app while b runs = %+v (%v)", rb, rb.Err)
	}

	r.running = []proc.Process{{PID: 9, Path: filepath.Join(r.dir(), "BIN", "app")}}
	if it := item(t, r.apply(r.engine(), Request{Want: "2.0.0", Packages: []string{"app"}}), "app"); it.Result != ResultSkipped ||
		!strings.Contains(it.Reason, "running") {
		t.Fatalf("update of app while it runs = %+v", it)
	}
	if rm := r.engine().Remove(context.Background(), "app", RemoveOptions{}); rm.Result != ResultSkipped {
		t.Fatalf("remove of app while it runs = %+v", rm)
	}
	if rm := r.engine().Remove(context.Background(), "b", RemoveOptions{}); rm.Result != ResultRemoved {
		t.Fatalf("remove of b while app runs = %+v (%v)", rm, rm.Err)
	}
	if _, err := os.Stat(r.paths.Staging("app")); err == nil {
		t.Error("the skipped update left staging")
	}
}

// The shared-folder update keeps the old-or-new guarantee and never touches b, under a failure or
// a crash at every point.
func TestSharedUpdateUnderFailuresAndCrashes(t *testing.T) {
	sweep(t, bothInstalled, func(e *Engine) (*Item, error) {
		p, err := e.Plan(context.Background(), Request{Packages: []string{"app"}})
		if err != nil {
			return nil, err
		}
		err = e.Apply(context.Background(), p, ApplyOptions{})
		return p.Items[0], err
	}, sharedTree("1", "1"), sharedTree("2", "1"), func(t *testing.T, r *rig) {
		if !slices.Contains([]string{"1.0.0", "2.0.0"}, r.state().Packages["app"].Version) || r.state().Packages["b"].Version != "1.0.0" {
			t.Errorf("state: app %+v b %+v", r.state().Packages["app"], r.state().Packages["b"])
		}
	})
}

// A package once installed into a shared folder stays treated as one when the package file stops
// saying so: state.json's record decides, the safe direction.
func TestSharedDirStaysSharedWhenThePackageFileForgetsIt(t *testing.T) {
	r := bothInstalled(t)
	r.writeFile(strings.Replace(sharedFile, "shared_dir = true\n", "", 1))
	r.running = []proc.Process{{PID: 7, Path: filepath.Join(r.dir(), "bin", "b")}}
	if it := item(t, r.apply(r.engine(), Request{Packages: []string{"app"}}), "app"); it.Result != ResultUpdated {
		t.Fatalf("update = %+v (%v)", it, it.Err)
	}
	if !r.state().Packages["app"].SharedDir {
		t.Error("the record lost shared_dir")
	}
	for _, d := range manifestDirs(t, r, "app") {
		if !strings.Contains(d, "/") {
			t.Errorf("the update claims the top-level folder %s", d)
		}
	}
}

// What a change may touch, for the running check: the files of the version being replaced (an
// update removes them) and of the version a rollback returns to, compared without case.
func TestSharedDirRunningCheckCoversOldAndRollbackFiles(t *testing.T) {
	r := newSharedRig(t)
	r.publish("app", "1.5.0", 25, append(appFiles("1.5"), archives.X("bin/App-Helper", "helper"))...)
	if it := item(t, r.apply(r.engine(), Request{Packages: []string{"app"}, Want: "1.5.0", InstallMissing: true}), "app"); it.Result != ResultInstalled {
		t.Fatalf("install 1.5.0 = %+v (%v)", it, it.Err)
	}
	r.publish("app", "2.0.0", 20, appFiles("2")...) // no helper any more
	helper := []proc.Process{{PID: 7, Path: filepath.Join(r.dir(), "bin", "app-helper")}}

	r.running = helper
	if it := item(t, r.apply(r.engine(), Request{Packages: []string{"app"}}), "app"); it.Result != ResultSkipped {
		t.Fatalf("update while the old version's helper runs = %+v (%v)", it, it.Err)
	}
	r.running = nil
	if it := item(t, r.apply(r.engine(), Request{Packages: []string{"app"}}), "app"); it.Result != ResultUpdated {
		t.Fatalf("update = %+v (%v)", it, it.Err)
	}
	r.running = helper
	if rb := r.engine().Rollback(context.Background(), "app"); rb.Result != ResultSkipped {
		t.Fatalf("rollback while a program runs from a file it would put back = %+v (%v)", rb, rb.Err)
	}
}

// The running check of a shared folder also reads the rollback version's manifest; when that one is
// missing, the error names it, not the installed version's.
func TestSharedDirNamesTheMissingRollbackManifest(t *testing.T) {
	r := bothInstalled(t)
	if it := item(t, r.apply(r.engine(), Request{Packages: []string{"app"}}), "app"); it.Result != ResultUpdated {
		t.Fatalf("update = %+v (%v)", it, it.Err)
	}
	rec := r.state().Packages["app"]
	if rec.Previous == nil || rec.Previous.Manifest == "" {
		t.Fatalf("no rollback manifest recorded: %+v", rec)
	}
	if err := os.Remove(state.Path(r.paths.State, rec.Previous.Manifest)); err != nil {
		t.Fatal(err)
	}
	rb := r.engine().Rollback(context.Background(), "app")
	if rb.Err == nil || !strings.Contains(rb.Err.Error(), "rollback version's manifest") {
		t.Fatalf("rollback = %+v (%v)", rb, rb.Err)
	}
}

// The ownership check is about one folder: packages in folders of their own may ship the same
// paths.
func TestOwnershipIsPerFolder(t *testing.T) {
	r := newRig(t, `schema_version = 1

[package.app]
repo      = "o/app"
asset     = "app-{version}.zip"
checksums = "SHA256SUMS.txt"
dir       = "{root}/App"

[package.b]
repo      = "o/b"
asset     = "b-{version}.zip"
checksums = "SHA256SUMS.txt"
dir       = "{root}/B"
`)
	r.publish("app", "1.0.0", 10, archives.X("bin/tool", "app's"))
	r.publishAs("b", "v1.0.0", "b-1.0.0.zip", 10, archives.X("bin/tool", "b's"))
	for _, it := range r.apply(r.engine(), Request{InstallMissing: true}).Items {
		if it.Result != ResultInstalled {
			t.Errorf("%s = %+v (%v)", it.Package, it, it.Err)
		}
	}
}

// A rollback puts back files the update removed; one that another package has taken since is
// refused like an install's.
func TestRollbackRefusesAnotherPackagesFile(t *testing.T) {
	r := newSharedRig(t)
	r.publish("app", "1.5.0", 25, append(appFiles("1.5"), archives.X("bin/helper", "app's helper"))...)
	if it := item(t, r.apply(r.engine(), Request{Packages: []string{"app"}, Want: "1.5.0", InstallMissing: true}), "app"); it.Result != ResultInstalled {
		t.Fatalf("install 1.5.0 = %+v (%v)", it, it.Err)
	}
	r.publish("app", "2.0.0", 20, appFiles("2")...)
	if it := item(t, r.apply(r.engine(), Request{Packages: []string{"app"}}), "app"); it.Result != ResultUpdated {
		t.Fatalf("update = %+v (%v)", it, it.Err)
	}
	r.publishAs("b", "v1.1.0", "b-1.1.0.zip", 10, append(bFiles("1"), archives.X("bin/helper", "b's helper"))...)
	if it := item(t, r.apply(r.engine(), Request{Packages: []string{"b"}, InstallMissing: true}), "b"); it.Result != ResultInstalled {
		t.Fatalf("install b = %+v (%v)", it, it.Err)
	}
	before := tree(t, r.dir())
	rb := r.engine().Rollback(context.Background(), "app")
	if rb.Result != ResultFailed || rb.Err == nil || !strings.Contains(rb.Err.Error(), "bin/helper") ||
		!strings.Contains(rb.Err.Error(), "belongs to b") {
		t.Fatalf("rollback over b's file = %+v (%v)", rb, rb.Err)
	}
	assertTree(t, r.dir(), before)
	r.assertClean()
}

// Self-update's adoption (ADR 0015): the running binary is recorded at its own version in a shared
// folder without a file list, a record that no longer describes it is replaced, and the next
// update goes from that version.
func TestAdoptSelf(t *testing.T) {
	ctx := context.Background()
	r := bothInstalled(t)
	if it := item(t, r.apply(r.engine(), Request{Packages: []string{"app"}}), "app"); it.Result != ResultUpdated {
		t.Fatalf("update to 2.0.0: %+v (%v)", it, it.Err)
	}
	// The user put app 3.0.0 there by hand; 4.0.0 is out.
	for _, f := range appFiles("3") {
		write(t, filepath.Join(r.dir(), filepath.FromSlash(f.Name)), string(f.Data))
	}
	r.publish("app", "3.0.0", 10, appFiles("3")...)
	r.publish("app", "4.0.0", 5, appFiles("4")...)
	bBefore := *r.state().Packages["b"]

	if it := r.engine().Adopt(ctx, "app", AdoptOptions{Version: "3.0.0"}); it.Result != ResultFailed {
		t.Fatalf("adopt over a record without Self: %+v", it)
	}
	if it := r.engine().Adopt(ctx, "app", AdoptOptions{Version: "3.0.0", Self: true}); it.Result != ResultAdopted || len(it.Notes) != 0 {
		t.Fatalf("adopt Self: %+v (%v)", it, it.Err)
	}
	rec := r.state().Packages["app"]
	if rec.Version != "3.0.0" || rec.Manifest != "" || rec.Previous != nil || !rec.Adopted || !rec.SharedDir {
		t.Errorf("record %+v", rec)
	}
	if ents, _ := os.ReadDir(r.paths.Manifests("app")); len(ents) != 0 {
		t.Errorf("%d manifests of the replaced record left", len(ents))
	}
	if _, err := os.Stat(r.paths.Previous("app")); !os.IsNotExist(err) {
		t.Errorf("the replaced record's rollback copy: %v", err)
	}
	if b := *r.state().Packages["b"]; !reflect.DeepEqual(b, bBefore) {
		t.Errorf("b's record changed:\n%+v\n%+v", b, bBefore)
	}

	it := item(t, r.apply(r.engine(), Request{Packages: []string{"app"}}), "app")
	if it.Result != ResultUpdated || it.From != "3.0.0" || it.To != "4.0.0" {
		t.Fatalf("update after adopt: %+v (%v)", it, it.Err)
	}
	assertTree(t, r.dir(), sharedTree("4", "1"))

	// Nothing recorded yet: a shared folder is adopted without --verify.
	r = newSharedRig(t)
	for _, f := range appFiles("1") {
		write(t, filepath.Join(r.dir(), filepath.FromSlash(f.Name)), string(f.Data))
	}
	if it := r.engine().Adopt(ctx, "app", AdoptOptions{Version: "1.0.0", Self: true}); it.Result != ResultAdopted {
		t.Fatalf("first adopt Self: %+v (%v)", it, it.Err)
	}
}
