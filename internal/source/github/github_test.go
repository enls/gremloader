package github_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/download"
	"github.com/enls/gremloader/internal/source/github"
	"github.com/enls/gremloader/internal/testutil/fakerelease"
	"github.com/enls/gremloader/internal/testutil/fixtures"
)

func newClient(t *testing.T, token string, apiBase string) *download.Client {
	t.Helper()
	u, err := url.Parse(apiBase)
	if err != nil {
		t.Fatal(err)
	}
	return download.New(download.Config{
		Policy: download.Policy{
			Hosts:      []string{"127.0.0.1"},
			TokenHosts: []string{u.Host},
			AllowHTTP:  true,
		},
		Token:  token,
		Sleep:  func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		Jitter: func(time.Duration) time.Duration { return 0 },
	})
}

var (
	t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	// The data of the asset in every fake release: its hash is what GitHub's digest should say.
	appData = []byte("MZ pretend this is a portable app")
)

type env struct {
	srv *fakerelease.Server
	src *github.Source
	c   *download.Client
}

// newEnv starts a fake with o/r: v1.0.0 and v1.1.0 (stable) and v2.0.0-rc.1 (a prerelease,
// created last, so "newest" and "latest stable" differ).
func newEnv(t *testing.T, token string, opts ...fakerelease.Option) *env {
	t.Helper()
	srv := fakerelease.New(t, opts...)
	add := func(tag string, pre bool, days int) {
		srv.AddRelease("o", "r", fakerelease.Release{
			Tag:         tag,
			Prerelease:  pre,
			PublishedAt: t0.AddDate(0, 0, days),
			Assets: []fakerelease.Asset{
				{Name: "app-" + strings.TrimPrefix(tag, "v") + "-windows-x64.zip", Data: appData},
				{Name: "SHA256SUMS.txt", Data: []byte("checksums for " + tag + "\n")},
			},
		})
	}
	add("v1.0.0", false, 0)
	add("v1.1.0", false, 10)
	add("v2.0.0-rc.1", true, 20)
	c := newClient(t, token, srv.APIBase())
	return &env{srv: srv, c: c, src: github.New(c, github.Options{APIBase: srv.APIBase(), WebBase: srv.WebBase()})}
}

func (e *env) list(t *testing.T, q github.Query) *github.Listing {
	t.Helper()
	if q.Repo == "" {
		q.Repo = "o/r"
	}
	l, err := e.src.List(context.Background(), q)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return l
}

func tags(l *github.Listing) []string {
	var out []string
	for _, r := range l.Releases {
		out = append(out, r.Tag)
	}
	return out
}

func find(t *testing.T, l *github.Listing, tag string) github.Release {
	t.Helper()
	for _, r := range l.Releases {
		if r.Tag == tag {
			return r
		}
	}
	t.Fatalf("no release %s in %v", tag, tags(l))
	return github.Release{}
}

