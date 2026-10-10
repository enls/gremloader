// Package fakerelease is an in-process stand-in for the parts of GitHub that GremLoader talks to,
// so no test ever depends on the real network or on api.github.com's rate limit.
//
// It serves two roots, mirroring GitHub's two hosts:
//
//	APIBase()  the REST API (api.github.com)
//	  GET /repos/{owner}/{repo}/releases/latest
//	  GET /repos/{owner}/{repo}/releases
//	  GET /repos/{owner}/{repo}/releases/tags/{tag}
//	  GET /repos/{owner}/{repo}/releases/assets/{id}      (the asset bytes)
//	WebBase()  the website (github.com)
//	  GET /{owner}/{repo}/releases/latest                 (302 to .../releases/tag/{tag})
//	  GET /{owner}/{repo}/releases/tag/{tag}
//	  GET /{owner}/{repo}/releases/download/{tag}/{name}  (the asset bytes)
//
// Behaviour that real clients have to survive can be switched on per test: API rate limiting
// (which, as on GitHub, does not touch the website), injected HTTP failures, and downloads that
// are cut off part-way.
//
// Simplifications: draft releases are never visible (GitHub shows them only to people with push
// access), and "latest" is the newest published non-draft, non-prerelease release.
package fakerelease

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/enls/gremloader/internal/clock"
)

// Asset is one downloadable file attached to a release.
type Asset struct {
	Name        string
	ContentType string // defaults to application/octet-stream
	Data        []byte
}

// Release is one release of one repository.
type Release struct {
	Tag         string
	Name        string // defaults to Tag
	Draft       bool
	Prerelease  bool
	PublishedAt time.Time // defaults to the server clock's now
	// CreatedAt defaults to PublishedAt. GitHub orders "latest" and the release list by creation
	// time, not publish time; set it earlier to reproduce a draft that was published long after.
	CreatedAt time.Time
	Assets    []Asset
}

// Fault makes matching requests misbehave. It applies to every root.
type Fault struct {
	// PathContains selects requests whose URL path contains this text.
	PathContains string
	// Status, if non-zero, is returned instead of the normal response.
	Status int
	// Header is added to the faulted response (e.g. "Retry-After": "2").
	Header map[string]string
	// TruncateAfter, if positive, serves the normal response but closes the connection after this
	// many body bytes, so the client sees a short read against the advertised Content-Length.
	TruncateAfter int
	// Times is how many requests the fault applies to; zero or less means every matching request.
	Times int
}

// Request is one request the server saw.
type Request struct {
	Method      string
	Path        string // path only, no query
	Accept      string
	Auth        string // the Authorization header, verbatim
	IfNoneMatch string
	Origin      string // which root served it: "api", "web" or "cdn"
	Query       string // raw query string (the CDN's signed parameters live here)
	UserAgent   string
}

// Option configures a Server.
type Option func(*Server)

// WithClock sets the clock used for default publish times and rate-limit reset times.
func WithClock(c clock.Clock) Option { return func(s *Server) { s.clock = c } }

// WithCDN makes every asset download answer 302 to a second server (a different host:port), the
// way github.com redirects to release-assets.githubusercontent.com with a signed, expiring URL.
// Without it downloads are served directly.
func WithCDN() Option { return func(s *Server) { s.useCDN = true } }

type storedAsset struct {
	id          int64
	owner, repo string
	tag         string
	Asset
}

type storedRelease struct {
	id          int64
	owner, repo string
	Release
}

// Server is a running fake. Create one with New; it shuts down with the test.
type Server struct {
	tb    TB
	srv   *httptest.Server
	clock clock.Clock

	useCDN bool
	cdn    *httptest.Server

	mu             sync.Mutex
	releases       []*storedRelease
	assets         map[int64]*storedAsset
	nextID         int64
	rateLimited    bool
	quotaLimit     int // 0 = unlimited
	quotaRemaining int
	faults         []*Fault
	requests       []Request
}

