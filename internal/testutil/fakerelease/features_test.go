package fakerelease

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/checksum"
	"github.com/enls/gremloader/internal/clock"
	"github.com/enls/gremloader/internal/testutil/fixtures"
)

// rawGet is a plain GET that keeps headers, follows redirects like a normal client, and never
// leaks the body.
func rawGet(t *testing.T, url string, headers ...string) (response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{StatusCode: resp.StatusCode, Header: resp.Header}, b
}

func TestAssetsCarryDigestAndState(t *testing.T) {
	s := New(t)
	data := []byte("hello assets")
	s.AddRelease("o", "r", Release{Tag: "v1", Assets: []Asset{{Name: "a.bin", Data: data}}})

	_, body := rawGet(t, s.APIBase()+"/repos/o/r/releases/latest")
	var rel apiRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		t.Fatal(err)
	}
	a := rel.Assets[0]
	want, _, _ := checksum.SHA256(strings.NewReader(string(data)))
	if a.State != "uploaded" || a.Digest != "sha256:"+want {
		t.Errorf("state=%q digest=%q, want uploaded / sha256:%s", a.State, a.Digest, want)
	}
	if got, err := checksum.ParseDigest(a.Digest); err != nil || got != want {
		t.Errorf("ParseDigest: %q, %v", got, err)
	}
}

func TestCreatedAtDecidesLatestNotPublishedAt(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s := New(t)
	// v1.0.0 was created first but published last; GitHub's "latest" follows creation time.
	s.AddRelease("o", "r", Release{Tag: "v1.0.0", CreatedAt: t0, PublishedAt: t0.Add(48 * time.Hour)})
	s.AddRelease("o", "r", Release{Tag: "v0.9.0", CreatedAt: t0.Add(24 * time.Hour), PublishedAt: t0.Add(25 * time.Hour)})

	_, body := rawGet(t, s.APIBase()+"/repos/o/r/releases/latest")
	var rel apiRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v0.9.0" {
		t.Errorf("latest = %s, want v0.9.0 (newest by created_at)", rel.TagName)
	}
}

func TestETagAndConditionalRequest(t *testing.T) {
	s := New(t)
	s.AddRelease("o", "r", Release{Tag: "v1"})
	url := s.APIBase() + "/repos/o/r/releases/latest"

	resp, _ := rawGet(t, url)
	etag := resp.Header.Get("ETag")
	if !strings.HasPrefix(etag, `W/"`) {
		t.Fatalf("ETag = %q", etag)
	}
	resp, body := rawGet(t, url, "If-None-Match", etag)
	if resp.StatusCode != http.StatusNotModified || len(body) != 0 {
		t.Errorf("conditional GET: %d, %d body bytes", resp.StatusCode, len(body))
	}

	s.AddRelease("o", "r", Release{Tag: "v2", PublishedAt: time.Now().Add(time.Hour)})
	resp, _ = rawGet(t, url, "If-None-Match", etag)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("after the content changed: %d, want 200", resp.StatusCode)
	}
}

