package github_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang/snappy"

	"github.com/enls/gremloader/internal/download"
	"github.com/enls/gremloader/internal/testutil/fakerelease"
)

const assetSHA = "ae64e556ecc240b200f7eba60d550e4bb60d78e860e69dd88c449405b86067f4"

// bundleClient stands in for a client under download.BundlePolicy: the fake's hosts, and a token
// that may go to the API's host and port only (so a request to the bundle store must not carry it).
func bundleClient(t *testing.T, e *env, hosts ...string) *download.Client {
	t.Helper()
	if hosts == nil {
		hosts = []string{"127.0.0.1"}
	}
	return download.New(download.Config{
		Policy: download.Policy{Hosts: hosts, TokenHosts: []string{apiHost(t, e)}, AllowHTTP: true},
		Token:  "SECRET-TOKEN",
		Sleep:  func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		Jitter: func(time.Duration) time.Duration { return 0 },
	})
}

func TestAttestationsNone(t *testing.T) {
	e := newEnv(t, "SECRET-TOKEN")
	got, err := e.src.Attestations(context.Background(), "o/r", assetSHA, bundleClient(t, e), 1<<20)
	if err != nil || got != nil {
		t.Fatalf("%v %v", got, err)
	}
	reqs := e.srv.Requests()
	if len(reqs) != 1 || reqs[0].Origin != "api" || reqs[0].Path != "/api/repos/o/r/attestations/sha256:"+assetSHA || reqs[0].Auth == "" {
		t.Errorf("%+v", reqs)
	}
}

func TestAttestationsURLAndInline(t *testing.T) {
	e := newEnv(t, "SECRET-TOKEN")
	e.srv.AddAttestation("o", "r", assetSHA, fakerelease.Attestation{Bundle: []byte(`{"from":"url"}`)})
	e.srv.AddAttestation("o", "r", assetSHA, fakerelease.Attestation{Bundle: []byte(`{"from":"inline"}`), Inline: true})
	got, err := e.src.Attestations(context.Background(), "o/r", assetSHA, bundleClient(t, e), 1<<20)
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	if dec, err := snappy.Decode(nil, got[0].Data); !got[0].Snappy || err != nil || string(dec) != `{"from":"url"}` {
		t.Errorf("url bundle: %+v %v", got[0], err)
	}
	if got[1].Snappy || string(got[1].Data) != `{"from":"inline"}` {
		t.Errorf("inline bundle: %+v", got[1])
	}
	for _, r := range e.srv.Requests() {
		if r.Origin == "blob" && r.Auth != "" {
			t.Errorf("the token went to the bundle store: %+v", r)
		}
	}
}

func TestAttestationsErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("bad arguments send nothing", func(t *testing.T) {
		e := newEnv(t, "")
		for _, args := range [][2]string{{"o", assetSHA}, {"o/r/x", assetSHA}, {"o/r", strings.ToUpper(assetSHA)}, {"o/r", assetSHA[:63]}, {"o/r", ""}} {
			if _, err := e.src.Attestations(ctx, args[0], args[1], bundleClient(t, e), 1<<20); err == nil {
				t.Errorf("%v: no error", args)
			}
		}
		if n := len(e.srv.Requests()); n != 0 {
			t.Errorf("%d requests", n)
		}
	})
	t.Run("API failure", func(t *testing.T) {
		e := newEnv(t, "")
		e.srv.Inject(fakerelease.Fault{PathContains: "/attestations/", Status: http.StatusInternalServerError})
		if got, err := e.src.Attestations(ctx, "o/r", assetSHA, bundleClient(t, e), 1<<20); err == nil || got != nil {
			t.Errorf("%v %v", got, err)
		}
	})
	t.Run("bundle store failure", func(t *testing.T) {
		e := newEnv(t, "")
		e.srv.AddAttestation("o", "r", assetSHA, fakerelease.Attestation{Bundle: []byte(`{}`)})
		e.srv.Inject(fakerelease.Fault{PathContains: "/blob/", Status: http.StatusServiceUnavailable})
		if got, err := e.src.Attestations(ctx, "o/r", assetSHA, bundleClient(t, e), 1<<20); err == nil || got != nil {
			t.Errorf("%v %v", got, err)
		}
	})
	t.Run("no bundle client", func(t *testing.T) {
		e := newEnv(t, "")
		e.srv.AddAttestation("o", "r", assetSHA, fakerelease.Attestation{Bundle: []byte(`{}`)})
		if _, err := e.src.Attestations(ctx, "o/r", assetSHA, nil, 1<<20); err == nil {
			t.Error("no error")
		}
	})
	t.Run("bundle host not allowed", func(t *testing.T) {
		e := newEnv(t, "")
		e.srv.AddAttestation("o", "r", assetSHA, fakerelease.Attestation{Bundle: []byte(`{}`)})
		_, err := e.src.Attestations(ctx, "o/r", assetSHA, bundleClient(t, e, "example.com"), 1<<20)
		var pe *download.PolicyError
		if !errors.As(err, &pe) {
			t.Errorf("%v", err)
		}
		for _, r := range e.srv.Requests() {
			if r.Origin == "blob" {
				t.Errorf("the bundle store was reached: %+v", r)
			}
		}
	})
	t.Run("bundle over the cap", func(t *testing.T) {
		e := newEnv(t, "")
		e.srv.AddAttestation("o", "r", assetSHA, fakerelease.Attestation{Served: bytes.Repeat([]byte("x"), 2048)})
		if _, err := e.src.Attestations(ctx, "o/r", assetSHA, bundleClient(t, e), 1024); !errors.Is(err, download.ErrTooLarge) {
			t.Errorf("%v", err)
		}
	})
}

func apiHost(t *testing.T, e *env) string {
	t.Helper()
	u, err := url.Parse(e.srv.APIBase())
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}