func TestAWeeklyUpdateOnTheWebPathMakesNoAPIRequests(t *testing.T) {
	e := newEnv(t, "")
	e.srv.SetQuota(60)

	l := e.list(t, github.Query{})
	if l.Via != github.ModeWeb || l.FellOver != "" {
		t.Fatalf("via %q, fell over %q; want the web path", l.Via, l.FellOver)
	}
	// "latest" is GitHub's own answer: the newest *stable* release, not the newest release.
	if l.Latest != "v1.1.0" {
		t.Errorf("Latest = %q, want v1.1.0", l.Latest)
	}
	rel := find(t, l, "v1.1.0")
	if !rel.Published.Equal(t0.AddDate(0, 0, 10)) {
		t.Errorf("Published = %v", rel.Published)
	}
	if rel.FlagKnown || rel.AssetsKnown {
		t.Error("the web path claimed to know the prerelease flag or the asset list")
	}
	if rel.Version.String() != "1.1.0" {
		t.Errorf("Version = %v", rel.Version)
	}

	// Pick the asset and download it and its checksum file, as an update would.
	asset, err := e.src.SelectAsset(rel, "app-1.1.0-windows-x64.zip")
	if err != nil {
		t.Fatal(err)
	}
	sums, err := e.src.SelectAsset(rel, "SHA256SUMS.txt")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	res, err := e.c.Fetch(context.Background(), asset.URL, filepath.Join(dir, "a"), download.FetchOptions{MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(appData)
	if res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("downloaded hash %s", res.SHA256)
	}
	if _, err := e.c.Fetch(context.Background(), sums.URL, filepath.Join(dir, "s"), download.FetchOptions{MaxBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}

	if n := e.srv.RequestCount("/api/"); n != 0 {
		t.Errorf("%d API requests were made; a plain update must make none", n)
	}
	if e.srv.QuotaRemaining() != 60 {
		t.Errorf("quota remaining = %d, want 60", e.srv.QuotaRemaining())
	}
}

func TestAPIPathReportsFlagsAssetsAndDigests(t *testing.T) {
	e := newEnv(t, "tok")
	l := e.list(t, github.Query{})
	if l.Via != github.ModeAPI {
		t.Fatalf("via %q, want api (a token is configured)", l.Via)
	}
	if l.Latest != "v1.1.0" {
		t.Errorf("Latest = %q", l.Latest)
	}
	rc := find(t, l, "v2.0.0-rc.1")
	if !rc.FlagKnown || !rc.Prerelease {
		t.Errorf("rc: FlagKnown %v Prerelease %v", rc.FlagKnown, rc.Prerelease)
	}
	rel := find(t, l, "v1.1.0")
	if !rel.AssetsKnown || len(rel.Assets) != 2 {
		t.Fatalf("assets = %+v", rel.Assets)
	}
	a, err := e.src.SelectAsset(rel, "app-*-windows-x64.zip")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(appData)
	if a.SHA256 != hex.EncodeToString(sum[:]) || a.Size != int64(len(appData)) {
		t.Errorf("asset = %+v", a)
	}
	if want := e.srv.AssetURL("o", "r", "v1.1.0", a.Name); a.URL != want {
		t.Errorf("URL = %q, want %q", a.URL, want)
	}
	reqs := e.srv.Requests()
	if len(reqs) != 1 || reqs[0].Auth != "Bearer tok" || !strings.Contains(reqs[0].Accept, "vnd.github") {
		t.Errorf("requests = %+v", reqs)
	}
}

func TestRateLimitedAPIFallsOverToWeb(t *testing.T) {
	e := newEnv(t, "tok")
	e.srv.SetRateLimited(true)
	l := e.list(t, github.Query{})
	if l.Via != github.ModeWeb || !strings.Contains(l.FellOver, "rate limit") {
		t.Errorf("via %q, fell over %q", l.Via, l.FellOver)
	}
	if l.Latest != "v1.1.0" {
		t.Errorf("Latest = %q", l.Latest)
	}
}

func TestBrokenWebFallsOverToAPI(t *testing.T) {
	e := newEnv(t, "")
	e.srv.Inject(fakerelease.Fault{PathContains: "/web/", Status: 500})
	l := e.list(t, github.Query{})
	if l.Via != github.ModeAPI || !strings.Contains(l.FellOver, "web path failed") {
		t.Errorf("via %q, fell over %q", l.Via, l.FellOver)
	}
}

func TestRateLimitedAndWebBrokenReportsBoth(t *testing.T) {
	e := newEnv(t, "tok")
	e.srv.SetRateLimited(true)
	e.srv.Inject(fakerelease.Fault{PathContains: "/web/", Status: 500})
	_, err := e.src.List(context.Background(), github.Query{Repo: "o/r"})
	var rl *download.RateLimitError
	var se *download.StatusError
	if !errors.As(err, &rl) || !errors.As(err, &se) {
		t.Fatalf("err = %v; want both the rate limit and the web failure to be reachable", err)
	}
}

func TestPrereleaseChannelDoesNotFallBackToWeb(t *testing.T) {
	e := newEnv(t, "")
	e.srv.SetRateLimited(true)
	_, err := e.src.List(context.Background(), github.Query{Repo: "o/r", NeedAPIData: true})
	var rl *download.RateLimitError
	if !errors.Is(err, github.ErrNeedsAPI) || !errors.As(err, &rl) {
		t.Fatalf("err = %v, want ErrNeedsAPI wrapping the rate limit", err)
	}
	if n := e.srv.RequestCount("/web/"); n != 0 {
		t.Errorf("%d web requests: the web path cannot answer this and must not be used", n)
	}
}

func TestExplicitAPIModeDoesNotFallBack(t *testing.T) {
	e := newEnv(t, "")
	e.srv.SetRateLimited(true)
	_, err := e.src.List(context.Background(), github.Query{Repo: "o/r", Mode: github.ModeAPI})
	var rl *download.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want the rate limit", err)
	}
	if n := e.srv.RequestCount("/web/"); n != 0 {
		t.Errorf("%d web requests", n)
	}
}

func TestExplicitWebModeNeverTouchesTheAPI(t *testing.T) {
	e := newEnv(t, "tok")
	l := e.list(t, github.Query{Mode: github.ModeWeb})
	if l.Via != github.ModeWeb || e.srv.RequestCount("/api/") != 0 {
		t.Errorf("via %q, %d API requests", l.Via, e.srv.RequestCount("/api/"))
	}
	_, err := e.src.List(context.Background(), github.Query{Repo: "o/r", Mode: github.ModeWeb, NeedAPIData: true})
	if !errors.Is(err, github.ErrNeedsAPI) {
		t.Errorf("web mode with prerelease needs: err = %v, want ErrNeedsAPI", err)
	}
}

func TestUnknownRepositoryIsNotFoundWithoutFallingOver(t *testing.T) {
	e := newEnv(t, "")
	_, err := e.src.List(context.Background(), github.Query{Repo: "nobody/nothing"})
	if !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if n := e.srv.RequestCount("/api/"); n != 0 {
		t.Errorf("%d API requests for a repository the website says does not exist", n)
	}
}

func TestOnlyDraftsAndPrereleasesOnTheWebPath(t *testing.T) {
	srv := fakerelease.New(t)
	srv.AddRelease("o", "r", fakerelease.Release{Tag: "v0.1.0-rc.1", Prerelease: true})
	c := newClient(t, "", srv.APIBase())
	src := github.New(c, github.Options{APIBase: srv.APIBase(), WebBase: srv.WebBase()})
	// GitHub's /releases/latest 404s when there is no stable release: the web path says not
	// found; with api=auto the API is not consulted for that, so the caller sees a clear answer.
	_, err := src.List(context.Background(), github.Query{Repo: "o/r"})
	if !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestAssetSelection(t *testing.T) {
	e := newEnv(t, "tok")
	api := find(t, e.list(t, github.Query{}), "v1.1.0")
	web := find(t, newEnv(t, "").list(t, github.Query{}), "v1.1.0")

	t.Run("exact name", func(t *testing.T) {
		a, err := e.src.SelectAsset(api, "SHA256SUMS.txt")
		if err != nil || a.Name != "SHA256SUMS.txt" {
			t.Fatalf("%+v, %v", a, err)
		}
	})
	t.Run("none lists what exists", func(t *testing.T) {
		_, err := e.src.SelectAsset(api, "nope.zip")
		if err == nil || !strings.Contains(err.Error(), "SHA256SUMS.txt") {
			t.Fatalf("err = %v, want a list of the release's assets", err)
		}
	})
	t.Run("two matches is an error naming both", func(t *testing.T) {
		_, err := e.src.SelectAsset(api, "*")
		if err == nil || !strings.Contains(err.Error(), "2 assets") ||
			!strings.Contains(err.Error(), "SHA256SUMS.txt") || !strings.Contains(err.Error(), "windows-x64.zip") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("web path trusts a plain name", func(t *testing.T) {
		a, err := e.src.SelectAsset(web, "app-1.1.0-windows-x64.zip")
		if err != nil || a.Size != -1 || a.SHA256 != "" || !strings.HasSuffix(a.URL, "/releases/download/v1.1.0/app-1.1.0-windows-x64.zip") {
			t.Fatalf("%+v, %v", a, err)
		}
	})
	t.Run("web path cannot expand a wildcard", func(t *testing.T) {
		_, err := e.src.SelectAsset(web, "app-*.zip")
		if !errors.Is(err, github.ErrNeedsAPI) {
			t.Fatalf("err = %v, want ErrNeedsAPI", err)
		}
	})
	t.Run("patterns that are not file names", func(t *testing.T) {
		for _, p := range []string{"", "a/b.zip", `..\x.zip`, "../x"} {
			if _, err := e.src.SelectAsset(api, p); err == nil {
				t.Errorf("pattern %q accepted", p)
			}
			if _, err := e.src.SelectAsset(web, p); err == nil {
				t.Errorf("web pattern %q accepted", p)
			}
		}
	})
	t.Run("bad pattern", func(t *testing.T) {
		if _, err := e.src.SelectAsset(api, "app-[.zip"); err == nil {
			t.Error("a malformed glob was accepted")
		}
	})
}

func TestTagPrefixSelectsOnePackageOfAMonorepo(t *testing.T) {
	srv := fakerelease.New(t)
	for i, tag := range []string{"app-v1.0.0", "app-v1.1.0", "lib-v9.9.9"} {
		srv.AddRelease("o", "mono", fakerelease.Release{Tag: tag, PublishedAt: t0.AddDate(0, 0, i)})
	}
	c := newClient(t, "", srv.APIBase())
	src := github.New(c, github.Options{APIBase: srv.APIBase(), WebBase: srv.WebBase()})

	web, err := src.List(context.Background(), github.Query{Repo: "o/mono", TagPrefix: "app-v", Mode: github.ModeWeb})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tags(web), ","); got != "app-v1.1.0,app-v1.0.0" {
		t.Errorf("web releases = %s", got)
	}
	// GitHub's own "latest" is lib-v9.9.9, which is not this package's tag: say so rather than
	// pretending.
	if web.Latest != "" {
		t.Errorf("web Latest = %q, want empty", web.Latest)
	}
	if len(web.Warnings) != 1 || !strings.Contains(web.Warnings[0], "lib-v9.9.9") {
		t.Errorf("warnings = %v", web.Warnings)
	}

	api, err := src.List(context.Background(), github.Query{Repo: "o/mono", TagPrefix: "app-v", Mode: github.ModeAPI})
	if err != nil {
		t.Fatal(err)
	}
	if api.Latest != "app-v1.1.0" {
		t.Errorf("api Latest = %q, want app-v1.1.0", api.Latest)
	}
}

// ---- hostile and real-world data, served by a hand-written server ----

type rawServer struct {
	ts   *httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

func newRaw(t *testing.T, routes map[string]http.HandlerFunc) *rawServer {
	t.Helper()
	r := &rawServer{hits: map[string]int{}}
	r.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hits[req.URL.Path]++
		r.mu.Unlock()
		if h, ok := routes[req.URL.Path]; ok {
			h(w, req)
			return
		}
		http.NotFound(w, req)
	}))
	t.Cleanup(r.ts.Close)
	return r
}

func (r *rawServer) count(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for p, c := range r.hits {
		if strings.HasPrefix(p, prefix) {
			n += c
		}
	}
	return n
}

func (r *rawServer) source(t *testing.T, token string) *github.Source {
	t.Helper()
	return github.New(newClient(t, token, r.ts.URL), github.Options{APIBase: r.ts.URL + "/api", WebBase: r.ts.URL + "/web"})
}

func body(s string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(s)) }
}