// Observed against the real service: an unauthenticated 304 still costs one unit.
func TestQuotaCountsNotModifiedAndAuthenticatedIsFree(t *testing.T) {
	s := New(t)
	s.AddRelease("o", "r", Release{Tag: "v1"})
	s.SetQuota(3)
	url := s.APIBase() + "/repos/o/r/releases/latest"

	resp, _ := rawGet(t, url)
	etag := resp.Header.Get("ETag")
	if resp.Header.Get("X-RateLimit-Remaining") != "2" || resp.Header.Get("X-RateLimit-Limit") != "3" {
		t.Errorf("headers after first call: remaining=%q limit=%q", resp.Header.Get("X-RateLimit-Remaining"), resp.Header.Get("X-RateLimit-Limit"))
	}
	resp, _ = rawGet(t, url, "If-None-Match", etag)
	if resp.StatusCode != http.StatusNotModified || resp.Header.Get("X-RateLimit-Remaining") != "1" {
		t.Errorf("304 call: %d remaining=%q (must still cost one)", resp.StatusCode, resp.Header.Get("X-RateLimit-Remaining"))
	}

	// An authenticated request is not counted against the unauthenticated quota.
	rawGet(t, url, "Authorization", "Bearer x")
	if got := s.QuotaRemaining(); got != 1 {
		t.Errorf("remaining after an authenticated call = %d, want 1", got)
	}

	rawGet(t, url) // 1 -> 0
	resp, body := rawGet(t, url)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "rate limit exceeded") {
		t.Errorf("exhausted: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-RateLimit-Reset") == "" {
		t.Error("no X-RateLimit-Reset")
	}
	// Web roots are never counted.
	if resp, _ := rawGet(t, s.WebBase()+"/o/r/releases.atom"); resp.StatusCode != http.StatusOK {
		t.Errorf("atom while API quota is exhausted: %d", resp.StatusCode)
	}
}