// TB is the part of testing.TB this package needs. Taking an interface keeps the "testing"
// package, and its flags, out of any binary that imports this one by mistake.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// New starts a fake release server that is closed automatically when the test ends.
func New(tb TB, opts ...Option) *Server {
	tb.Helper()
	s := &Server{
		tb:     tb,
		clock:  clock.System{},
		assets: make(map[int64]*storedAsset),
		nextID: 1000,
	}
	for _, o := range opts {
		o(s)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/repos/{owner}/{repo}/releases/latest", s.apiLatest)
	mux.HandleFunc("GET /api/repos/{owner}/{repo}/releases/tags/{tag}", s.apiByTag)
	mux.HandleFunc("GET /api/repos/{owner}/{repo}/releases/assets/{id}", s.apiAsset)
	mux.HandleFunc("GET /api/repos/{owner}/{repo}/releases", s.apiList)
	mux.HandleFunc("GET /web/{owner}/{repo}/releases/latest", s.webLatest)
	mux.HandleFunc("GET /web/{owner}/{repo}/releases/tag/{tag}", s.webTag)
	mux.HandleFunc("GET /web/{owner}/{repo}/releases/download/{tag}/{name}", s.webDownload)
	mux.HandleFunc("GET /web/{owner}/{repo}/releases.atom", s.webAtom)

	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.serve(mux, w, r)
	}))
	tb.Cleanup(s.srv.Close)

	if s.useCDN {
		cdnMux := http.NewServeMux()
		cdnMux.HandleFunc("GET /cdn/{id}/{name}", s.cdnAsset)
		s.cdn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.serve(cdnMux, w, r)
		}))
		tb.Cleanup(s.cdn.Close)
	}
	return s
}

// APIBase is the root to use in place of https://api.github.com.
func (s *Server) APIBase() string { return s.srv.URL + "/api" }

// WebBase is the root to use in place of https://github.com.
func (s *Server) WebBase() string { return s.srv.URL + "/web" }

// CDNBase is the second server's root ("" unless WithCDN was given).
func (s *Server) CDNBase() string {
	if s.cdn == nil {
		return ""
	}
	return s.cdn.URL + "/cdn"
}

// Client returns an http.Client that talks to the fake (and honours its TLS-free transport).
func (s *Server) Client() *http.Client { return s.srv.Client() }

// AddRelease publishes a release. The tag must be unique within the repository.
func (s *Server) AddRelease(owner, repo string, r Release) {
	s.tb.Helper()
	if owner == "" || repo == "" || r.Tag == "" {
		s.tb.Fatalf("fakerelease: AddRelease needs owner, repo and tag (got %q, %q, %q)", owner, repo, r.Tag)
	}
	if r.Name == "" {
		r.Name = r.Tag
	}
	if r.PublishedAt.IsZero() {
		r.PublishedAt = s.clock.Now()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = r.PublishedAt
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.releases {
		if existing.owner == owner && existing.repo == repo && existing.Tag == r.Tag {
			s.tb.Fatalf("fakerelease: %s/%s already has a release tagged %s", owner, repo, r.Tag)
		}
	}
	s.nextID++
	stored := &storedRelease{id: s.nextID, owner: owner, repo: repo, Release: r}
	for _, a := range r.Assets {
		if a.Name == "" {
			s.tb.Fatalf("fakerelease: asset with no name in %s/%s %s", owner, repo, r.Tag)
		}
		s.nextID++
		s.assets[s.nextID] = &storedAsset{id: s.nextID, owner: owner, repo: repo, tag: r.Tag, Asset: a}
	}
	s.releases = append(s.releases, stored)
}

// SetRateLimited turns API rate limiting on or off. The website roots are unaffected, as on GitHub.
func (s *Server) SetRateLimited(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimited = on
}

// SetQuota simulates GitHub's unauthenticated primary rate limit: after limit API requests the
// API answers 403 with X-RateLimit-Remaining: 0. Every API response carries the X-RateLimit-*
// headers. As observed against the real service, a 304 Not Modified still costs one unit;
// requests that carry an Authorization header are not counted (they have their own, larger
// quota). The website roots are never counted. limit <= 0 turns the simulation off.
func (s *Server) SetQuota(limit int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit < 0 {
		limit = 0
	}
	s.quotaLimit, s.quotaRemaining = limit, limit
}

// QuotaRemaining reports the simulated remaining API requests (0 when the simulation is off).
func (s *Server) QuotaRemaining() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quotaRemaining
}

// Inject adds a fault. Faults are consulted in the order they were added.
func (s *Server) Inject(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := f
	s.faults = append(s.faults, &cp)
}