func redirect(to string, code int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(to, "/") {
			to = "http://" + r.Host + to
		}
		http.Redirect(w, r, to, code)
	}
}

func TestHostileAPIDataIsFilteredAndExplained(t *testing.T) {
	const list = `[
	 {"tag_name":"v1.0.0","draft":false,"prerelease":false,"published_at":"2026-01-01T00:00:00Z","assets":[
	    {"id":1,"name":"good.zip","state":"uploaded","size":3,"digest":"sha256:` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `"},
	    {"id":2,"name":"../evil.zip","state":"uploaded","size":3,"digest":""},
	    {"id":3,"name":"half.zip","state":"starter","size":3,"digest":""},
	    {"id":4,"name":"baddigest.zip","state":"uploaded","size":3,"digest":"md5:abc"},
	    {"id":5,"name":"back\\slash.zip","state":"uploaded","size":3,"digest":""}]},
	 {"tag_name":"v1.1.0","draft":true,"prerelease":false,"published_at":null,"assets":[]},
	 {"tag_name":"../../etc/passwd","draft":false,"prerelease":false,"published_at":"2026-01-02T00:00:00Z","assets":[]},
	 {"tag_name":"v1;rm -rf","draft":false,"prerelease":false,"published_at":"2026-01-02T00:00:00Z","assets":[]},
	 {"tag_name":"nightly","draft":false,"prerelease":false,"published_at":"2026-01-02T00:00:00Z","assets":[]},
	 {"tag_name":"v1.0.0","draft":false,"prerelease":false,"published_at":"2026-01-03T00:00:00Z","assets":[]}
	]`
	r := newRaw(t, map[string]http.HandlerFunc{"/api/repos/o/r/releases": body(list)})
	l, err := r.source(t, "tok").List(context.Background(), github.Query{Repo: "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tags(l), ","); got != "v1.0.0" {
		t.Fatalf("releases = %s, want only v1.0.0 (draft, unsafe, unparsable and duplicate tags dropped)", got)
	}
	var names []string
	for _, a := range l.Releases[0].Assets {
		names = append(names, a.Name)
	}
	if got := strings.Join(names, ","); got != "good.zip,baddigest.zip" {
		t.Errorf("assets = %s", got)
	}
	if l.Releases[0].Assets[0].SHA256 != strings.Repeat("a", 64) || l.Releases[0].Assets[1].SHA256 != "" {
		t.Errorf("digests = %+v", l.Releases[0].Assets)
	}
	joined := strings.Join(l.Warnings, "\n")
	for _, want := range []string{"unsafe name", "starter", "md5:abc", "unsafe tag", "nightly"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings do not mention %q:\n%s", want, joined)
		}
	}
}

