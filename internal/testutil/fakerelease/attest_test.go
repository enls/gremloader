package fakerelease

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/golang/snappy"
)

const someSHA = "ae64e556ecc240b200f7eba60d550e4bb60d78e860e69dd88c449405b86067f4"

func TestAttestationsListAndBundleURL(t *testing.T) {
	s := New(t)
	s.AddAttestation("o", "r", someSHA, Attestation{Bundle: []byte(`{"b":1}`)})
	s.AddAttestation("o", "r", someSHA, Attestation{Bundle: []byte(`{"b":2}`), Inline: true})

	resp, body := rawGet(t, s.APIBase()+"/repos/o/r/attestations/sha256:"+someSHA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}
	var list struct {
		Attestations []struct {
			Bundle    json.RawMessage `json:"bundle"`
			BundleURL string          `json:"bundle_url"`
		} `json:"attestations"`
	}
	if err := json.Unmarshal(body, &list); err != nil || len(list.Attestations) != 2 {
		t.Fatalf("%s %v", body, err)
	}
	first, second := list.Attestations[0], list.Attestations[1]
	if string(first.Bundle) != "null" || !strings.HasPrefix(first.BundleURL, s.BlobBase()+"/") || !strings.Contains(first.BundleURL, "sig=FAKESIG") {
		t.Errorf("first: %+v", first)
	}
	if string(second.Bundle) != `{"b":2}` || second.BundleURL != "" {
		t.Errorf("second: %+v", second)
	}
	// bundle_url is on another server, like GitHub's Azure storage, and serves snappy.
	if u, _ := url.Parse(first.BundleURL); u.Host == strings.TrimPrefix(s.srv.URL, "http://") {
		t.Errorf("bundle_url on the API's host: %s", first.BundleURL)
	}
	resp, body = rawGet(t, first.BundleURL)
	if dec, err := snappy.Decode(nil, body); resp.StatusCode != http.StatusOK || err != nil || !bytes.Equal(dec, []byte(`{"b":1}`)) {
		t.Errorf("bundle: %d %q %v", resp.StatusCode, body, err)
	}
	if reqs := s.Requests(); reqs[len(reqs)-1].Origin != "blob" {
		t.Errorf("origin %q", reqs[len(reqs)-1].Origin)
	}
	// Without its signature, or for an inline one, the store answers 404.
	if resp, _ := rawGet(t, strings.Replace(first.BundleURL, "sig=FAKESIG", "sig=X", 1)); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unsigned: %d", resp.StatusCode)
	}
}

func TestAttestationsNoneIs404(t *testing.T) {
	s := New(t)
	s.AddAttestation("o", "r", someSHA, Attestation{Bundle: []byte(`{}`)})
	for _, path := range []string{"/repos/o/other/attestations/sha256:" + someSHA, "/repos/o/r/attestations/sha256:" + strings.Repeat("0", 64),
		"/repos/o/r/attestations/" + someSHA} {
		if resp, _ := rawGet(t, s.APIBase()+path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
}

func TestAttestationServedAsGiven(t *testing.T) {
	s := New(t)
	s.AddAttestation("o", "r", someSHA, Attestation{Served: []byte("not snappy")})
	_, body := rawGet(t, s.APIBase()+"/repos/o/r/attestations/sha256:"+someSHA)
	var list struct {
		Attestations []struct {
			BundleURL string `json:"bundle_url"`
		} `json:"attestations"`
	}
	if err := json.Unmarshal(body, &list); err != nil || len(list.Attestations) != 1 {
		t.Fatalf("%s %v", body, err)
	}
	if _, got := rawGet(t, list.Attestations[0].BundleURL); string(got) != "not snappy" {
		t.Errorf("%q", got)
	}
}
