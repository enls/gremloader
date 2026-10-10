package download_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/download"
	"github.com/enls/gremloader/internal/testutil/fakerelease"
)

// The fake serves its API and website roots from one listener, so both have the same host:port.
// Real GitHub uses different hosts for them, and the token rules are about hosts. Reaching the API
// root through the name "localhost" instead of "127.0.0.1" gives the same listener a second
// identity, which is enough to tell the two apart.
func apiRoot(srv *fakerelease.Server) string {
	return strings.Replace(srv.APIBase(), "127.0.0.1", "localhost", 1)
}

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

type sleeper struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeper) Sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *sleeper) got() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.d...)
}

type rig struct {
	srv    *fakerelease.Server
	client *download.Client
	sleep  *sleeper
	dir    string
}

// newRig starts a fake with one release (o/r v1.0.0, asset "app.zip") and a client that may talk
// to it. The token, if any, is allowed only for the "localhost" API identity.
func newRig(t *testing.T, token string, fopts []fakerelease.Option, mutate func(*download.Config)) *rig {
	t.Helper()
	srv := fakerelease.New(t, fopts...)
	srv.AddRelease("o", "r", fakerelease.Release{
		Tag:    "v1.0.0",
		Assets: []fakerelease.Asset{{Name: "app.zip", Data: bytes.Repeat([]byte("0123456789"), 100)}},
	})
	sl := &sleeper{}
	cfg := download.Config{
		Policy: download.Policy{
			Hosts:      []string{"127.0.0.1", "localhost"},
			TokenHosts: []string{hostOf(t, apiRoot(srv))},
			AllowHTTP:  true,
		},
		Token:     token,
		UserAgent: "gremloader-test",
		Sleep:     sl.Sleep,
		Jitter:    func(time.Duration) time.Duration { return 0 },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return &rig{srv: srv, client: download.New(cfg), sleep: sl, dir: t.TempDir()}
}

func (r *rig) webAsset() string { return r.srv.AssetURL("o", "r", "v1.0.0", "app.zip") }
func (r *rig) dst() string      { return filepath.Join(r.dir, "asset.bin") }

func (r *rig) fetch(url string) (download.Result, error) {
	return r.client.Fetch(context.Background(), url, r.dst(), download.FetchOptions{MaxBytes: 1 << 20})
}

func noFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("unexpected file left behind: %s", e.Name())
	}
}

func TestFetchStreamsHashesAndPlacesTheFile(t *testing.T) {
	r := newRig(t, "", nil, nil)
	res, err := r.fetch(r.webAsset())
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte("0123456789"), 100)
	sum := sha256.Sum256(want)
	if res.SHA256 != hex.EncodeToString(sum[:]) || res.Size != int64(len(want)) {
		t.Errorf("result = %+v, want sha256 %x size %d", res, sum, len(want))
	}
	got, err := os.ReadFile(r.dst())
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("file content wrong (err %v)", err)
	}
	if _, err := os.Stat(r.dst() + ".part"); !os.IsNotExist(err) {
		t.Errorf(".part file left behind (%v)", err)
	}
	if ua := r.srv.Requests()[0].UserAgent; ua != "gremloader-test" {
		t.Errorf("User-Agent = %q", ua)
	}
}

func TestDefaultUserAgentNamesTheProgram(t *testing.T) {
	r := newRig(t, "", nil, func(c *download.Config) { c.UserAgent = "" })
	if _, err := r.fetch(r.webAsset()); err != nil {
		t.Fatal(err)
	}
	if ua := r.srv.Requests()[0].UserAgent; !strings.HasPrefix(ua, "gremloader/") {
		t.Errorf("User-Agent = %q", ua)
	}
}

func TestPlainHTTPRefusedWithoutOptIn(t *testing.T) {
	r := newRig(t, "", nil, func(c *download.Config) { c.Policy.AllowHTTP = false })
	_, err := r.fetch(r.webAsset())
	var pe *download.PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *PolicyError", err)
	}
	if n := len(r.srv.Requests()); n != 0 {
		t.Errorf("%d requests were made for a refused URL", n)
	}
}

func TestHostOutsideAllowListRefusedBeforeAnyRequest(t *testing.T) {
	r := newRig(t, "", nil, func(c *download.Config) { c.Policy.Hosts = []string{"github.com"} })
	_, err := r.fetch(r.webAsset())
	var pe *download.PolicyError
	if !errors.As(err, &pe) || pe.Redirect {
		t.Fatalf("err = %v, want a non-redirect *PolicyError", err)
	}
	if n := len(r.srv.Requests()); n != 0 {
		t.Errorf("%d requests were made", n)
	}
}

