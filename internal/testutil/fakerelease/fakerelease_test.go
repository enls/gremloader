package fakerelease

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/clock"
	"github.com/enls/gremloader/internal/testutil/fixtures"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// response is the part of an HTTP response the tests look at; the body is read and closed by get.
type response struct {
	StatusCode int
	Header     http.Header
}

func get(t *testing.T, url string, headers ...string) (response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	// Never follow redirects: tests want to see them.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return response{StatusCode: resp.StatusCode, Header: resp.Header}, body
}

func decodeRelease(t *testing.T, b []byte) apiRelease {
	t.Helper()
	var r apiRelease
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("decode release: %v\n%s", err, b)
	}
	return r
}

func newPopulated(t *testing.T) *Server {
	t.Helper()
	s := New(t, WithClock(clock.NewFake(t0)))
	s.AddRelease("enls", "tool", Release{Tag: "v1.0.0", PublishedAt: t0.Add(-72 * time.Hour),
		Assets: []Asset{{Name: "tool-1.0.0.zip", Data: []byte("one")}}})
	s.AddRelease("enls", "tool", Release{Tag: "v1.1.0", PublishedAt: t0.Add(-48 * time.Hour),
		Assets: []Asset{{Name: "tool-1.1.0.zip", Data: []byte("one-one")}}})
	s.AddRelease("enls", "tool", Release{Tag: "v2.0.0-rc.1", Prerelease: true, PublishedAt: t0.Add(-24 * time.Hour)})
	s.AddRelease("enls", "tool", Release{Tag: "v9.9.9", Draft: true, PublishedAt: t0})
	return s
}

func TestLatestSkipsPrereleaseAndDraft(t *testing.T) {
	s := newPopulated(t)
	resp, body := get(t, s.APIBase()+"/repos/enls/tool/releases/latest")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if got := decodeRelease(t, body).TagName; got != "v1.1.0" {
		t.Errorf("latest = %s, want v1.1.0", got)
	}
}

func TestListIsNewestFirstAndHidesDrafts(t *testing.T) {
	s := newPopulated(t)
	_, body := get(t, s.APIBase()+"/repos/enls/tool/releases")
	var list []apiRelease
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, r := range list {
		tags = append(tags, r.TagName)
	}
	if got, want := strings.Join(tags, ","), "v2.0.0-rc.1,v1.1.0,v1.0.0"; got != want {
		t.Errorf("tags = %s, want %s", got, want)
	}
	if !list[0].Prerelease {
		t.Error("prerelease flag lost")
	}
}