func TestHostileAtomTagsAreDropped(t *testing.T) {
	const feed = `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom">
	 <entry><updated>2026-01-03T00:00:00Z</updated><title>ok</title><link rel="alternate" href="https://github.com/o/r/releases/tag/v1.2.0"/></entry>
	 <entry><updated>2026-01-02T00:00:00Z</updated><title>bad</title><link rel="alternate" href="https://github.com/o/r/releases/tag/../../x"/></entry>
	 <entry><updated>not a time</updated><title>notime</title><link rel="alternate" href="https://github.com/o/r/releases/tag/v1.1.0"/></entry>
	 <entry><updated>2026-01-01T00:00:00Z</updated><title>nolink</title></entry>
	</feed>`
	r := newRaw(t, map[string]http.HandlerFunc{
		"/web/o/r/releases/latest": redirect("/web/o/r/releases/tag/v1.2.0", http.StatusFound),
		"/web/o/r/releases.atom":   body(feed),
	})
	l, err := r.source(t, "").List(context.Background(), github.Query{Repo: "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tags(l), ","); got != "v1.2.0,v1.1.0" {
		t.Fatalf("releases = %s", got)
	}
	if !find(t, l, "v1.1.0").Published.IsZero() {
		t.Error("an unparsable time became a real one")
	}
	if len(l.Warnings) != 1 || !strings.Contains(l.Warnings[0], "unsafe tag") {
		t.Errorf("warnings = %v", l.Warnings)
	}
}

func TestAnUnavailableFeedStillYieldsTheLatestTag(t *testing.T) {
	r := newRaw(t, map[string]http.HandlerFunc{
		"/web/o/r/releases/latest": redirect("/web/o/r/releases/tag/v3.0.0", http.StatusFound),
		"/web/o/r/releases.atom":   func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", 500) },
	})
	l, err := r.source(t, "").List(context.Background(), github.Query{Repo: "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tags(l), ","); got != "v3.0.0" || l.Latest != "v3.0.0" {
		t.Errorf("releases = %s, latest = %q", got, l.Latest)
	}
	if !find(t, l, "v3.0.0").Published.IsZero() || len(l.Warnings) != 1 || !strings.Contains(l.Warnings[0], "feed unavailable") {
		t.Errorf("published %v, warnings %v", find(t, l, "v3.0.0").Published, l.Warnings)
	}
	if r.count("/api/") != 0 {
		t.Error("a missing feed sent the run to the API")
	}
}

func TestRenamedRepositoryIsFollowedThroughItsRedirects(t *testing.T) {
	r := newRaw(t, map[string]http.HandlerFunc{
		"/web/old/name/releases/latest": redirect("/web/new/name/releases/latest", http.StatusMovedPermanently),
		"/web/new/name/releases/latest": redirect("/web/new/name/releases/tag/v3.0.0", http.StatusFound),
		"/web/old/name/releases.atom":   redirect("/web/new/name/releases.atom", http.StatusMovedPermanently),
		"/web/new/name/releases.atom": body(`<feed xmlns="http://www.w3.org/2005/Atom"><entry><updated>2026-02-01T00:00:00Z</updated>
		  <link rel="alternate" href="https://github.com/new/name/releases/tag/v3.0.0"/></entry></feed>`),
	})
	l, err := r.source(t, "").List(context.Background(), github.Query{Repo: "old/name"})
	if err != nil {
		t.Fatal(err)
	}
	if l.Latest != "v3.0.0" || find(t, l, "v3.0.0").Published.IsZero() {
		t.Errorf("latest %q, release %+v", l.Latest, find(t, l, "v3.0.0"))
	}
	// Downloads are named after the repository the caller asked for; GitHub redirects those too.
	if got := find(t, l, "v3.0.0").Repo; got != "old/name" {
		t.Errorf("Repo = %q", got)
	}
}

func TestLatestRedirectToAForeignHostIsRefused(t *testing.T) {
	r := newRaw(t, map[string]http.HandlerFunc{
		"/web/o/r/releases/latest": redirect("http://evil.example.test/o/r/releases/tag/v9.9.9?x=secret", http.StatusFound),
	})
	_, err := r.source(t, "").List(context.Background(), github.Query{Repo: "o/r"})
	var pe *download.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *PolicyError", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error leaks the query string: %v", err)
	}
	if r.count("/api/") != 0 {
		t.Error("a policy refusal fell over to the API")
	}
}