func TestRedirectToDisallowedHostIsRefused(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "http://evil.example.test/steal?sig=SECRET", http.StatusFound)
	}))
	t.Cleanup(ts.Close)

	c := download.New(download.Config{Policy: download.Policy{Hosts: []string{"127.0.0.1"}, AllowHTTP: true}})
	_, err := c.Get(context.Background(), download.Request{URL: ts.URL + "/a"})
	var pe *download.PolicyError
	if !errors.As(err, &pe) || !pe.Redirect {
		t.Fatalf("err = %v, want a redirect *PolicyError", err)
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "sig=") {
		t.Errorf("error leaks the query string: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("server hit %d times, want 1 (a refused redirect is not retried)", hits.Load())
	}
}

func TestRedirectLimit(t *testing.T) {
	var hits atomic.Int32
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, ts.URL+"/again", http.StatusFound)
	}))
	t.Cleanup(ts.Close)

	c := download.New(download.Config{Policy: download.Policy{Hosts: []string{"127.0.0.1"}, AllowHTTP: true}})
	_, err := c.Get(context.Background(), download.Request{URL: ts.URL + "/start"})
	var pe *download.PolicyError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "5 redirects") {
		t.Fatalf("err = %v, want a *PolicyError about 5 redirects", err)
	}
	if hits.Load() != 6 {
		t.Errorf("server hit %d times, want 6 (the request plus five redirects)", hits.Load())
	}
}

func assetAPIURL(t *testing.T, r *rig) string {
	t.Helper()
	resp, err := http.Get(r.srv.APIBase() + "/repos/o/r/releases/latest")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var rel struct{ Assets []struct{ URL string } }
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil || len(rel.Assets) != 1 {
		t.Fatalf("decode release: %v (%d assets)", err, len(rel.Assets))
	}
	r.srv.ResetRequests()
	return rel.Assets[0].URL
}

