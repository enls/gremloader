package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/download"
	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/lock"
	"github.com/enls/gremloader/internal/platform"
	"github.com/enls/gremloader/internal/testutil/archives"
	"github.com/enls/gremloader/internal/testutil/fakerelease"
)

// damaging passes downloads through and damages the first n whose URL contains match, as a bad
// proxy or a flaky connection would: the bytes change but the transfer "succeeds".
type damaging struct {
	Fetcher
	match  string
	n      int
	damage func(path string) error
}

func (d *damaging) Fetch(ctx context.Context, url, dst string, o download.FetchOptions) (download.Result, error) {
	r, err := d.Fetcher.Fetch(ctx, url, dst, o)
	if err != nil || d.n == 0 || !strings.Contains(url, d.match) {
		return r, err
	}
	d.n--
	if err := d.damage(dst); err != nil {
		return r, err
	}
	sum, size, err := hashFile(dst)
	return download.Result{SHA256: sum, Size: size}, err
}

func flipByte(p string) error {
	b, err := os.ReadFile(p) //nolint:gosec // test
	if err != nil {
		return err
	}
	b[len(b)/2] ^= 0xff
	return os.WriteFile(p, b, 0o600)
}

func truncateHalf(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	return os.Truncate(p, fi.Size()/2)
}

func (r *rig) damageFirst(match string, n int, how func(string) error) {
	r.wrap = func(f Fetcher) Fetcher { return &damaging{Fetcher: f, match: match, n: n, damage: how} }
}

// publishBadSums adds v3.0.0 whose checksum file names a hash that is not the asset's (a
// publisher who did not regenerate it, or tampering). It returns the asset's real hash.
func (r *rig) publishBadSums(v string) string {
	r.t.Helper()
	zip := archives.Zip(archives.X("app-"+v+"/app.exe", "app "+v))
	r.srv.AddRelease("o", "app", fakeRelease("v"+v, "app-"+v+".zip", zip, strings.Repeat("ab", 32)+"  app-"+v+".zip\n", r.clock.Now()))
	return sha(zip)
}

func (r *rig) quarantineInfo() (quarantineInfo, bool) {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.paths.State, "quarantine", "app", "info.json"))
	if err != nil {
		return quarantineInfo{}, false
	}
	var qi quarantineInfo
	if err := json.Unmarshal(b, &qi); err != nil {
		r.t.Fatal(err)
	}
	return qi, true
}

func (r *rig) applyWith(e *Engine, rq Request, o ApplyOptions) (*Plan, error) {
	r.t.Helper()
	p := r.plan(e, rq)
	return p, e.Apply(context.Background(), p, o)
}

// The common case: the bytes were damaged in transit once. The second download is fine and
// installs.
func TestTransientCorruptionIsRetriedOnce(t *testing.T) {
	r := atV1(t)
	r.damageFirst("app-2.0.0.zip", 1, flipByte)
	r.srv.ResetRequests()
	it := item(t, r.apply(r.engine(), Request{}), "app")
	if it.Result != ResultUpdated {
		t.Fatalf("item = %+v", it)
	}
	assertTree(t, r.dir(), v2Tree)
	if n := r.srv.RequestCount("app-2.0.0.zip"); n != 2 {
		t.Errorf("%d downloads, want 2", n)
	}
	if _, ok := r.quarantineInfo(); ok {
		t.Error("a download that verified on the second try was quarantined")
	}
	retried := false
	for _, ev := range r.events() {
		retried = retried || ev.Event == "download.retry"
	}
	if !retried {
		t.Error("no download.retry event")
	}
}

// A mismatch that repeats is final: nothing installed, exit 5, the bytes kept for inspection, and
// the message says what to do.
func TestLastingMismatchIsQuarantined(t *testing.T) {
	r := atV1(t)
	received := r.publishBadSums("3.0.0")
	r.srv.ResetRequests()
	p := r.apply(r.engine(), Request{})
	it := item(t, p, "app")
	var ve *VerifyError
	if it.Result != ResultFailed || !errors.As(it.Err, &ve) || p.ExitCode(false) != exitcode.VerifyFailed {
		t.Fatalf("item = %+v", it)
	}
	if n := r.srv.RequestCount("app-3.0.0.zip"); n != 2 {
		t.Errorf("%d downloads, want 2 (one retry)", n)
	}
	for _, want := range []string{"expected " + strings.Repeat("ab", 32), "received " + received, "--accept-sha256 " + received, "quarantine"} {
		if !strings.Contains(ve.Msg, want) {
			t.Errorf("message lacks %q:\n%s", want, ve.Msg)
		}
	}
	qi, ok := r.quarantineInfo()
	if !ok || !qi.Kept || qi.Received != received || qi.Version != "3.0.0" || qi.Asset != "app-3.0.0.zip" {
		t.Errorf("quarantine = %+v", qi)
	}
	if b, err := os.ReadFile(filepath.Join(r.paths.State, "quarantine", "app", "download")); err != nil || sha(b) != received {
		t.Errorf("kept download: %v", err)
	}
	assertTree(t, r.dir(), v1Tree)
	r.assertClean()

	// The next failure replaces it.
	r.publishBadSums("4.0.0")
	r.apply(r.engine(), Request{})
	if qi, _ := r.quarantineInfo(); qi.Version != "4.0.0" {
		t.Errorf("quarantine after the next failure = %+v", qi)
	}
}

