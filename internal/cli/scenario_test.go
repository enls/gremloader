package cli

// The CraftWerk scenario port (plan E.5, M7): what CraftWerk does, behaviour C1-C19 of plan Part
// A, as tests of the command line with the shipped examples/craft/gremloader.toml against a fake
// GitHub serving synthetic Craft releases (the real archive shapes, the version inside every
// file). The Windows layout (one folder per app, register-apps.ps1) and the Linux one (all apps in
// a shared ~/.local) both run on every host, through the platform seam; only real processes, file
// permissions and sh are host-bound, and those tests say so. C20 (the schedule) and C21 (.msi
// report) are not ported (plan E.7).

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/pkgfile"
	"github.com/enls/gremloader/internal/platform"
	"github.com/enls/gremloader/internal/proc"
	"github.com/enls/gremloader/internal/state"
	"github.com/enls/gremloader/internal/testutil/archives"
	"github.com/enls/gremloader/internal/testutil/fakerelease"
)

var (
	winLayout = platform.Platform{OS: "windows", Arch: "amd64"}
	linLayout = platform.Platform{OS: "linux", Arch: "amd64"}
	layouts   = map[string]platform.Platform{"windows": winLayout, "linux": linLayout}
)

// craftRig runs the shipped Craft file for one layout, with every app's 1.0.0 published 30 days
// ago and hooks recorded instead of run.
func craftRig(t *testing.T, layout platform.Platform) *rig {
	t.Helper()
	b, err := os.ReadFile(craftFile)
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, string(b))
	r.platform = layout
	r.hooks = &recordHooks{}
	if err := os.MkdirAll(r.deps().home, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, app := range craftApps {
		r.publishCraft(app, "1.0.0", 30, craftRelease{})
	}
	return r
}

// craftRelease varies a published release.
type craftRelease struct {
	extra  []archives.Entry               // more files, under the archives' top folder
	sums   func(name, hash string) string // one SHA256SUMS.txt line; nil: "<hash>  <name>"
	damage bool                           // the assets are not archives (their real hashes are listed)
}

// publishCraft publishes app v as the Craft repos do: a Windows portable zip, a Linux tarball (and
// for pdfcraft its second, -cli- tarball), and a SHA256SUMS.txt naming them.
func (r *rig) publishCraft(app, v string, daysAgo int, o craftRelease) {
	r.t.Helper()
	assets := map[string][]byte{
		fmt.Sprintf("%s-%s-windows-x64-portable.zip", app, v): craftZip(app, v, o.extra),
		fmt.Sprintf("%s-%s-linux-x86_64.tar.gz", app, v):      craftTarGz(app, v, o.extra),
	}
	if app == "pdfcraft" {
		assets["pdfcraft-cli-"+v+"-linux-x86_64.tar.gz"] = archives.TarGz(archives.X("pdfcraft-cli/bin/pdfcraft-cli", "the other tarball"))
	}
	if o.damage {
		for n := range assets {
			assets[n] = []byte("not an archive: " + n)
		}
	}
	rel := fakerelease.Release{Tag: "v" + v, PublishedAt: r.clock.Now().Add(-time.Duration(daysAgo) * 24 * time.Hour)}
	var sums strings.Builder
	for _, n := range slices.Sorted(maps.Keys(assets)) {
		sum := sha256.Sum256(assets[n])
		h := hex.EncodeToString(sum[:])
		if o.sums != nil {
			sums.WriteString(o.sums(n, h))
		} else {
			sums.WriteString(h + "  " + n + "\n")
		}
		rel.Assets = append(rel.Assets, fakerelease.Asset{Name: n, Data: assets[n]})
	}
	rel.Assets = append(rel.Assets, fakerelease.Asset{Name: "SHA256SUMS.txt", Data: []byte(sums.String())})
	r.srv.AddRelease("storytold", app, rel)
}

func under(top string, es []archives.Entry) []archives.Entry {
	out := make([]archives.Entry, len(es))
	for i, e := range es {
		e.Name = top + e.Name
		out[i] = e
	}
	return out
}

// craftZip is the Windows portable zip's shape (seven files under one top-level folder, no folder
// entries), with the version in every file.
func craftZip(app, v string, extra []archives.Entry) []byte {
	var es []archives.Entry
	for rel, content := range winFiles(app, v) {
		es = append(es, archives.F(rel, content))
	}
	slices.SortFunc(es, func(a, b archives.Entry) int { return strings.Compare(a.Name, b.Name) })
	return archives.Zip(under(app+"-"+v+"-windows-x64-portable/", append(es, extra...))...)
}

