package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/lock"
	"github.com/enls/gremloader/internal/platform"
	"github.com/enls/gremloader/internal/testutil/archives"
	"github.com/enls/gremloader/internal/testutil/fakerelease"
)

// twoPlatforms returns this machine's platform key and another one.
func twoPlatforms() (here, other string) {
	here, other = platform.Current().String(), "linux/arm64"
	if here == other {
		other = "windows/arm64"
	}
	return here, other
}

// multiFile is a package with one asset per platform, verified by one checksum file (or with
// sidecar = true, a ".sha256" file per asset).
func multiFile(sidecar bool) string {
	here, other := twoPlatforms()
	checksums := "SHA256SUMS.txt"
	if sidecar {
		checksums = "{asset}.sha256"
	}
	return `schema_version = 1

[package.app]
repo      = "o/app"
asset     = { "` + here + `" = "app-{version}-here.zip", "` + other + `" = "app-{version}-other.zip" }
checksums = "` + checksums + `"
dir       = "{root}/App"
`
}

// publishMulti publishes v with both platforms' zips and their checksums; sums overrides the
// content of the shared checksum file when not "".
func (r *rig) publishMulti(v string, sidecar bool, sums string) (here, other []byte) {
	r.t.Helper()
	here = archives.Zip(archives.X("app-"+v+"/app.exe", "app "+v+" here"))
	other = archives.Zip(archives.X("app-"+v+"/app.exe", "app "+v+" other"))
	hn, on := "app-"+v+"-here.zip", "app-"+v+"-other.zip"
	rel := fakerelease.Release{Tag: "v" + v, PublishedAt: r.clock.Now().AddDate(0, 0, -10),
		Assets: []fakerelease.Asset{{Name: hn, Data: here}, {Name: on, Data: other}}}
	if sidecar {
		rel.Assets = append(rel.Assets,
			fakerelease.Asset{Name: hn + ".sha256", Data: []byte(sha(here) + "  " + hn + "\n")},
			fakerelease.Asset{Name: on + ".sha256", Data: []byte(sha(other) + "  " + on + "\n")})
	} else {
		if sums == "" {
			sums = sha(here) + "  " + hn + "\n" + sha(other) + "  " + on + "\n"
		}
		rel.Assets = append(rel.Assets, fakerelease.Asset{Name: "SHA256SUMS.txt", Data: []byte(sums)})
	}
	r.srv.AddRelease("o", "app", rel)
	return here, other
}

