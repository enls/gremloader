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
	"encoding/json"
	"fmt"
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
	Assets      []Asset
}

// Fault makes matching requests misbehave. It applies to both roots.
type Fault struct {
	// PathContains selects requests whose URL path contains this text.
	PathContains string
	// Status, if non-zero, is returned instead of the normal response.
	Status int
	// TruncateAfter, if positive, serves the normal response but closes the connection after this
	// many body bytes, so the client sees a short read against the advertised Content-Length.
	TruncateAfter int
	// Times is how many requests the fault applies to; zero or less means every matching request.
	Times int
}

// Request is one request the server saw.
type Request struct {
	Method string
	Path   string // path only, no query
	Accept string
	Auth   string // the Authorization header, verbatim
}

// Option configures a Server.
type Option func(*Server)

// WithClock sets the clock used for default publish times and rate-limit reset times.
func WithClock(c clock.Clock) Option { return func(s *Server) { s.clock = c } }

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

	mu          sync.Mutex
	releases    []*storedRelease
	assets      map[int64]*storedAsset
	nextID      int64
	rateLimited bool
	faults      []*Fault
	requests    []Request
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

	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.serve(mux, w, r)
	}))
	tb.Cleanup(s.srv.Close)
	return s
}

// APIBase is the root to use in place of https://api.github.com.
func (s *Server) APIBase() string { return s.srv.URL + "/api" }

// WebBase is the root to use in place of https://github.com.
func (s *Server) WebBase() string { return s.srv.URL + "/web" }

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
	s.mu.Lock()
	s.requests = append(s.requests, Request{
		Method: r.Method,
		Path:   r.URL.Path,
		Accept: r.Header.Get("Accept"),
		Auth:   r.Header.Get("Authorization"),
	})
	limited := s.rateLimited && strings.HasPrefix(r.URL.Path, "/api/")
	fault := s.takeFault(r.URL.Path)
	s.mu.Unlock()

	if limited {
		reset := s.clock.Now().Add(time.Hour)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-RateLimit-Limit", "60")
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
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	ContentType        string `json:"content_type"`
	Size               int    `json:"size"`
	URL                string `json:"url"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type apiRelease struct {
	ID          int64      `json:"id"`
	TagName     string     `json:"tag_name"`
	Name        string     `json:"name"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
	PublishedAt time.Time  `json:"published_at"`
	HTMLURL     string     `json:"html_url"`
	Assets      []apiAsset `json:"assets"`
}

// visible returns the repository's non-draft releases, newest first (ties: added later first).
// Caller holds s.mu.
func (s *Server) visible(owner, repo string) []*storedRelease {
	var out []*storedRelease
	for _, r := range s.releases {
		if r.owner == owner && r.repo == repo && !r.Draft {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].PublishedAt.Equal(out[j].PublishedAt) {
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
		out.Assets = append(out.Assets, apiAsset{
			ID:                 id,
			Name:               a.Name,
			ContentType:        ct,
			Size:               len(a.Data),
			URL:                fmt.Sprintf("%s/repos/%s/%s/releases/assets/%d", s.APIBase(), a.owner, a.repo, id),
			BrowserDownloadURL: s.AssetURL(a.owner, a.repo, a.tag, a.Name),
		})
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func (s *Server) apiLatest(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rel := range s.visible(r.PathValue("owner"), r.PathValue("repo")) {
		if !rel.Prerelease {
			writeJSON(w, http.StatusOK, s.toAPI(rel))
			return
		}
	}
	notFound(w)
}

func (s *Server) apiList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []apiRelease{}
	for _, rel := range s.visible(r.PathValue("owner"), r.PathValue("repo")) {
		out = append(out, s.toAPI(rel))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiByTag(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rel := range s.visible(r.PathValue("owner"), r.PathValue("repo")) {
		if rel.Tag == r.PathValue("tag") {
			writeJSON(w, http.StatusOK, s.toAPI(rel))
			return
		}
	}
	notFound(w)
}

func (s *Server) apiAsset(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w)
		return
	}
	s.mu.Lock()
	a := s.assets[id]
	s.mu.Unlock()
	if a == nil || a.owner != r.PathValue("owner") || a.repo != r.PathValue("repo") {
		notFound(w)
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
	serveBytes(w, found.Asset)
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