// ClearFaults removes every injected fault.
func (s *Server) ClearFaults() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = nil
}

// Requests returns every request seen so far, oldest first.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// RequestCount counts requests whose path contains the given text.
func (s *Server) RequestCount(pathContains string) int {
	n := 0
	for _, r := range s.Requests() {
		if strings.Contains(r.Path, pathContains) {
			n++
		}
	}
	return n
}

// ResetRequests forgets the request log.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

// AssetURL is the website download URL of an asset (a release's browser_download_url).
func (s *Server) AssetURL(owner, repo, tag, name string) string {
	return fmt.Sprintf("%s/%s/%s/releases/download/%s/%s", s.WebBase(), owner, repo, tag, name)
}

// serve wraps every request with logging, rate limiting and fault injection.
func (s *Server) serve(next http.Handler, w http.ResponseWriter, r *http.Request) {
	origin := "web"
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/"):
		origin = "api"
	case strings.HasPrefix(r.URL.Path, "/cdn/"):
		origin = "cdn"
	}

	s.mu.Lock()
	s.requests = append(s.requests, Request{
		Method:      r.Method,
		Path:        r.URL.Path,
		Accept:      r.Header.Get("Accept"),
		Auth:        r.Header.Get("Authorization"),
		IfNoneMatch: r.Header.Get("If-None-Match"),
		Origin:      origin,
		Query:       r.URL.RawQuery,
		UserAgent:   r.Header.Get("User-Agent"),
	})
	limited := s.rateLimited && origin == "api"
	remaining, limit := -1, 0
	if origin == "api" && s.quotaLimit > 0 {
		limit = s.quotaLimit
		if r.Header.Get("Authorization") == "" {
			if s.quotaRemaining == 0 {
				limited = true
			} else {
				s.quotaRemaining--
			}
		}
		remaining = s.quotaRemaining
	}
	fault := s.takeFault(r.URL.Path)
	s.mu.Unlock()

	if origin == "api" && limit > 0 {
		reset := s.clock.Now().Add(time.Hour)
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	}

	if limited {
		reset := s.clock.Now().Add(time.Hour)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if limit == 0 {
			w.Header().Set("X-RateLimit-Limit", "60")
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message":           "API rate limit exceeded for 203.0.113.9. (But here's the good news: Authenticated requests get a higher rate limit.)",
			"documentation_url": "https://docs.github.com/rest/overview/resources-in-the-rest-api#rate-limiting",
		})
		return
	}

	if fault != nil && fault.Status != 0 {
		for k, v := range fault.Header {
			w.Header().Set(k, v)
		}
		http.Error(w, "injected fault", fault.Status)
		return
	}
	if fault != nil && fault.TruncateAfter > 0 {
		w = &truncatingWriter{ResponseWriter: w, remaining: fault.TruncateAfter}
	}
	next.ServeHTTP(w, r)
}

// takeFault returns the first matching fault and counts one use of it. Caller holds s.mu.
func (s *Server) takeFault(path string) *Fault {
	for i, f := range s.faults {
		if !strings.Contains(path, f.PathContains) {
			continue
		}
		used := *f
		if f.Times > 0 {
			f.Times--
			if f.Times == 0 {
				s.faults = append(s.faults[:i], s.faults[i+1:]...)
			}
		}
		return &used
	}
	return nil
}

// truncatingWriter lets the handler declare the full Content-Length, passes through the first
// `remaining` body bytes, then aborts the connection.
type truncatingWriter struct {
	http.ResponseWriter
	remaining int
}

func (t *truncatingWriter) Write(p []byte) (int, error) {
	if len(p) <= t.remaining {
		t.remaining -= len(p)
		return t.ResponseWriter.Write(p)
	}
	if t.remaining > 0 {
		_, _ = t.ResponseWriter.Write(p[:t.remaining])
		t.remaining = 0
	}
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	panic(http.ErrAbortHandler)
}

// ---- REST API ----