func TestInvalidRepositoryNames(t *testing.T) {
	for _, s := range []string{"", "o", "o/r/x", "../x", "o/..", "./r", "o /r", "o/r?x=1", "o/r#f", "-o/r", "o\\r", "o/" + strings.Repeat("a", 101)} {
		if github.ValidRepo(s) {
			t.Errorf("ValidRepo(%q) = true", s)
		}
	}
	for _, s := range []string{"storytold/photocraft", "o/r", "a.b/c_d-e", "o/.github"} {
		if !github.ValidRepo(s) {
			t.Errorf("ValidRepo(%q) = false", s)
		}
	}
	e := newEnv(t, "")
	if _, err := e.src.List(context.Background(), github.Query{Repo: "../../x"}); err == nil {
		t.Error("List accepted a path-like repository")
	}
	if n := len(e.srv.Requests()); n != 0 {
		t.Errorf("%d requests for an invalid repository", n)
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]github.Mode{"": github.ModeAuto, "auto": github.ModeAuto, "api": github.ModeAPI, "web": github.ModeWeb} {
		if got, err := github.ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := github.ParseMode("rest"); err == nil {
		t.Error("ParseMode accepted an unknown mode")
	}
}

// ---- the captured real world ----

func TestRealPhotoCraftFeedAndRelease(t *testing.T) {
	atom := fixtures.Read(t, "photocraft-v0.6.0/releases.atom")
	latest := fixtures.Read(t, "photocraft-v0.6.0/release-latest.json")
	r := newRaw(t, map[string]http.HandlerFunc{
		"/web/storytold/photocraft/releases/latest": redirect("/web/storytold/photocraft/releases/tag/v0.6.0", http.StatusFound),
		"/web/storytold/photocraft/releases.atom":   func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(atom) },
		"/api/repos/storytold/photocraft/releases": func(w http.ResponseWriter, _ *http.Request) {
			// The fixture is the single release /releases/latest returned; the list is an array of them.
			_, _ = w.Write(append(append([]byte("["), bytes.TrimSpace(latest)...), ']'))
		},
	})
	src := r.source(t, "")
	ctx := context.Background()

	web, err := src.List(ctx, github.Query{Repo: "storytold/photocraft", Mode: github.ModeWeb})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tags(web), ","); got != "v0.6.0,v0.5.0,v0.3.0,v0.2.0,v0.1.1,v0.1.1-rc.5,v0.1.1-rc.4,v0.1.0" {
		t.Errorf("tags = %s", got)
	}
	if web.Latest != "v0.6.0" || len(web.Warnings) != 0 {
		t.Errorf("latest %q, warnings %v", web.Latest, web.Warnings)
	}
	if got := find(t, web, "v0.6.0").Published; !got.Equal(time.Date(2026, 10, 10, 2, 51, 42, 0, time.UTC)) {
		t.Errorf("v0.6.0 published %v", got)
	}
	if !find(t, web, "v0.1.1-rc.5").Version.IsPrerelease() {
		t.Error("v0.1.1-rc.5 is not a prerelease version")
	}

	api, err := src.List(ctx, github.Query{Repo: "storytold/photocraft", Mode: github.ModeAPI})
	if err != nil {
		t.Fatal(err)
	}
	rel := find(t, api, "v0.6.0")
	if len(rel.Assets) != 23 || len(api.Warnings) != 0 {
		t.Fatalf("%d assets, warnings %v", len(rel.Assets), api.Warnings)
	}
	for _, a := range rel.Assets {
		if len(a.SHA256) != 64 {
			t.Errorf("%s has digest %q", a.Name, a.SHA256)
		}
	}
	zip, err := src.SelectAsset(rel, "photocraft-0.6.0-windows-x64-portable.zip")
	if err != nil {
		t.Fatal(err)
	}
	if want := r.ts.URL + "/web/storytold/photocraft/releases/download/v0.6.0/photocraft-0.6.0-windows-x64-portable.zip"; zip.URL != want {
		t.Errorf("URL = %s", zip.URL)
	}
	// A wildcard that also matches the installer is ambiguous, and says so.
	_, err = src.SelectAsset(rel, "photocraft-0.6.0-windows-x64*")
	if err == nil || !strings.Contains(err.Error(), "2 assets") || !strings.Contains(err.Error(), "x64.msi") {
		t.Errorf("err = %v", err)
	}
	// The second, near-identical download of another Craft app: the CLI build is not matched by
	// the app's exact name.
	if _, err := src.SelectAsset(rel, "photocraft-0.6.0-macos-universal.dmg"); err != nil {
		t.Error(err)
	}
	if _, err := src.SelectAsset(rel, "photocraft-0.6.0-macos-universal.zip"); err == nil {
		t.Error("matched an asset that does not exist")
	}
}