func TestByTagAndMissing(t *testing.T) {
	s := newPopulated(t)
	resp, body := get(t, s.APIBase()+"/repos/enls/tool/releases/tags/v1.0.0")
	if resp.StatusCode != http.StatusOK || decodeRelease(t, body).TagName != "v1.0.0" {
		t.Errorf("by tag: %d %s", resp.StatusCode, body)
	}
	for _, path := range []string{
		"/repos/enls/tool/releases/tags/v9.9.9",    // draft
		"/repos/enls/tool/releases/tags/nope",      // unknown tag
		"/repos/enls/other/releases/latest",        // unknown repo
		"/repos/enls/other/releases/assets/123456", // unknown asset
	} {
		resp, _ := get(t, s.APIBase()+path)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestAssetDownloadBothRoots(t *testing.T) {
	s := newPopulated(t)
	_, body := get(t, s.APIBase()+"/repos/enls/tool/releases/tags/v1.1.0")
	rel := decodeRelease(t, body)
	if len(rel.Assets) != 1 {
		t.Fatalf("assets = %+v", rel.Assets)
	}
	a := rel.Assets[0]
	if a.Size != len("one-one") || a.Name != "tool-1.1.0.zip" {
		t.Errorf("asset metadata = %+v", a)
	}

	for _, url := range []string{a.URL, a.BrowserDownloadURL, s.AssetURL("enls", "tool", "v1.1.0", "tool-1.1.0.zip")} {
		resp, data := get(t, url, "Accept", "application/octet-stream")
		if resp.StatusCode != http.StatusOK || string(data) != "one-one" {
			t.Errorf("%s: %d %q", url, resp.StatusCode, data)
		}
	}

	resp, _ := get(t, s.AssetURL("enls", "tool", "v1.1.0", "missing.zip"))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing asset: %d, want 404", resp.StatusCode)
	}
}

func TestRateLimitHitsAPIOnlyAndUsesClockForReset(t *testing.T) {
	s := newPopulated(t)
	s.SetRateLimited(true)

	resp, body := get(t, s.APIBase()+"/repos/enls/tool/releases/latest")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("API status %d, want 403: %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-RateLimit-Remaining") != "0" {
		t.Errorf("X-RateLimit-Remaining = %q", resp.Header.Get("X-RateLimit-Remaining"))
	}
	wantReset := t0.Add(time.Hour).Unix()
	if got := resp.Header.Get("X-RateLimit-Reset"); got != itoa(wantReset) {
		t.Errorf("X-RateLimit-Reset = %q, want %d", got, wantReset)
	}
	if !strings.Contains(string(body), "rate limit exceeded") {
		t.Errorf("body = %s", body)
	}

	// The fallback path a rate-limited client uses: the website still answers.
	resp, _ = get(t, s.WebBase()+"/enls/tool/releases/latest")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("web latest status %d, want 302", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Location"), s.WebBase()+"/enls/tool/releases/tag/v1.1.0"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	resp, data := get(t, s.AssetURL("enls", "tool", "v1.1.0", "tool-1.1.0.zip"))
	if resp.StatusCode != http.StatusOK || string(data) != "one-one" {
		t.Errorf("download while rate limited: %d %q", resp.StatusCode, data)
	}

	s.SetRateLimited(false)
	if resp, _ := get(t, s.APIBase()+"/repos/enls/tool/releases/latest"); resp.StatusCode != http.StatusOK {
		t.Errorf("after turning rate limit off: %d", resp.StatusCode)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestStatusFaultAppliesNTimesThenStops(t *testing.T) {
	s := newPopulated(t)
	s.Inject(Fault{PathContains: "/download/", Status: http.StatusServiceUnavailable, Times: 2})
	url := s.AssetURL("enls", "tool", "v1.0.0", "tool-1.0.0.zip")

	for i := 0; i < 2; i++ {
		if resp, _ := get(t, url); resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("attempt %d: status %d, want 503", i+1, resp.StatusCode)
		}
	}
	resp, data := get(t, url)
	if resp.StatusCode != http.StatusOK || string(data) != "one" {
		t.Errorf("third attempt: %d %q", resp.StatusCode, data)
	}
	if n := s.RequestCount("/download/"); n != 3 {
		t.Errorf("RequestCount = %d, want 3", n)
	}
}

func TestTruncatedDownloadIsAShortRead(t *testing.T) {
	s := New(t)
	payload := []byte(strings.Repeat("0123456789", 1000))
	s.AddRelease("enls", "tool", Release{Tag: "v1", Assets: []Asset{{Name: "big.bin", Data: payload}}})
	s.Inject(Fault{PathContains: "big.bin", TruncateAfter: 2500, Times: 1})

	resp, err := http.Get(s.AssetURL("enls", "tool", "v1", "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ContentLength != int64(len(payload)) {
		t.Errorf("Content-Length = %d, want %d (the full size must still be advertised)", resp.ContentLength, len(payload))
	}
	got, err := io.ReadAll(resp.Body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("read error = %v, want io.ErrUnexpectedEOF", err)
	}
	if len(got) != 2500 {
		t.Errorf("read %d bytes before the cut, want 2500", len(got))
	}

	// One-shot fault is spent: the retry gets the whole file.
	resp2, err := http.Get(s.AssetURL("enls", "tool", "v1", "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	full, err := io.ReadAll(resp2.Body)
	if err != nil || len(full) != len(payload) {
		t.Errorf("retry: %d bytes, err %v", len(full), err)
	}
}

func TestRequestLogCapturesHeaders(t *testing.T) {
	s := newPopulated(t)
	get(t, s.APIBase()+"/repos/enls/tool/releases/latest", "Authorization", "Bearer abc", "Accept", "application/vnd.github+json")

	reqs := s.Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	if reqs[0].Auth != "Bearer abc" || reqs[0].Accept != "application/vnd.github+json" || reqs[0].Method != "GET" {
		t.Errorf("request = %+v", reqs[0])
	}
	s.ResetRequests()
	if len(s.Requests()) != 0 {
		t.Error("ResetRequests did not clear the log")
	}
}

// The harness has to be able to serve the real fixtures byte for byte, or tests that verify
// checksums against it are meaningless.
func TestServesRealFixtureBytesUnchanged(t *testing.T) {
	zipBytes := fixtures.Read(t, "craftwerk-v0.2.0/CraftWerk-Manager.zip")
	sums := fixtures.Read(t, "craftwerk-v0.2.0/SHA256SUMS.txt")

	s := New(t)
	s.AddRelease("enls", "craftwerk", Release{Tag: "v0.2.0", Assets: []Asset{
		{Name: "CraftWerk-Manager.zip", ContentType: "application/zip", Data: zipBytes},
		{Name: "SHA256SUMS.txt", ContentType: "text/plain", Data: sums},
	}})

	_, gotZip := get(t, s.AssetURL("enls", "craftwerk", "v0.2.0", "CraftWerk-Manager.zip"))
	_, gotSums := get(t, s.AssetURL("enls", "craftwerk", "v0.2.0", "SHA256SUMS.txt"))
	if string(gotZip) != string(zipBytes) || string(gotSums) != string(sums) {
		t.Error("served bytes differ from the fixtures")
	}
}

// A request the rate limiter answers never reaches fault injection, so it must not use up one of
// a fault's counted uses: a test combining the two would otherwise never see its fault.
func TestRateLimitedRequestDoesNotConsumeAFault(t *testing.T) {
	s := newPopulated(t)
	s.Inject(Fault{PathContains: "/api/", Status: http.StatusServiceUnavailable, Times: 1})
	s.SetRateLimited(true)
	if resp, _ := get(t, s.APIBase()+"/repos/enls/tool/releases"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("limited request: status %d, want 403", resp.StatusCode)
	}
	s.SetRateLimited(false)
	if resp, _ := get(t, s.APIBase()+"/repos/enls/tool/releases"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d, want the 503 that was injected", resp.StatusCode)
	}
}

// A draft is invisible everywhere, including to a client that knows an asset id.
func TestDraftAssetsAreNotServedByID(t *testing.T) {
	s := New(t)
	s.AddRelease("o", "r", Release{Tag: "v1.0.0", Draft: true, Assets: []Asset{{Name: "a.zip", Data: []byte("secret")}}})
	var id string
	for k, a := range s.assets {
		if a.tag == "v1.0.0" {
			id = strconv.FormatInt(k, 10)
		}
	}
	if id == "" {
		t.Fatal("asset not stored")
	}
	if resp, body := get(t, s.APIBase()+"/repos/o/r/releases/assets/"+id); resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d (%q), want 404 for an asset of a draft", resp.StatusCode, body)
	}
}