// The API and the CDN are both on 127.0.0.1 here, on different ports. Go compares host names only
// and would forward the token to the other port; the client must not.
func TestTokenIsStrippedWhenARedirectLeavesTheTokenHost(t *testing.T) {
	r := newRig(t, "ghp_secret", []fakerelease.Option{fakerelease.WithCDN()}, nil)
	r.client = download.New(download.Config{
		Policy: download.Policy{
			Hosts:      []string{"127.0.0.1"},
			TokenHosts: []string{hostOf(t, r.srv.APIBase())},
			AllowHTTP:  true,
		},
		Token: "ghp_secret",
	})
	apiURL := assetAPIURL(t, r)

	_, err := r.client.Fetch(context.Background(), apiURL, r.dst(), download.FetchOptions{
		MaxBytes: 1 << 20,
		Header:   http.Header{"Accept": {"application/octet-stream"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawAPI, sawCDN bool
	for _, q := range r.srv.Requests() {
		switch q.Origin {
		case "api":
			sawAPI = true
			if q.Auth != "Bearer ghp_secret" {
				t.Errorf("API request Authorization = %q, want the bearer token", q.Auth)
			}
		case "cdn":
			sawCDN = true
			if q.Auth != "" {
				t.Errorf("CDN request carried Authorization %q", q.Auth)
			}
		}
	}
	if !sawAPI || !sawCDN {
		t.Errorf("did not exercise the redirect (api %v, cdn %v)", sawAPI, sawCDN)
	}
}

func TestTokenIsNotSentToTheWebsiteIdentity(t *testing.T) {
	r := newRig(t, "ghp_secret", []fakerelease.Option{fakerelease.WithCDN()}, nil)
	if _, err := r.fetch(r.webAsset()); err != nil {
		t.Fatal(err)
	}
	for _, q := range r.srv.Requests() {
		if q.Auth != "" {
			t.Errorf("%s request to %s carried Authorization %q", q.Origin, q.Path, q.Auth)
		}
	}
}

func TestRetriesServerErrorsWithBackoff(t *testing.T) {
	r := newRig(t, "", nil, nil)
	r.srv.Inject(fakerelease.Fault{PathContains: "/releases/download/", Status: 503, Times: 2})
	if _, err := r.fetch(r.webAsset()); err != nil {
		t.Fatal(err)
	}
	if n := r.srv.RequestCount("/releases/download/"); n != 3 {
		t.Errorf("requests = %d, want 3", n)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second}
	if got := r.sleep.got(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("sleeps = %v, want %v", got, want)
	}
}

func TestGivesUpAfterThreeAttempts(t *testing.T) {
	r := newRig(t, "", nil, nil)
	r.srv.Inject(fakerelease.Fault{PathContains: "/releases/download/", Status: 502})
	_, err := r.fetch(r.webAsset())
	var se *download.StatusError
	if !errors.As(err, &se) || se.Code != 502 {
		t.Fatalf("err = %v, want *StatusError 502", err)
	}
	if n := r.srv.RequestCount("/releases/download/"); n != 3 {
		t.Errorf("requests = %d, want 3", n)
	}
	noFiles(t, r.dir)
}

func TestClientErrorsFailFast(t *testing.T) {
	r := newRig(t, "", nil, nil)
	_, err := r.fetch(r.srv.AssetURL("o", "r", "v1.0.0", "missing.zip"))
	var se *download.StatusError
	if !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("err = %v, want *StatusError 404", err)
	}
	if n := len(r.srv.Requests()); n != 1 || len(r.sleep.got()) != 0 {
		t.Errorf("requests = %d, sleeps = %v; want one request and no waiting", n, r.sleep.got())
	}
}

func TestRetryAfterIsHonouredAndCapped(t *testing.T) {
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"2", 2 * time.Second},
		{"3600", 60 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.header, func(t *testing.T) {
			r := newRig(t, "", nil, nil)
			r.srv.Inject(fakerelease.Fault{
				PathContains: "/releases/download/", Status: 429, Times: 1,
				Header: map[string]string{"Retry-After": c.header},
			})
			if _, err := r.fetch(r.webAsset()); err != nil {
				t.Fatal(err)
			}
			if got := r.sleep.got(); len(got) != 1 || got[0] != c.want {
				t.Errorf("sleeps = %v, want [%v]", got, c.want)
			}
		})
	}
}

func TestSecondaryRateLimitIsRetried(t *testing.T) {
	r := newRig(t, "", nil, nil)
	r.srv.Inject(fakerelease.Fault{
		PathContains: "/releases/download/", Status: 403, Times: 1,
		Header: map[string]string{"Retry-After": "1"},
	})
	if _, err := r.fetch(r.webAsset()); err != nil {
		t.Fatal(err)
	}
	if got := r.sleep.got(); len(got) != 1 || got[0] != time.Second {
		t.Errorf("sleeps = %v, want [1s]", got)
	}
}

func TestPrimaryRateLimitIsNotRetried(t *testing.T) {
	r := newRig(t, "", nil, nil)
	r.srv.SetRateLimited(true)
	_, err := r.client.Get(context.Background(), download.Request{URL: r.srv.APIBase() + "/repos/o/r/releases"})
	var rl *download.RateLimitError
	if !errors.As(err, &rl) || !rl.Primary || rl.Reset.IsZero() {
		t.Fatalf("err = %v, want a primary *RateLimitError with a reset time", err)
	}
	if n := len(r.srv.Requests()); n != 1 || len(r.sleep.got()) != 0 {
		t.Errorf("requests = %d, sleeps = %v; want one request and no waiting", n, r.sleep.got())
	}
}

func TestTruncatedBodyIsRetried(t *testing.T) {
	r := newRig(t, "", nil, nil)
	r.srv.Inject(fakerelease.Fault{PathContains: "/releases/download/", TruncateAfter: 10, Times: 1})
	res, err := r.fetch(r.webAsset())
	if err != nil {
		t.Fatal(err)
	}
	if res.Size != 1000 {
		t.Errorf("size = %d, want 1000", res.Size)
	}
	if n := r.srv.RequestCount("/releases/download/"); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
}

func TestAlwaysTruncatedLeavesNothingBehind(t *testing.T) {
	r := newRig(t, "", nil, nil)
	r.srv.Inject(fakerelease.Fault{PathContains: "/releases/download/", TruncateAfter: 10})
	if _, err := r.fetch(r.webAsset()); err == nil {
		t.Fatal("a body that never completes was accepted")
	}
	noFiles(t, r.dir)
}

func TestStalledTransferTimesOut(t *testing.T) {
	release := make(chan struct{})
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("0123456789"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); ts.Close() })

	sl := &sleeper{}
	c := download.New(download.Config{
		Policy:       download.Policy{Hosts: []string{"127.0.0.1"}, AllowHTTP: true},
		StallTimeout: 150 * time.Millisecond,
		Attempts:     2,
		Sleep:        sl.Sleep,
	})
	dir := t.TempDir()
	_, err := c.Fetch(context.Background(), ts.URL+"/x", filepath.Join(dir, "f"), download.FetchOptions{MaxBytes: 1 << 20})
	if !errors.Is(err, download.ErrStalled) {
		t.Fatalf("err = %v, want ErrStalled", err)
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2 attempts", hits.Load())
	}
	noFiles(t, dir)
}

