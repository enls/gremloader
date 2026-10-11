package sigstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/attest"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Without the network and without a cache there is no root: unreachable, not a crash or a hang.
func TestTrustedRootOffline(t *testing.T) {
	calls := 0
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("offline")
	})}
	for _, dir := range []string{filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		if _, err := TrustedRoot(context.Background(), dir, hc); !errors.Is(err, attest.ErrUnreachable) {
			t.Errorf("%s: %v", dir, err)
		}
	}
	if calls == 0 {
		t.Error("the TUF client never tried the network")
	}
}

// The fetch reaches only TUFHost over https, redirects included.
func TestTrustedRootReachesOnlyTUFHost(t *testing.T) {
	for _, raw := range []string{"http://" + TUFHost + "/x", "https://evil.example/x", "https://" + TUFHost + ":8443/x",
		"https://" + TUFHost + ".evil.example/x"} {
		req := httptest.NewRequest(http.MethodGet, raw, nil)
		if err := allowedTUF(req); err == nil {
			t.Errorf("%s allowed", raw)
		}
	}
	if err := allowedTUF(httptest.NewRequest(http.MethodGet, "https://"+strings.ToUpper(TUFHost)+"/10.root.json", nil)); err != nil {
		t.Error(err)
	}

	var reached []string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reached = append(reached, r.URL.String())
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://evil.example/root.json"}},
			Body: http.NoBody, Request: r}, nil
	})
	c := onlyTUF(context.Background(), &http.Client{Transport: base})
	resp, err := c.Get("https://" + TUFHost + "/2.root.json")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a redirect off the TUF host was followed")
	}
	if len(reached) != 1 {
		t.Errorf("reached %v", reached)
	}
	resp, err = onlyTUF(context.Background(), &http.Client{Transport: base}).Get("https://evil.example/")
	if err == nil {
		_ = resp.Body.Close()
	}
	if err == nil || len(reached) != 1 {
		t.Errorf("a request off the TUF host went out: %v %v", err, reached)
	}
}

// GREMLOADER_LIVE=1 fetches the real root and checks the cli/cli provenance with it. Not run in CI.
func TestTrustedRootLive(t *testing.T) {
	if os.Getenv("GREMLOADER_LIVE") != "1" {
		t.Skip("set GREMLOADER_LIVE=1 to fetch Sigstore's trust root over the network")
	}
	dir := t.TempDir()
	tr, err := TrustedRoot(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(tr)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := v.Check([][]byte{provenance(t)}, zipSHA256, attest.Policy{Repo: "cli/cli"}); err != nil || r.Outcome != attest.Passed {
		t.Fatalf("%+v %v", r, err)
	}
	// A second fetch inside the day is served from the cache, with no network at all.
	offline := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })}
	if _, err := TrustedRoot(context.Background(), dir, offline); err != nil {
		t.Errorf("cached root: %v", err)
	}
}

// A redirect loop on the TUF host itself ends after a few hops instead of running forever.
func TestTrustedRootRedirectLoopEnds(t *testing.T) {
	n := 0
	loop := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://" + TUFHost + "/again"}},
			Body: http.NoBody, Request: r}, nil
	})
	resp, err := onlyTUF(context.Background(), &http.Client{Transport: loop}).Get("https://" + TUFHost + "/1.root.json")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("no error")
	}
	if n > 6 {
		t.Errorf("%d requests", n)
	}
}