func TestFaultHeadersRetryAfter(t *testing.T) {
	s := New(t)
	s.AddRelease("o", "r", Release{Tag: "v1"})
	s.Inject(Fault{PathContains: "/releases/latest", Status: http.StatusTooManyRequests, Header: map[string]string{"Retry-After": "2"}, Times: 1})
	resp, _ := rawGet(t, s.APIBase()+"/repos/o/r/releases/latest")
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "2" {
		t.Errorf("faulted: %d Retry-After=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if resp, _ := rawGet(t, s.APIBase()+"/repos/o/r/releases/latest"); resp.StatusCode != http.StatusOK {
		t.Errorf("fault should be spent: %d", resp.StatusCode)
	}
}

func TestCDNRedirectIsSignedAndOnAnotherHost(t *testing.T) {
	s := New(t, WithCDN())
	s.AddRelease("o", "r", Release{Tag: "v1", Assets: []Asset{{Name: "a.bin", Data: []byte("payload")}}})
	if s.CDNBase() == "" || strings.HasPrefix(s.CDNBase(), s.WebBase()[:strings.Index(s.WebBase(), "/web")]) {
		t.Fatalf("CDN must be a different host:port, got %q vs %q", s.CDNBase(), s.WebBase())
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest(http.MethodGet, s.AssetURL("o", "r", "v1", "a.bin"), nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(loc, s.CDNBase()+"/") || !strings.Contains(loc, "sig=FAKESIG") {
		t.Fatalf("redirect: %d %q", resp.StatusCode, loc)
	}

	// A client that follows it gets the bytes, and the CDN request is logged with its query.
	_, got := rawGet(t, s.AssetURL("o", "r", "v1", "a.bin"))
	if string(got) != "payload" {
		t.Errorf("body = %q", got)
	}
	var cdn []Request
	for _, r := range s.Requests() {
		if r.Origin == "cdn" {
			cdn = append(cdn, r)
		}
	}
	if len(cdn) != 1 || !strings.Contains(cdn[0].Query, "sig=FAKESIG") {
		t.Errorf("cdn requests = %+v", cdn)
	}
	// NOTE for the download client (M2): net/http only drops Authorization on a redirect when the
	// *hostname* differs, and both fake roots are 127.0.0.1 (ports are ignored), so Go forwards it
	// here. In production github.com -> release-assets.githubusercontent.com does differ, but the
	// client must not depend on that: it attaches the token per request only for the allow-listed
	// API hosts and strips it itself in CheckRedirect. The request log's Auth field is what its
	// tests assert on.

	// A CDN URL without a valid signature is refused.
	if resp, _ := rawGet(t, s.CDNBase()+"/1/a.bin?sig=bad"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unsigned CDN request: %d", resp.StatusCode)
	}
}

func TestAtomFeedShape(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s := New(t, WithClock(clock.NewFake(t0)))
	s.AddRelease("o", "r", Release{Tag: "v1.0.0", PublishedAt: t0.Add(-48 * time.Hour)})
	s.AddRelease("o", "r", Release{Tag: "v1.1.0-rc.1", Prerelease: true, PublishedAt: t0.Add(-24 * time.Hour)})
	s.AddRelease("o", "r", Release{Tag: "v9", Draft: true})

	resp, body := rawGet(t, s.WebBase()+"/o/r/releases.atom")
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "atom+xml") {
		t.Fatalf("%d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var feed atomFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		t.Fatalf("not XML: %v\n%s", err, body)
	}
	if len(feed.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (draft hidden, prerelease present)", len(feed.Entries))
	}
	if !strings.HasSuffix(feed.Entries[0].ID, "/v1.1.0-rc.1") || feed.Entries[0].Updated != "2026-09-30T12:00:00Z" {
		t.Errorf("first entry = %+v", feed.Entries[0])
	}
	if !strings.HasSuffix(feed.Entries[0].Link.Href, "/releases/tag/v1.1.0-rc.1") {
		t.Errorf("link = %q", feed.Entries[0].Link.Href)
	}
}

// The harness must be able to be *read* by the same code that reads the real thing, so the real
// captured feed is parsed with the harness's own atom types (same element names).
func TestRealAtomFixtureParses(t *testing.T) {
	var feed atomFeed
	if err := xml.Unmarshal(fixtures.Read(t, "photocraft-v0.6.0/releases.atom"), &feed); err != nil {
		t.Fatal(err)
	}
	if len(feed.Entries) != 8 {
		t.Fatalf("entries = %d, want 8", len(feed.Entries))
	}
	first := feed.Entries[0]
	if first.Title != "PhotoCraft v0.6.0" || first.Updated != "2026-10-10T02:51:42Z" ||
		!strings.HasSuffix(first.ID, "/v0.6.0") || !strings.HasSuffix(first.Link.Href, "/releases/tag/v0.6.0") {
		t.Errorf("first entry = %+v", first)
	}
}

// And the real latest-release JSON must decode into the harness's own release/asset types: if
// the real API has fields the harness leaves out, this is where a mismatch would show.
func TestRealReleaseJSONFixture(t *testing.T) {
	var rel apiRelease
	if err := json.Unmarshal(fixtures.Read(t, "photocraft-v0.6.0/release-latest.json"), &rel); err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v0.6.0" || rel.Draft || rel.Prerelease {
		t.Errorf("release = %+v", rel)
	}
	if !rel.CreatedAt.Before(rel.PublishedAt) {
		t.Errorf("created_at %v should precede published_at %v (assets are uploaded in between)", rel.CreatedAt, rel.PublishedAt)
	}
	if len(rel.Assets) != 23 {
		t.Fatalf("assets = %d, want 23", len(rel.Assets))
	}
	names := map[string]apiAsset{}
	for _, a := range rel.Assets {
		if a.State != "uploaded" {
			t.Errorf("%s state = %q", a.Name, a.State)
		}
		if _, err := checksum.ParseDigest(a.Digest); err != nil {
			t.Errorf("%s digest: %v", a.Name, err)
		}
		names[a.Name] = a
	}
	// The digests GitHub reports must agree with the checksum file the author published.
	sums, err := checksum.Parse(strings.NewReader(string(fixtures.Read(t, "photocraft-v0.6.0/SHA256SUMS.txt"))))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range sums.Entries {
		a, ok := names[e.Name]
		if !ok {
			t.Errorf("%s is in SHA256SUMS.txt but not in the release", e.Name)
			continue
		}
		d, _ := checksum.ParseDigest(a.Digest)
		if d != e.Hash {
			t.Errorf("%s: API digest %s != SHA256SUMS %s", e.Name, d, e.Hash)
		}
		checked++
	}
	if checked != 22 {
		t.Errorf("cross-checked %d assets, want 22", checked)
	}
	if strconv.Itoa(len(sums.Entries)) != "22" {
		t.Errorf("SHA256SUMS has %d entries", len(sums.Entries))
	}
}