func TestAnnouncedSizeOverCapIsRefusedUnread(t *testing.T) {
	r := newRig(t, "", nil, nil)
	_, err := r.client.Fetch(context.Background(), r.webAsset(), r.dst(), download.FetchOptions{MaxBytes: 500})
	if !errors.Is(err, download.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if n := len(r.srv.Requests()); n != 1 {
		t.Errorf("requests = %d, want 1 (too large is not retried)", n)
	}
	noFiles(t, r.dir)
}

func TestUnannouncedSizeOverCapIsStopped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := bytes.Repeat([]byte("x"), 4096)
		for range 64 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(ts.Close)

	c := download.New(download.Config{Policy: download.Policy{Hosts: []string{"127.0.0.1"}, AllowHTTP: true}})
	dir := t.TempDir()
	_, err := c.Fetch(context.Background(), ts.URL+"/x", filepath.Join(dir, "f"), download.FetchOptions{MaxBytes: 10000})
	if !errors.Is(err, download.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	noFiles(t, dir)
}

func TestGetCapsTheBody(t *testing.T) {
	r := newRig(t, "", nil, nil)
	_, err := r.client.Get(context.Background(), download.Request{URL: r.webAsset(), MaxBytes: 100})
	if !errors.Is(err, download.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestSignedURLsNeverAppearInErrors(t *testing.T) {
	r := newRig(t, "", []fakerelease.Option{fakerelease.WithCDN()}, nil)
	r.srv.Inject(fakerelease.Fault{PathContains: "/cdn/", Status: 500})
	_, err := r.fetch(r.webAsset())
	if err == nil {
		t.Fatal("expected a failure")
	}
	for _, bad := range []string{"FAKESIG", "sig=", "jwt=", "eyJFAKE"} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("error leaks %q: %v", bad, err)
		}
	}
	if r.srv.RequestCount("/cdn/") != 3 {
		t.Errorf("CDN requests = %d, want 3", r.srv.RequestCount("/cdn/"))
	}
}

func TestNoFollowHandsBackTheRedirect(t *testing.T) {
	r := newRig(t, "", nil, nil)
	resp, err := r.client.Get(context.Background(), download.Request{
		URL: r.srv.WebBase() + "/o/r/releases/latest", NoFollow: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusFound || !strings.HasSuffix(resp.Header.Get("Location"), "/releases/tag/v1.0.0") {
		t.Errorf("status %d, Location %q", resp.Status, resp.Header.Get("Location"))
	}
	if r.client.Allowed(resp.Header.Get("Location")) != nil {
		t.Error("Allowed refused the fake's own Location")
	}
}

func TestDestinationProblemsAreNotRetried(t *testing.T) {
	r := newRig(t, "", nil, nil)
	_, err := r.client.Fetch(context.Background(), r.webAsset(), filepath.Join(r.dir, "no-such-dir", "f"),
		download.FetchOptions{MaxBytes: 1 << 20})
	if err == nil {
		t.Fatal("expected an error")
	}
	if n := r.srv.RequestCount("/releases/download/"); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

func TestCancellationStopsRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := newRig(t, "", nil, func(c *download.Config) {
		c.Sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	})
	r.srv.Inject(fakerelease.Fault{PathContains: "/releases/download/", Status: 503})
	_, err := r.client.Fetch(ctx, r.webAsset(), r.dst(), download.FetchOptions{MaxBytes: 1 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := r.srv.RequestCount("/releases/download/"); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

func TestFetchNeedsASizeCap(t *testing.T) {
	r := newRig(t, "", nil, nil)
	if _, err := r.client.Fetch(context.Background(), r.webAsset(), r.dst(), download.FetchOptions{}); err == nil {
		t.Fatal("Fetch without MaxBytes was accepted")
	}
	if n := len(r.srv.Requests()); n != 0 {
		t.Errorf("%d requests were made", n)
	}
}