type apiAsset struct {
	ID                 int64     `json:"id"`
	Name               string    `json:"name"`
	ContentType        string    `json:"content_type"`
	State              string    `json:"state"`
	Size               int       `json:"size"`
	Digest             string    `json:"digest"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	URL                string    `json:"url"`
	BrowserDownloadURL string    `json:"browser_download_url"`
}

type apiRelease struct {
	ID          int64      `json:"id"`
	TagName     string     `json:"tag_name"`
	Name        string     `json:"name"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
	CreatedAt   time.Time  `json:"created_at"`
	PublishedAt time.Time  `json:"published_at"`
	HTMLURL     string     `json:"html_url"`
	Assets      []apiAsset `json:"assets"`
}

// visible returns the repository's non-draft releases, newest first by creation time as GitHub
// does (ties: published later, then added later). Caller holds s.mu.
func (s *Server) visible(owner, repo string) []*storedRelease {
	var out []*storedRelease
	for _, r := range s.releases {
		if r.owner == owner && r.repo == repo && !r.Draft {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		switch {
		case !out[i].CreatedAt.Equal(out[j].CreatedAt):
			return out[i].CreatedAt.After(out[j].CreatedAt)
		case !out[i].PublishedAt.Equal(out[j].PublishedAt):
			return out[i].PublishedAt.After(out[j].PublishedAt)
		}
		return out[i].id > out[j].id
	})
	return out
}

func (s *Server) toAPI(r *storedRelease) apiRelease {
	out := apiRelease{
		ID:          r.id,
		TagName:     r.Tag,
		Name:        r.Name,
		Draft:       r.Draft,
		Prerelease:  r.Prerelease,
		CreatedAt:   r.CreatedAt.UTC(),
		PublishedAt: r.PublishedAt.UTC(),
		HTMLURL:     fmt.Sprintf("%s/%s/%s/releases/tag/%s", s.WebBase(), r.owner, r.repo, r.Tag),
		Assets:      []apiAsset{},
	}
	var ids []int64
	for id, a := range s.assets {
		if a.owner == r.owner && a.repo == r.repo && a.tag == r.Tag {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		a := s.assets[id]
		ct := a.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		sum := sha256.Sum256(a.Data)
		out.Assets = append(out.Assets, apiAsset{
			ID:                 id,
			Name:               a.Name,
			ContentType:        ct,
			State:              "uploaded",
			Size:               len(a.Data),
			Digest:             "sha256:" + hex.EncodeToString(sum[:]),
			CreatedAt:          r.CreatedAt.UTC(),
			UpdatedAt:          r.PublishedAt.UTC(),
			URL:                fmt.Sprintf("%s/repos/%s/%s/releases/assets/%d", s.APIBase(), a.owner, a.repo, id),
			BrowserDownloadURL: s.AssetURL(a.owner, a.repo, a.tag, a.Name),
		})
	}
	return out
}

// writeJSON answers with v, adds a weak ETag, and honours If-None-Match with a 304 (which, like
// the real service, still counted against the quota in serve()).
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if status == http.StatusOK {
		sum := sha256.Sum256(body)
		etag := `W/"` + hex.EncodeToString(sum[:16]) + `"`
		w.Header().Set("ETag", etag)
		if r != nil && r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func notFound(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func (s *Server) apiLatest(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rel := range s.visible(r.PathValue("owner"), r.PathValue("repo")) {
		if !rel.Prerelease {
			writeJSON(w, r, http.StatusOK, s.toAPI(rel))
			return
		}
	}
	notFound(w, r)
}

func (s *Server) apiList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []apiRelease{}
	for _, rel := range s.visible(r.PathValue("owner"), r.PathValue("repo")) {
		out = append(out, s.toAPI(rel))
	}
	writeJSON(w, r, http.StatusOK, out)
}

func (s *Server) apiByTag(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rel := range s.visible(r.PathValue("owner"), r.PathValue("repo")) {
		if rel.Tag == r.PathValue("tag") {
			writeJSON(w, r, http.StatusOK, s.toAPI(rel))
			return
		}
	}
	notFound(w, r)
}

func (s *Server) apiAsset(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w, r)
		return
	}
	s.mu.Lock()
	a := s.assets[id]
	s.mu.Unlock()
	if a == nil || a.owner != r.PathValue("owner") || a.repo != r.PathValue("repo") {
		notFound(w, r)
		return
	}
	s.deliver(w, r, a)
}

