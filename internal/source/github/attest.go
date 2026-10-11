package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/enls/gremloader/internal/download"
)

// Bundle is one attestation bundle as GitHub serves it (ADR 0014).
type Bundle struct {
	Data []byte
	// Snappy is true when Data came from bundle_url (snappy's block format); otherwise Data is the
	// bundle's JSON, sent inline.
	Snappy bool
}

// maxAttestations is how many attestations of one asset are read: one page. Real assets have one
// or two (build provenance, GitHub's release attestation).
const maxAttestations = 30

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

type apiAttestations struct {
	Attestations []struct {
		Bundle    json.RawMessage `json:"bundle"`
		BundleURL string          `json:"bundle_url"`
	} `json:"attestations"`
}

// Attestations returns the bundles GitHub lists for the asset of repo whose SHA-256 is sha256hex;
// none published is (nil, nil). The list comes from the API through the Source's client (a token,
// when set, goes to the API only). A bundle behind bundle_url is fetched with bundles, a client
// under download.BundlePolicy, capped at maxBundle bytes. Nothing here checks a bundle: that is
// the attest package's job.
func (s *Source) Attestations(ctx context.Context, repo, sha256hex string, bundles *download.Client, maxBundle int64) ([]Bundle, error) {
	if !ValidRepo(repo) {
		return nil, fmt.Errorf("%q is not owner/name", repo)
	}
	if !sha256Hex.MatchString(sha256hex) {
		return nil, fmt.Errorf("%q is not a lower-case SHA-256", sha256hex)
	}
	resp, err := s.c.Get(ctx, download.Request{
		URL: fmt.Sprintf("%s/repos/%s/attestations/sha256:%s?per_page=%d", s.api, repo, sha256hex, maxAttestations),
		Header: http.Header{
			"Accept":               {"application/vnd.github+json"},
			"X-GitHub-Api-Version": {"2022-11-28"},
		},
		MaxBytes: maxAPIBytes,
	})
	if err != nil {
		var se *download.StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	var list apiAttestations
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		return nil, fmt.Errorf("GitHub API answer for the attestations of %s is not an attestation list: %w", repo, err)
	}
	var out []Bundle
	for _, a := range list.Attestations {
		if b := bytes.TrimSpace(a.Bundle); len(b) > 0 && !bytes.Equal(b, []byte("null")) {
			out = append(out, Bundle{Data: b})
			continue
		}
		if a.BundleURL == "" {
			continue
		}
		if bundles == nil {
			return nil, errors.New("an attestation bundle must be fetched, and no bundle client was given")
		}
		got, err := bundles.Get(ctx, download.Request{URL: a.BundleURL, MaxBytes: maxBundle})
		if err != nil {
			return nil, fmt.Errorf("attestation bundle: %w", err)
		}
		out = append(out, Bundle{Data: got.Body, Snappy: true})
	}
	return out, nil
}