func lockOf(t *testing.T, e *Engine, o LockOptions) (map[string]*lock.Package, *Item) {
	t.Helper()
	entries, items, err := e.Lock(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	return entries, items[0]
}

func TestLockEveryPlatformFromOneChecksumFile(t *testing.T) {
	r := newRig(t, multiFile(false))
	r.publishMulti("1.0.0", false, "")
	here, other := r.publishMulti("2.0.0", false, "")
	hk, ok := twoPlatforms()

	r.srv.ResetRequests()
	entries, it := lockOf(t, r.engine(), LockOptions{})
	if it.Result != ResultLocked || it.Err != nil {
		t.Fatalf("item = %+v (%v)", it, it.Err)
	}
	lp := entries["app"]
	if lp == nil || lp.Version != "2.0.0" || lp.Tag != "v2.0.0" || lp.Repo != "o/app" || lp.PublishedAt == nil || len(lp.Platforms) != 2 {
		t.Fatalf("entry = %+v", lp)
	}
	for key, data := range map[string][]byte{hk: here, ok: other} {
		a := lp.Platforms[key]
		if a == nil || a.SHA256 != sha(data) || a.Size != int64(len(data)) || a.VerifiedBy != "" ||
			!strings.HasSuffix(a.URL, "/releases/download/v2.0.0/"+a.Asset) {
			t.Errorf("%s = %+v", key, a)
		}
	}
	sumsFetched := 0
	for _, rq := range r.srv.Requests() {
		if strings.HasSuffix(rq.Path, "/SHA256SUMS.txt") {
			sumsFetched++
		}
	}
	if sumsFetched != 1 {
		t.Errorf("the checksum file was downloaded %d times, want once", sumsFetched)
	}
	if _, err := os.Stat(r.paths.Staging("app")); err == nil {
		t.Error("staging left behind")
	}

	// Written twice from two runs: the same bytes (the lock must not churn in version control).
	dir := t.TempDir()
	write := func(name string) []byte {
		entries, _ := lockOf(t, r.engine(), LockOptions{})
		f := lock.New("gremloader test", r.clock.Now())
		f.Packages = entries
		p := filepath.Join(dir, name)
		if err := lock.Save(p, f); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p)
		return b
	}
	if a, b := write("a"), write("b"); !bytes.Equal(a, b) {
		t.Error("two locks of the same releases differ")
	}

	// What was locked installs with --locked.
	lf, _, err := lock.Load(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if it := item(t, r.apply(r.engine(), Request{InstallMissing: true, Locked: lf}), "app"); it.Result != ResultInstalled || it.To != "2.0.0" {
		t.Errorf("locked install = %+v (%v)", it, it.Err)
	}
}

func TestLockWithASidecarPerPlatform(t *testing.T) {
	r := newRig(t, multiFile(true))
	here, other := r.publishMulti("1.0.0", true, "")
	entries, it := lockOf(t, r.engine(), LockOptions{})
	if it.Result != ResultLocked {
		t.Fatalf("item = %+v (%v)", it, it.Err)
	}
	hk, ok := twoPlatforms()
	if entries["app"].Platforms[hk].SHA256 != sha(here) || entries["app"].Platforms[ok].SHA256 != sha(other) {
		t.Errorf("entry = %+v", entries["app"].Platforms)
	}
}

func TestLockRefusesWhatItCannotTrust(t *testing.T) {
	t.Run("checksum file and digest disagree", func(t *testing.T) {
		r := newRig(t, multiFile(false))
		r.publishMulti("1.0.0", false, strings.Repeat("ab", 32)+"  app-1.0.0-here.zip\n"+strings.Repeat("cd", 32)+"  app-1.0.0-other.zip\n")
		entries, it := lockOf(t, r.engine(), LockOptions{})
		var ve *VerifyError
		if it.Result != ResultFailed || !errors.As(it.Err, &ve) || !strings.Contains(it.Err.Error(), "disagree") || entries["app"] != nil {
			t.Errorf("item = %+v (%v), entries %+v", it, it.Err, entries)
		}
	})
	t.Run("a platform's asset is missing", func(t *testing.T) {
		r := newRig(t, multiFile(false))
		zip := archives.Zip(archives.X("app-1.0.0/app.exe", "x"))
		r.srv.AddRelease("o", "app", fakeRelease("v1.0.0", "app-1.0.0-here.zip", zip, "", r.clock.Now().AddDate(0, 0, -10)))
		_, other := twoPlatforms()
		entries, it := lockOf(t, r.engine(), LockOptions{})
		if it.Result != ResultFailed || !strings.Contains(it.Err.Error(), other) || entries["app"] != nil {
			t.Errorf("item = %+v (%v)", it, it.Err)
		}
	})
	t.Run("unknown package", func(t *testing.T) {
		r := newRig(t, multiFile(false))
		if _, _, err := r.engine().Lock(context.Background(), LockOptions{Packages: []string{"nope"}}); err == nil {
			t.Error("accepted")
		}
	})
}

// The lock is shared: the installed version does not matter, an explicit one does.
func TestLockIgnoresWhatIsInstalledAndHonoursWant(t *testing.T) {
	r := atV1(t)
	if entries, it := lockOf(t, r.engine(), LockOptions{}); it.Result != ResultLocked || entries["app"].Version != "2.0.0" {
		t.Errorf("lock = %+v, %+v (%v)", entries["app"], it, it.Err)
	}
	entries, it := lockOf(t, r.engine(), LockOptions{Want: map[string]string{"app": "1.0.0"}})
	if it.Result != ResultLocked || entries["app"].Version != "1.0.0" {
		t.Errorf("lock of 1.0.0 = %+v, %+v (%v)", entries["app"], it, it.Err)
	}
	if v := r.state().Packages["app"].Version; v != "1.0.0" {
		t.Errorf("locking changed the install: %s", v)
	}
}

// An install that --accept-sha256 let through tells the caller what the lock should record.
func TestAcceptedInstallCarriesItsOverride(t *testing.T) {
	r := atV1(t)
	received := r.publishBadSums("3.0.0")
	p, err := r.applyWith(r.engine(), Request{Packages: []string{"app"}},
		ApplyOptions{Accept: &Acceptance{Package: "app", Version: "3.0.0", SHA256: received}})
	if err != nil {
		t.Fatal(err)
	}
	it := item(t, p, "app")
	if it.Result != ResultUpdated || it.Override == nil || it.Override.SHA256 != received || it.Override.Asset != "app-3.0.0.zip" ||
		it.Override.Size <= 0 || it.Override.URL == "" {
		t.Fatalf("item = %+v, override %+v", it, it.Override)
	}

	// An ordinary install carries none.
	r2 := newRig(t, appFile)
	r2.publish("app", "1.0.0", 30, v1Files...)
	if it := item(t, r2.apply(r2.engine(), Request{InstallMissing: true}), "app"); it.Override != nil {
		t.Errorf("override = %+v", it.Override)
	}
}