func winFiles(app, v string) map[string]string {
	m := map[string]string{app + ".exe": app + " " + v, app + "-cli.exe": "cli " + app + " " + v}
	for _, n := range []string{"ATTRIBUTION.md", "LICENSE-APACHE", "LICENSE-MIT", "NOTICE", "README.md"} {
		m[n] = n + " for " + app + " " + v + "\n"
	}
	return m
}

// craftTarGz is the Linux tarball's shape (bin/ and share/ under one top-level folder, explicit
// folder entries), with the version in every file.
func craftTarGz(app, v string, extra []archives.Entry) []byte {
	es := []archives.Entry{archives.D("bin"), archives.D("share"), archives.D("share/doc"), archives.D("share/doc/" + app),
		archives.D("share/applications"), archives.D("share/icons"), archives.D("share/icons/hicolor"),
		archives.D("share/icons/hicolor/64x64"), archives.D("share/icons/hicolor/64x64/apps")}
	files := linFiles(app, v)
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		if strings.HasPrefix(rel, "bin/") {
			es = append(es, archives.X(rel, files[rel]))
		} else {
			es = append(es, archives.F(rel, files[rel]))
		}
	}
	return archives.TarGz(under(app+"-"+v+"-linux-x86_64/", append(es, extra...))...)
}

func linFiles(app, v string) map[string]string {
	id := "ai.storyteller." + app
	return map[string]string{
		"bin/" + app:                                    app + " " + v,
		"bin/" + app + "-cli":                           "cli " + app + " " + v,
		"share/doc/" + app + "/README.md":               "readme " + app + " " + v + "\n",
		"share/doc/" + app + "/LICENSE-MIT":             "mit\n",
		"share/applications/" + id + ".desktop":         "[Desktop Entry]\nName=" + app + "\nExec=" + app + " %F\n",
		"share/icons/hicolor/64x64/apps/" + id + ".png": "\x89PNG-fake " + v,
	}
}