// deliver serves an asset directly, or redirects to the CDN root when WithCDN is on. The
// redirect carries credential-looking query parameters so that redaction has something to bite.
func (s *Server) deliver(w http.ResponseWriter, r *http.Request, a *storedAsset) {
	if s.cdn != nil {
		loc := fmt.Sprintf("%s/%d/%s?sp=r&sv=2018-11-09&sig=FAKESIG%d&jwt=eyJFAKE.JWT.%d", s.CDNBase(), a.id, a.Name, a.id, a.id)
		http.Redirect(w, r, loc, http.StatusFound)
		return
	}
	serveBytes(w, a.Asset)
}

func (s *Server) cdnAsset(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	a := s.assets[id]
	s.mu.Unlock()
	if a == nil || a.Name != r.PathValue("name") || !strings.HasPrefix(r.URL.Query().Get("sig"), "FAKESIG") {
		http.NotFound(w, r)
		return
	}
	serveBytes(w, a.Asset)
}

// ---- website ----

func (s *Server) webLatest(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	var tag string
	for _, rel := range s.visible(r.PathValue("owner"), r.PathValue("repo")) {
		if !rel.Prerelease {
			tag = rel.Tag
			break
		}
	}
	s.mu.Unlock()
	if tag == "" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s/%s/%s/releases/tag/%s", s.WebBase(), r.PathValue("owner"), r.PathValue("repo"), tag), http.StatusFound)
}

func (s *Server) webTag(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rel := range s.visible(r.PathValue("owner"), r.PathValue("repo")) {
		if rel.Tag == r.PathValue("tag") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprintf(w, "<!doctype html><title>%s</title><h1>%s</h1>\n", rel.Name, rel.Name)
			return
		}
	}
	http.NotFound(w, r)
}

func (s *Server) webDownload(w http.ResponseWriter, r *http.Request) {
	owner, repo, tag, name := r.PathValue("owner"), r.PathValue("repo"), r.PathValue("tag"), r.PathValue("name")
	s.mu.Lock()
	var found *storedAsset
	for _, rel := range s.visible(owner, repo) {
		if rel.Tag != tag {
			continue
		}
		for _, a := range s.assets {
			if a.owner == owner && a.repo == repo && a.tag == tag && a.Name == name {
				found = a
			}
		}
	}
	s.mu.Unlock()
	if found == nil {
		http.NotFound(w, r)
		return
	}
	s.deliver(w, r, found)
}

// atomFeed is the shape of github.com/{owner}/{repo}/releases.atom (the parts clients read).
type atomFeed struct {
	XMLName xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	ID      string      `xml:"id"`
	Title   string      `xml:"title"`
	Updated string      `xml:"updated"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID      string   `xml:"id"`
	Updated string   `xml:"updated"`
	Link    atomLink `xml:"link"`
	Title   string   `xml:"title"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
	Href string `xml:"href,attr"`
}

// webAtom serves the release feed: every non-draft release, prereleases included, newest first.
// As on GitHub the entry's <updated> is the publish time and nothing says "prerelease".
func (s *Server) webAtom(w http.ResponseWriter, r *http.Request) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	s.mu.Lock()
	rels := s.visible(owner, repo)
	feed := atomFeed{
		ID:      fmt.Sprintf("tag:github.com,2008:%s/%s/%s/releases", s.WebBase(), owner, repo),
		Title:   "Release notes from " + repo,
		Updated: s.clock.Now().UTC().Format(time.RFC3339),
	}
	if len(rels) > 0 {
		feed.Updated = rels[0].CreatedAt.UTC().Format(time.RFC3339)
	}
	for _, rel := range rels {
		feed.Entries = append(feed.Entries, atomEntry{
			ID:      fmt.Sprintf("tag:github.com,2008:Repository/%d/%s", rel.id, rel.Tag),
			Updated: rel.PublishedAt.UTC().Format(time.RFC3339),
			Link: atomLink{Rel: "alternate", Type: "text/html",
				Href: fmt.Sprintf("%s/%s/%s/releases/tag/%s", s.WebBase(), owner, repo, rel.Tag)},
			Title: rel.Name,
		})
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
	_, _ = io.WriteString(w, xml.Header)
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(feed)
}

func serveBytes(w http.ResponseWriter, a Asset) {
	ct := a.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(a.Data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(a.Data)
}