func TestQuarantineIsCapped(t *testing.T) {
	r := atV1(t)
	r.publishBadSums("3.0.0")
	quarantineCap = 10
	t.Cleanup(func() { quarantineCap = 512 << 20 })
	it := item(t, r.apply(r.engine(), Request{}), "app")
	qi, ok := r.quarantineInfo()
	if !ok || qi.Kept || !strings.Contains(it.Err.Error(), "too large to keep") {
		t.Errorf("quarantine = %+v, err = %v", qi, it.Err)
	}
	if _, err := os.Stat(filepath.Join(r.paths.State, "quarantine", "app", "download")); err == nil {
		t.Error("an oversized download was kept")
	}
}

// --accept-sha256: only the exact hash received, only for that package and version, logged.
func TestAcceptSHA256(t *testing.T) {
	one := Request{Packages: []string{"app"}, Want: "3.0.0"}
	t.Run("wrong hash refuses", func(t *testing.T) {
		r := atV1(t)
		r.publishBadSums("3.0.0")
		p, err := r.applyWith(r.engine(), one, ApplyOptions{Accept: &Acceptance{Package: "app", Version: "3.0.0", SHA256: strings.Repeat("cd", 32)}})
		if err != nil {
			t.Fatal(err)
		}
		if it := item(t, p, "app"); it.Result != ResultFailed {
			t.Errorf("item = %+v", it)
		}
		assertTree(t, r.dir(), v1Tree)
	})
	t.Run("the hash received installs and is logged", func(t *testing.T) {
		r := atV1(t)
		received := r.publishBadSums("3.0.0")
		r.srv.ResetRequests()
		p, err := r.applyWith(r.engine(), one, ApplyOptions{Accept: &Acceptance{Package: "app", Version: "3.0.0", SHA256: strings.ToUpper(received)}})
		if err != nil {
			t.Fatal(err)
		}
		if it := item(t, p, "app"); it.Result != ResultUpdated {
			t.Fatalf("item = %+v", it)
		}
		if n := r.srv.RequestCount("app-3.0.0.zip"); n != 1 {
			t.Errorf("%d downloads, want 1", n)
		}
		if vb := r.state().Packages["app"].VerifiedBy; len(vb) != 1 || vb[0] != "hash-override" {
			t.Errorf("verified_by = %v", vb)
		}
		logged := false
		for _, ev := range r.events() {
			logged = logged || (ev.Code == "hash-override" && strings.Contains(ev.Msg, received))
		}
		if !logged {
			t.Error("no hash-override event")
		}
	})
	t.Run("another version refuses", func(t *testing.T) {
		r := atV1(t)
		received := r.publishBadSums("3.0.0")
		_, err := r.applyWith(r.engine(), one, ApplyOptions{Accept: &Acceptance{Package: "app", Version: "2.0.0", SHA256: received}})
		if err == nil {
			t.Error("an acceptance for 2.0.0 was used for 3.0.0")
		}
	})
	t.Run("never over several packages", func(t *testing.T) {
		r := newRig(t, appFile+`
[package.b]
repo = "o/b"
asset = "app-{version}.zip"
checksums = "SHA256SUMS.txt"
dir = "{root}/B"
`)
		r.publish("app", "1.0.0", 10, v1Files...)
		r.publish("b", "1.0.0", 10, v1Files...)
		_, err := r.applyWith(r.engine(), Request{InstallMissing: true},
			ApplyOptions{Accept: &Acceptance{Package: "app", Version: "1.0.0", SHA256: strings.Repeat("ab", 32)}})
		if err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Errorf("err = %v", err)
		}
		if _, err := os.Stat(r.dir()); err == nil {
			t.Error("something was installed")
		}
	})
}

// --locked trusts the lock's hash; a mismatch against it is the same hard failure and needs the
// same deliberate override.
func TestLockedMismatch(t *testing.T) {
	r := atV1(t)
	lf := lock.New("test", r.clock.Now())
	lf.Packages["app"] = &lock.Package{Repo: "o/app", Tag: "v2.0.0", Version: "2.0.0",
		Platforms: map[string]*lock.Asset{platform.Current().String(): {
			Asset: "app-2.0.0.zip", URL: r.srv.AssetURL("o", "app", "v2.0.0", "app-2.0.0.zip"), SHA256: strings.Repeat("ef", 32),
		}}}
	p := r.apply(r.engine(), Request{Locked: lf})
	if it := item(t, p, "app"); it.Result != ResultFailed || p.ExitCode(false) != exitcode.VerifyFailed {
		t.Fatalf("item = %+v", it)
	}
	qi, _ := r.quarantineInfo()
	p, err := r.applyWith(r.engine(), Request{Packages: []string{"app"}, Locked: lf},
		ApplyOptions{Accept: &Acceptance{Package: "app", Version: "2.0.0", SHA256: qi.Received}})
	if err != nil {
		t.Fatal(err)
	}
	if it := item(t, p, "app"); it.Result != ResultUpdated {
		t.Errorf("with the override: %+v", it)
	}
	assertTree(t, r.dir(), v2Tree)
}

