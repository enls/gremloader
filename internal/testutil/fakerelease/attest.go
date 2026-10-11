package fakerelease

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/golang/snappy"
)

// Attestation is one item of an asset's attestation list, as GitHub's API serves it (ADR 0014).
type Attestation struct {
	// Bundle is the bundle's JSON. It is served behind bundle_url, snappy-compressed (as GitHub
	// does today), unless Inline is set.
	Bundle []byte
	Inline bool
	// Served, if set, is what bundle_url serves instead, byte for byte (already compressed, or
	// broken on purpose).
	Served []byte
}

type storedAttestation struct {
	id int64
	Attestation
}

// AddAttestation lists a for the asset of owner/repo whose SHA-256 is sha256hex (lower case).
func (s *Server) AddAttestation(owner, repo, sha256hex string, a Attestation) {
	s.tb.Helper()
	if len(sha256hex) != 64 || strings.ToLower(sha256hex) != sha256hex {
		s.tb.Fatalf("fakerelease: AddAttestation needs a lower-case SHA-256, got %q", sha256hex)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	key := owner + "/" + repo + "@" + sha256hex
	s.attestations[key] = append(s.attestations[key], &storedAttestation{id: s.nextID, Attestation: a})
	s.blobs[s.nextID] = s.attestations[key][len(s.attestations[key])-1]
}

// BlobBase is the root of the bundle store: a second server, like GitHub's Azure storage.
func (s *Server) BlobBase() string { return s.blob.URL + "/blob" }

type apiAttestation struct {
	RepositoryID int64           `json:"repository_id"`
	Bundle       json.RawMessage `json:"bundle"`
	BundleURL    string          `json:"bundle_url,omitempty"`
	Initiator    string          `json:"initiator"`
}

func (s *Server) apiAttestations(w http.ResponseWriter, r *http.Request) {
	sum, ok := strings.CutPrefix(r.PathValue("digest"), "sha256:")
	if !ok {
		notFound(w, r)
		return
	}
	s.mu.Lock()
	list := s.attestations[r.PathValue("owner")+"/"+r.PathValue("repo")+"@"+sum]
	s.mu.Unlock()
	if len(list) == 0 {
		notFound(w, r)
		return
	}
	out := struct {
		Attestations []apiAttestation `json:"attestations"`
	}{}
	for _, a := range list {
		item := apiAttestation{RepositoryID: 1, Bundle: json.RawMessage("null"), Initiator: "user"}
		if a.Inline {
			item.Bundle = json.RawMessage(a.Bundle)
		} else {
			// The shape of a real one: an Azure SAS URL, signed for about an hour.
			item.BundleURL = fmt.Sprintf("%s/%d.json.sn?se=2026-10-11T21%%3A06%%3A44Z&sig=FAKESIG%d&sp=r&sr=b", s.BlobBase(), a.id, a.id)
		}
		out.Attestations = append(out.Attestations, item)
	}
	writeJSON(w, r, http.StatusOK, out)
}

func (s *Server) blobBundle(w http.ResponseWriter, r *http.Request) {
	idText, ok := strings.CutSuffix(r.PathValue("file"), ".json.sn")
	id, err := strconv.ParseInt(idText, 10, 64)
	s.mu.Lock()
	a := s.blobs[id]
	s.mu.Unlock()
	if !ok || err != nil || a == nil || a.Inline || r.URL.Query().Get("sig") != "FAKESIG"+idText {
		http.Error(w, "BlobNotFound", http.StatusNotFound)
		return
	}
	body := a.Served
	if body == nil {
		body = snappy.Encode(nil, a.Bundle)
	}
	w.Header().Set("Content-Type", "application/x-snappy")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}