// appDir is where app is installed: '{root}\{Name}' (the template as written, so on a Linux host
// one folder whose name holds a backslash) or the shared ~/.local.
func (r *rig) appDir(app string) string {
	if r.platform.OS == "windows" {
		return r.root + `\` + strings.ToUpper(app[:1]) + app[1:]
	}
	return filepath.Join(r.deps().home, ".local")
}

// appFiles is what app v puts in its folder, by path relative to it.
func (r *rig) appFiles(app, v string) map[string]string {
	if r.platform.OS == "windows" {
		return winFiles(app, v)
	}
	return linFiles(app, v)
}

// files lists every file under dir, "/"-separated, with its content.
func files(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p) //nolint:gosec // test
		out[filepath.ToSlash(rel)] = string(b)
		return err
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return out
}

// holds checks that app v's files are all in its folder, exactly.
func (r *rig) holds(t *testing.T, app, v string) {
	t.Helper()
	got := files(t, r.appDir(app))
	for rel, want := range r.appFiles(app, v) {
		if got[rel] != want {
			t.Errorf("%s %s: %s = %q, want %q", app, v, rel, got[rel], want)
		}
	}
}

// lacks checks that none of app's files is left in its folder.
func (r *rig) lacks(t *testing.T, app string) {
	t.Helper()
	got := files(t, r.appDir(app))
	for rel := range r.appFiles(app, "x") {
		if _, ok := got[rel]; ok {
			t.Errorf("%s: %s is still there", app, rel)
		}
	}
}

func (r *rig) version(t *testing.T, app string) string {
	t.Helper()
	st, err := state.Load(filepath.Join(r.stateDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if p := st.Packages[app]; p != nil {
		return p.Version
	}
	return ""
}

// must runs a command that has to succeed.
func (r *rig) must(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := r.run(args...)
	if code != exitcode.OK {
		t.Fatalf("%q: exit %d\n%s%s", args, code, out, errOut)
	}
	return out
}

func writeFiles(t *testing.T, dir string, m map[string]string) {
	t.Helper()
	for rel, c := range m {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func forEachLayout(t *testing.T, f func(t *testing.T, r *rig)) {
	for _, name := range []string{"windows", "linux"} {
		t.Run(name, func(t *testing.T) { f(t, craftRig(t, layouts[name])) })
	}
}

// C1: a run without --apply only reports (CraftWerk's dry run). Here check and plan change no
// file, and apply asks first: without --yes and no terminal to ask on, it refuses (exit 4).
func TestC01NothingChangesWithoutConsent(t *testing.T) {
	forEachLayout(t, func(t *testing.T, r *rig) {
		r.must(t, "install", "photocraft", "--yes")
		r.publishCraft("photocraft", "2.0.0", 20, craftRelease{})
		before := files(t, r.appDir("photocraft"))
		if code, _, _ := r.run("check", "photocraft"); code != exitcode.UpdatesAvailable {
			t.Errorf("check: exit %d, want 2", code)
		}
		r.run("plan")
		if code, _, _ := r.run("apply"); code != exitcode.Usage {
			t.Errorf("apply without --yes, no terminal: exit %d, want 4", code)
		}
		if got := files(t, r.appDir("photocraft")); !reflect.DeepEqual(got, before) || r.version(t, "photocraft") != "1.0.0" {
			t.Errorf("something changed: %s", r.version(t, "photocraft"))
		}
		if r.version(t, "wordcraft") != "" {
			t.Error("apply installed wordcraft without consent")
		}
	})
}

// C2: the installed version is read from the .exe's file properties (Windows). Here adopt reads
// version_from_exe; a four-part ProductVersion ends in .0.
func TestC02VersionFromTheExe(t *testing.T) {
	r := craftRig(t, winLayout)
	dir := r.appDir("photocraft")
	writeFiles(t, dir, winFiles("photocraft", "1.0.0"))
	if code, out, errOut := r.run("adopt", "photocraft"); code == exitcode.OK || !strings.Contains(out+errOut, "--version") {
		t.Fatalf("adopt with no version resource: exit %d\n%s%s", code, out, errOut)
	}
	r.exeVersions = map[string]string{filepath.Join(dir, "photocraft.exe"): "1.0.0.0"}
	r.must(t, "adopt", "photocraft")
	if v := r.version(t, "photocraft"); v != "1.0.0" {
		t.Errorf("adopted version = %q", v)
	}
}

// C3: the latest release comes from GitHub's API, or from the release page when the API is
// unavailable. Here the web path comes first: a plain update asks the API nothing at all, so a
// rate-limited API does not matter.
func TestC03UpdatesWithoutTheAPI(t *testing.T) {
	forEachLayout(t, func(t *testing.T, r *rig) {
		r.must(t, "install", "photocraft", "--yes")
		r.publishCraft("photocraft", "2.0.0", 20, craftRelease{})
		r.srv.SetRateLimited(true)
		r.srv.ResetRequests()
		r.must(t, "update", "--yes")
		r.holds(t, "photocraft", "2.0.0")
		if n := r.srv.RequestCount("/api/"); n != 0 {
			t.Errorf("%d API requests", n)
		}
	})
}

// C4: the download is checked against SHA256SUMS.txt; a file not listed, or listed with another
// hash, is refused (exit 5, nothing installed); "<hash> *<name>" lines count.
func TestC04Checksums(t *testing.T) {
	r := craftRig(t, winLayout)
	r.must(t, "install", "photocraft", "--yes")
	r.publishCraft("photocraft", "2.0.0", 25, craftRelease{sums: func(n, h string) string {
		if strings.Contains(n, "windows") {
			return ""
		}
		return h + "  " + n + "\n"
	}})
	r.publishCraft("photocraft", "3.0.0", 20, craftRelease{sums: func(n, _ string) string {
		return strings.Repeat("ab", 32) + "  " + n + "\n"
	}})
	r.publishCraft("photocraft", "4.0.0", 15, craftRelease{sums: func(n, h string) string { return h + " *" + n + "\n" }})

	for _, v := range []string{"2.0.0", "3.0.0"} {
		if code, out, errOut := r.run("update", "photocraft", "--version", v, "--yes"); code != exitcode.VerifyFailed {
			t.Errorf("update to %s: exit %d, want 5\n%s%s", v, code, out, errOut)
		}
		r.holds(t, "photocraft", "1.0.0")
	}
	if _, err := os.Stat(filepath.Join(r.stateDir(), "quarantine", "photocraft")); err != nil {
		t.Errorf("the mismatched download was not kept: %v", err)
	}
	r.must(t, "update", "photocraft", "--version", "4.0.0", "--yes")
	r.holds(t, "photocraft", "4.0.0")
}

// C5: unpacked into a scratch folder first, the archive's single top-level folder stepped into.
// Here a release that is not an archive (though its hash checks out) changes nothing.
func TestC05StagedAndUnwrapped(t *testing.T) {
	forEachLayout(t, func(t *testing.T, r *rig) {
		r.must(t, "install", "photocraft", "--yes")
		r.holds(t, "photocraft", "1.0.0")
		for rel := range files(t, r.appDir("photocraft")) {
			if strings.Contains(rel, "photocraft-1.0.0-") {
				t.Errorf("the archive's top-level folder was kept: %s", rel)
			}
		}
		before := files(t, r.appDir("photocraft"))
		r.publishCraft("photocraft", "2.0.0", 20, craftRelease{damage: true})
		if code, _, _ := r.run("update", "--yes"); code == exitcode.OK {
			t.Error("a damaged release installed")
		}
		if got := files(t, r.appDir("photocraft")); !reflect.DeepEqual(got, before) || r.version(t, "photocraft") != "1.0.0" {
			t.Error("a damaged release changed the install")
		}
	})
}

// userData is what the user keeps in an app's folder (CraftWerk: "your data isn't in the zip").
var userData = map[string]string{"PhotoCraftData/library.db": "my photos", "settings.ini": "theme=dark"}

// C6, C7, C11: an update saves only what it replaces, one version back; the user's files are
// never touched; a rollback swaps, so rolling back again goes forward.
func TestC06C07C11BackupUserDataAndRollback(t *testing.T) {
	forEachLayout(t, func(t *testing.T, r *rig) {
		r.must(t, "install", "photocraft", "--yes")
		writeFiles(t, r.appDir("photocraft"), userData)
		for _, v := range []string{"2.0.0", "3.0.0"} {
			r.publishCraft("photocraft", v, 20, craftRelease{})
			r.must(t, "update", "photocraft", "--yes")
		}
		r.holds(t, "photocraft", "3.0.0")
		// One version back: the rollback record is 2.0.0's, not 1.0.0's.
		r.must(t, "rollback", "photocraft", "--yes")
		r.holds(t, "photocraft", "2.0.0")
		r.must(t, "rollback", "photocraft", "--yes")
		r.holds(t, "photocraft", "3.0.0")
		if v := r.version(t, "photocraft"); v != "3.0.0" {
			t.Errorf("version after two rollbacks = %s", v)
		}
		got := files(t, r.appDir("photocraft"))
		for rel, c := range userData {
			if got[rel] != c {
				t.Errorf("user file %s = %q", rel, got[rel])
			}
		}
		// Remove takes the app's files and leaves the user's.
		r.must(t, "remove", "photocraft", "--yes")
		r.lacks(t, "photocraft")
		got = files(t, r.appDir("photocraft"))
		for rel, c := range userData {
			if got[rel] != c {
				t.Errorf("after remove, user file %s = %q", rel, got[rel])
			}
		}
	})
}

// C8: a failure half way through the copy puts back what was saved. Here a file that cannot be
// replaced (Windows: read-only; Linux: in a folder that cannot be written) stops the update half
// way: the old version is back exactly, and nothing needs repair. The engine's failure and crash
// sweeps are the thorough proof; this is the same through the command line.
func TestC08FailureHalfWayRestores(t *testing.T) {
	layout, blocked := winLayout, "photocraft.exe"
	if runtime.GOOS != "windows" {
		layout, blocked = linLayout, "share/doc/photocraft"
	}
	r := craftRig(t, layout)
	r.must(t, "install", "photocraft", "--yes")
	before := files(t, r.appDir("photocraft"))
	p := filepath.Join(r.appDir("photocraft"), filepath.FromSlash(blocked))
	mode := os.FileMode(0o444)
	if runtime.GOOS != "windows" {
		mode = 0o555
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o755) })
	r.publishCraft("photocraft", "2.0.0", 20, craftRelease{})
	if code, _, _ := r.run("update", "--yes"); code == exitcode.OK {
		t.Fatal("the update went through")
	}
	if got := files(t, r.appDir("photocraft")); !reflect.DeepEqual(got, before) || r.version(t, "photocraft") != "1.0.0" {
		t.Errorf("not the old version exactly: %s", r.version(t, "photocraft"))
	}
	if code, out, _ := r.run("plan", "photocraft"); code == exitcode.NeedsRepair || strings.Contains(out, "repair") {
		t.Errorf("needs repair after a clean failure: exit %d\n%s", code, out)
	}
}

// C9: a running app is skipped, matched by the folder it runs from, not its name. In the shared
// ~/.local only the app's own files count, so the other apps still update.
func TestC09RunningAppIsSkipped(t *testing.T) {
	forEachLayout(t, func(t *testing.T, r *rig) {
		r.must(t, "install", "photocraft", "--yes")
		r.must(t, "install", "wordcraft", "--yes")
		r.publishCraft("photocraft", "2.0.0", 20, craftRelease{})
		r.publishCraft("wordcraft", "2.0.0", 20, craftRelease{})
		exe := "photocraft.exe"
		if r.platform.OS == "linux" {
			exe = "bin/photocraft"
		}
		// The same app running from a copy somewhere else does not count.
		r.running = []proc.Process{{PID: 41, Path: filepath.Join(r.root+"-copy", "Photocraft", "photocraft.exe")}}
		r.must(t, "update", "photocraft", "--yes")
		r.holds(t, "photocraft", "2.0.0")

		r.publishCraft("photocraft", "3.0.0", 10, craftRelease{})
		r.running = []proc.Process{{PID: 42, Path: filepath.Join(r.appDir("photocraft"), filepath.FromSlash(exe))}}
		code, out, errOut := r.run("update", "--yes")
		if code != exitcode.OK || !strings.Contains(out, "running") {
			t.Errorf("update with photocraft running: exit %d\n%s%s", code, out, errOut)
		}
		r.holds(t, "photocraft", "2.0.0")
		r.holds(t, "wordcraft", "2.0.0")
	})
}

// C10: reinstalling the version already there keeps the backup of the version before it.
func TestC10ForcedReinstallKeepsTheOlderBackup(t *testing.T) {
	forEachLayout(t, func(t *testing.T, r *rig) {
		r.must(t, "install", "photocraft", "--yes")
		r.publishCraft("photocraft", "2.0.0", 20, craftRelease{})
		r.must(t, "update", "--yes")
		r.must(t, "update", "photocraft", "--force", "--yes")
		r.holds(t, "photocraft", "2.0.0")
		r.must(t, "rollback", "photocraft", "--yes")
		r.holds(t, "photocraft", "1.0.0")
	})
}

// C12: installing an app that is not there yet, or an exact version of it. A folder that holds
// something GremLoader did not put there is not installed over (Windows layout).
func TestC12Install(t *testing.T) {
	forEachLayout(t, func(t *testing.T, r *rig) {
		r.publishCraft("wordcraft", "2.0.0", 20, craftRelease{})
		r.must(t, "install", "photocraft", "--yes")
		r.holds(t, "photocraft", "1.0.0")
		r.must(t, "install", "wordcraft@1.0.0", "--yes")
		r.holds(t, "wordcraft", "1.0.0")
		if r.platform.OS == "windows" {
			writeFiles(t, r.appDir("deckcraft"), map[string]string{"notes.txt": "mine"})
			code, out, errOut := r.run("install", "deckcraft", "--yes")
			if code == exitcode.OK || !strings.Contains(out+errOut, "adopt deckcraft") {
				t.Errorf("install over someone else's folder: exit %d\n%s%s", code, out, errOut)
			}
		}
	})
}

// C13: uninstall. GremLoader always keeps the user's files (CraftWerk's -KeepData; see C6). An app
// adopted without a file list can only go with its whole folder (--delete-dir).
func TestC13RemoveAnAdoptedApp(t *testing.T) {
	r := craftRig(t, winLayout)
	dir := r.appDir("photocraft")
	writeFiles(t, dir, winFiles("photocraft", "1.0.0"))
	r.exeVersions = map[string]string{filepath.Join(dir, "photocraft.exe"): "1.0.0.0"}
	r.must(t, "adopt", "photocraft")
	if code, out, errOut := r.run("remove", "photocraft", "--yes"); code == exitcode.OK || !strings.Contains(out+errOut, "--delete-dir") {
		t.Fatalf("remove of an adopted app: exit %d\n%s%s", code, out, errOut)
	}
	r.must(t, "remove", "photocraft", "--delete-dir", "--yes")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the folder is still there: %v", err)
	}
}

// C14: verify compares what is installed with the official release (CraftWerk re-downloaded).
// Here locally against the file list kept at install, or --remote against the release.
func TestC14Verify(t *testing.T) {
	forEachLayout(t, func(t *testing.T, r *rig) {
		r.must(t, "install", "photocraft", "--yes")
		r.must(t, "verify")
		exe := "photocraft.exe"
		if r.platform.OS == "linux" {
			exe = "bin/photocraft"
		}
		writeFiles(t, r.appDir("photocraft"), map[string]string{exe: "tampered"})
		for _, args := range [][]string{{"verify"}, {"verify", "photocraft", "--remote"}} {
			code, out, errOut := r.run(args...)
			if code == exitcode.OK || !strings.Contains(out+errOut, filepath.Base(exe)) {
				t.Errorf("%q with a changed %s: exit %d\n%s%s", args, exe, code, out, errOut)
			}
		}
	})
}

// C15: only the apps named.
func TestC15OnlyTheAppsNamed(t *testing.T) {
	forEachLayout(t, func(t *testing.T, r *rig) {
		r.must(t, "apply", "--yes")
		for _, app := range craftApps {
			r.holds(t, app, "1.0.0")
		}
		for _, app := range []string{"photocraft", "wordcraft", "gridcraft"} {
			r.publishCraft(app, "2.0.0", 20, craftRelease{})
		}
		r.must(t, "update", "photocraft", "wordcraft", "--yes")
		r.holds(t, "photocraft", "2.0.0")
		r.holds(t, "wordcraft", "2.0.0")
		r.holds(t, "gridcraft", "1.0.0")
	})
}

// C16: on Linux every app shares ~/.local; each one's file list says what is its own, so remove
// and rollback touch exactly those files. A file one app owns is never taken by another.
func TestC16SharedLocalFolder(t *testing.T) {
	r := craftRig(t, linLayout)
	local := r.appDir("photocraft")
	mine := map[string]string{"bin/my-tool": "the user's", "share/applications/my.desktop": "the user's"}
	writeFiles(t, local, mine)
	r.must(t, "install", "photocraft", "--yes")
	r.must(t, "install", "wordcraft", "--yes")
	r.holds(t, "photocraft", "1.0.0")
	r.holds(t, "wordcraft", "1.0.0")

	r.publishCraft("wordcraft", "2.0.0", 20, craftRelease{})
	r.must(t, "update", "wordcraft", "--yes")
	r.holds(t, "wordcraft", "2.0.0")
	r.must(t, "rollback", "wordcraft", "--yes")
	r.holds(t, "wordcraft", "1.0.0")
	r.holds(t, "photocraft", "1.0.0")

	r.must(t, "remove", "photocraft", "--yes")
	r.lacks(t, "photocraft")
	r.holds(t, "wordcraft", "1.0.0")
	got := files(t, local)
	for rel, c := range mine {
		if got[rel] != c {
			t.Errorf("%s = %q after removing photocraft", rel, got[rel])
		}
	}
	if code, out, errOut := r.run("remove", "wordcraft", "--delete-dir", "--yes"); code == exitcode.OK || !strings.Contains(out+errOut, "shared") {
		t.Errorf("remove --delete-dir in ~/.local: exit %d\n%s%s", code, out, errOut)
	}

	r.publishCraft("gridcraft", "2.0.0", 20, craftRelease{extra: []archives.Entry{archives.X("bin/wordcraft", "gridcraft's")}})
	code, out, errOut := r.run("install", "gridcraft", "--yes")
	if code == exitcode.OK || !strings.Contains(out+errOut, "bin/wordcraft") || !strings.Contains(out+errOut, "belongs to wordcraft") {
		t.Errorf("install over wordcraft's file: exit %d\n%s%s", code, out, errOut)
	}
	r.holds(t, "wordcraft", "1.0.0")
}

// C17 (Windows) and the Linux hooks: after an install, update or remove, file types and Start menu
// entries are registered again for that one app (register-apps.ps1 -Only <app>); on Linux the
// desktop caches are refreshed. Checked as the commands the run asks for.
func TestC17RegistrationAfterEveryChange(t *testing.T) {
	forEachLayout(t, func(t *testing.T, r *rig) {
		r.must(t, "install", "photocraft", "--yes")
		r.publishCraft("photocraft", "2.0.0", 20, craftRelease{})
		r.must(t, "update", "--yes")
		r.must(t, "remove", "photocraft", "--yes")
		var events []string
		for _, h := range r.hooks.ran {
			events = append(events, h.Event+" "+h.Action+" "+h.Version)
			if h.Package != "photocraft" || h.Dir != r.appDir("photocraft") || len(h.Commands) != 1 {
				t.Errorf("hook %+v", h)
				continue
			}
			argv := h.Commands[0]
			if r.platform.OS == "windows" {
				want := []string{"powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", r.root + `\_update\register-apps.ps1`, "-Only", "photocraft"}
				if !reflect.DeepEqual(argv, want) {
					t.Errorf("%s argv = %q", h.Event, argv)
				}
			} else if argv[0] != "sh" || argv[1] != "-c" || !strings.Contains(argv[2], "update-desktop-database") {
				t.Errorf("%s argv = %q", h.Event, argv)
			}
		}
		want := []string{pkgfile.PostInstall + " install 1.0.0", pkgfile.PostUpdate + " update 2.0.0", pkgfile.PostRemove + " remove 2.0.0"}
		if !reflect.DeepEqual(events, want) {
			t.Errorf("hooks = %q, want %q", events, want)
		}
	})
}

// linuxHookRig is a Linux-layout rig whose hooks really run, with stub programs first on PATH.
func linuxHookRig(t *testing.T, lddOutput string, localBinOnPath bool) (r *rig, calls string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("runs the Craft file's sh hook: Linux only")
	}
	r = craftRig(t, linLayout)
	r.hooks = nil
	stubs := t.TempDir()
	calls = filepath.Join(stubs, "calls")
	for name, script := range map[string]string{
		"ldd":                     "#!/bin/sh\nprintf '" + lddOutput + "'\n",
		"update-desktop-database": "#!/bin/sh\necho \"update-desktop-database $*\" >> '" + calls + "'\n",
	} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(script), 0o700); err != nil { //nolint:gosec // a test stub must be executable
			t.Fatal(err)
		}
	}
	path := stubs + string(os.PathListSeparator) + os.Getenv("PATH")
	if localBinOnPath {
		path = filepath.Join(r.appDir("photocraft"), "bin") + string(os.PathListSeparator) + path
	}
	t.Setenv("PATH", path)
	return r, calls
}

// C18: after an install, the app's missing system libraries are named, with what to do. The
// install itself stands (the hook's failure is a warning).
func TestC18MissingLibrariesAreNamed(t *testing.T) {
	r, _ := linuxHookRig(t, `\tlibfoo.so.1 => not found\n\tlibc.so.6 => /lib/libc.so.6\n`, false)
	code, out, errOut := r.run("install", "photocraft", "--yes")
	if code != exitcode.OK || !strings.Contains(out+errOut, "libfoo.so.1") || strings.Contains(out+errOut, "libc.so.6") ||
		!strings.Contains(out+errOut, "is not on PATH") {
		t.Errorf("install: exit %d\n%s%s", code, out, errOut)
	}
	r.holds(t, "photocraft", "1.0.0")
}

// C19: the desktop database is refreshed so the new menu entries and file types show; the shipped
// .desktop files stay as released (verify would report them changed otherwise).
func TestC19DesktopRefreshLeavesTheMenuEntriesAsShipped(t *testing.T) {
	r, calls := linuxHookRig(t, `\tlibc.so.6 => /lib/libc.so.6\n`, true)
	code, out, errOut := r.run("install", "photocraft", "--yes")
	if code != exitcode.OK || strings.Contains(out+errOut, "hook failed") {
		t.Errorf("install: exit %d\n%s%s", code, out, errOut)
	}
	b, err := os.ReadFile(calls) //nolint:gosec // test
	if want := filepath.Join(r.appDir("photocraft"), "share", "applications"); err != nil || !strings.Contains(string(b), want) {
		t.Errorf("update-desktop-database calls: %q, %v", b, err)
	}
	r.holds(t, "photocraft", "1.0.0") // the .desktop file byte for byte
	r.must(t, "verify")
}