// A damaged archive that no hash vouched for may be a damaged download: fetched once more.
func TestDamagedUnverifiedArchiveIsFetchedAgain(t *testing.T) {
	r := newRig(t, `schema_version = 1
[package.app]
repo   = "o/app"
asset  = "app-{version}.zip"
dir    = "{root}/App"
verify = { require = [] }
`)
	r.publish("app", "1.0.0", 10, v1Files...)
	r.damageFirst("app-1.0.0.zip", 1, truncateHalf)
	r.srv.ResetRequests()
	it := item(t, r.apply(r.engine(), Request{InstallMissing: true}), "app")
	if it.Result != ResultInstalled {
		t.Fatalf("item = %+v", it)
	}
	if n := r.srv.RequestCount("app-1.0.0.zip"); n != 2 {
		t.Errorf("%d downloads, want 2", n)
	}
}

// A damaged archive whose hash checked out is the publisher's: the same bytes would come again.
func TestDamagedVerifiedArchiveIsNotFetchedAgain(t *testing.T) {
	r := atV1(t)
	zip := archives.Zip(archives.F("app-3.0.0/app.exe", strings.Repeat("compress me ", 500)))
	zip[len(zip)/3] ^= 0xff
	r.srv.AddRelease("o", "app", fakeRelease("v3.0.0", "app-3.0.0.zip", zip, "", r.clock.Now()))
	r.srv.ResetRequests()
	it := item(t, r.apply(r.engine(), Request{}), "app")
	if it.Result != ResultFailed || !isCorrupt(it.Err) {
		t.Fatalf("item = %+v", it)
	}
	if n := r.srv.RequestCount("app-3.0.0.zip"); n != 1 {
		t.Errorf("%d downloads, want 1", n)
	}
	assertTree(t, r.dir(), v1Tree)
}

// A checksum file that does not list the asset is not a corrupt download: no retry, nothing
// quarantined, and --accept-sha256 (which answers a mismatch) does not get past it.
func TestUnlistedAssetIsNotAMismatch(t *testing.T) {
	r := atV1(t)
	zip := archives.Zip(archives.X("app-3.0.0/app.exe", "app 3"))
	r.srv.AddRelease("o", "app", fakeRelease("v3.0.0", "app-3.0.0.zip", zip, sha(zip)+"  something-else.zip\n", r.clock.Now()))
	r.srv.ResetRequests()
	p, err := r.applyWith(r.engine(), Request{Packages: []string{"app"}, Want: "3.0.0"},
		ApplyOptions{Accept: &Acceptance{Package: "app", Version: "3.0.0", SHA256: sha(zip)}})
	if err != nil {
		t.Fatal(err)
	}
	var ve *VerifyError
	if it := item(t, p, "app"); it.Result != ResultFailed || !errors.As(it.Err, &ve) || !strings.Contains(ve.Msg, "not listed") {
		t.Fatalf("item = %+v", it)
	}
	if n := r.srv.RequestCount("app-3.0.0.zip"); n != 1 {
		t.Errorf("%d downloads, want 1", n)
	}
	if _, ok := r.quarantineInfo(); ok {
		t.Error("quarantined")
	}
	assertTree(t, r.dir(), v1Tree)
}

// Through the API both the checksum file and GitHub's digest vouch for the asset.
func TestDigestIsCheckedAndRecorded(t *testing.T) {
	r := newRig(t, appFile+"api = \"api\"\n")
	r.publish("app", "1.0.0", 10, v1Files...)
	if it := item(t, r.apply(r.engine(), Request{InstallMissing: true}), "app"); it.Result != ResultInstalled {
		t.Fatalf("item = %+v", it)
	}
	if got := strings.Join(r.state().Packages["app"].VerifiedBy, ","); got != "checksum,digest" {
		t.Errorf("verified_by = %s", got)
	}
}

// A per-asset checksum file naming another file is used, with a warning (plan E.9).
func TestSidecarNamingAnotherFileWarns(t *testing.T) {
	r := newRig(t, strings.Replace(appFile, `"SHA256SUMS.txt"`, `"{asset}.sha256"`, 1))
	zip := archives.Zip(archives.X("app-1.0.0/app.exe", "app 1"))
	r.srv.AddRelease("o", "app", fakerelease.Release{Tag: "v1.0.0", PublishedAt: r.clock.Now(), Assets: []fakerelease.Asset{
		{Name: "app-1.0.0.zip", Data: zip},
		{Name: "app-1.0.0.zip.sha256", Data: []byte(sha(zip) + "  build/output.zip\n")},
	}})
	it := item(t, r.apply(r.engine(), Request{InstallMissing: true}), "app")
	if it.Result != ResultInstalled || len(it.Notes) == 0 || !strings.Contains(strings.Join(it.Notes, "\n"), "build/output.zip") {
		t.Errorf("item = %+v", it)
	}
}
