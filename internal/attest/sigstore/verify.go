// Package sigstore checks GitHub artifact attestations with sigstore-go (ADR 0014): a
// build-provenance bundle, signed through public-good Sigstore by a workflow of the package's own
// repository, for the exact bytes that were downloaded. It is the only package that imports
// sigstore-go (ADR 0006); the engine sees only the types in package attest.
//
// A bundle of another kind (GitHub's release attestation, an SBOM, one signed by GitHub's own
// Sigstore instance for a private repository) is skipped, not failed: it says nothing about how
// the asset was built. Skipping cannot let a forged bundle through; at worst the asset looks
// unattested, which the engine's regression rule catches.
package sigstore

import (
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/golang/snappy"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/enls/gremloader/internal/attest"
)

const (
	// publicGoodOrg is the organization of public-good Fulcio's certificates; GitHub's own
	// instance says "GitHub, Inc.".
	publicGoodOrg = "sigstore.dev"

	// MaxBundle is the largest compressed bundle read; maxDecoded the largest it may expand to.
	// Real ones are a few KiB to 12 KiB.
	MaxBundle  = 2 << 20
	maxDecoded = 16 << 20
)

// Verifier checks bundles against one trusted root.
type Verifier struct {
	v *verify.Verifier
}

// NewVerifier returns a verifier for public-good Sigstore: one signed certificate timestamp, one
// transparency-log entry and one observer timestamp, as `gh attestation verify` requires there.
func NewVerifier(tm root.TrustedMaterial) (*Verifier, error) {
	v, err := verify.NewVerifier(tm, verify.WithSignedCertificateTimestamps(1), verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1))
	if err != nil {
		return nil, err
	}
	return &Verifier{v: v}, nil
}

// Check verifies the bundles (decoded JSON) published for an asset whose SHA-256 is sha256hex.
// The error is for a policy or digest that cannot be used; what the bundles amount to is in the
// Result.
func (v *Verifier) Check(bundles [][]byte, sha256hex string, p attest.Policy) (attest.Result, error) {
	if err := p.Validate(); err != nil {
		return attest.Result{}, err
	}
	digest, err := hex.DecodeString(sha256hex)
	if err != nil || len(digest) != 32 {
		return attest.Result{}, fmt.Errorf("%q is not a SHA-256", sha256hex)
	}
	san, err := verify.NewSANMatcher("", p.SANPattern())
	if err != nil {
		return attest.Result{}, err
	}
	issuer, err := verify.NewIssuerMatcher(attest.GitHubIssuer, "")
	if err != nil {
		return attest.Result{}, err
	}
	id, err := verify.NewCertificateIdentity(san, issuer, certificate.Extensions{})
	if err != nil {
		return attest.Result{}, err
	}
	policy := verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(id))

	var r attest.Result
	for i, raw := range bundles {
		signer, skip, fail := v.one(raw, policy, p)
		switch {
		case skip != "":
			r.Skipped = append(r.Skipped, fmt.Sprintf("bundle %d: %s", i+1, skip))
		case fail != "":
			r.Failures = append(r.Failures, fmt.Sprintf("bundle %d: %s", i+1, fail))
		case r.Outcome != attest.Passed:
			r.Outcome, r.Workflow = attest.Passed, signer
		}
	}
	if r.Outcome != attest.Passed && len(r.Failures) > 0 {
		r.Outcome = attest.Failed
	}
	return r, nil
}

// one checks one bundle: the signer when it passes, else why it was skipped or failed. What
// decides "skip" is read before verification, which is safe: a forged value can only make the
// bundle be ignored.
func (v *Verifier) one(raw []byte, policy verify.PolicyBuilder, p attest.Policy) (signer, skip, fail string) {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(raw); err != nil {
		return "", "unreadable: " + err.Error(), ""
	}
	env, err := b.Envelope()
	if err != nil {
		return "", "not an attestation (no DSSE envelope)", ""
	}
	st, err := env.Statement()
	if err != nil {
		return "", "unreadable statement: " + err.Error(), ""
	}
	if pt := st.GetPredicateType(); pt != attest.ProvenanceV1 {
		return "", "predicate " + pt + ", not build provenance", ""
	}
	vc, err := b.VerificationContent()
	if err != nil {
		return "", "unreadable verification material: " + err.Error(), ""
	}
	c := vc.Certificate()
	if c == nil {
		return "", "signed with a key, not a workflow certificate", ""
	}
	if !slices.Contains(c.Issuer.Organization, publicGoodOrg) {
		return "", fmt.Sprintf("issued by %q, not public-good Sigstore", strings.Join(c.Issuer.Organization, ", ")), ""
	}

	res, err := v.v.Verify(&b, policy)
	if err != nil {
		return "", "", err.Error()
	}
	if res.Statement == nil || res.Statement.GetPredicateType() != attest.ProvenanceV1 {
		return "", "", "the verified statement is not build provenance"
	}
	if res.Signature == nil || res.Signature.Certificate == nil {
		return "", "", "no certificate in the verification result"
	}
	if err := checkSource(res.Signature.Certificate, p.Repo); err != nil {
		return "", "", err.Error()
	}
	return res.Signature.Certificate.SubjectAlternativeName, "", ""
}

// checkSource compares the certificate's source repository and owner, ignoring case as gh does
// (GitHub's names are case-insensitive).
func checkSource(c *certificate.Summary, repo string) error {
	owner, _, _ := strings.Cut(repo, "/")
	if want := "https://github.com/" + repo; !strings.EqualFold(c.SourceRepositoryURI, want) {
		return fmt.Errorf("built from %q, not %s", c.SourceRepositoryURI, want)
	}
	if want := "https://github.com/" + owner; !strings.EqualFold(c.SourceRepositoryOwnerURI, want) {
		return fmt.Errorf("owner %q, not %s", c.SourceRepositoryOwnerURI, want)
	}
	if c.Issuer != attest.GitHubIssuer {
		return fmt.Errorf("issuer %q, not %s", c.Issuer, attest.GitHubIssuer)
	}
	return nil
}

// ErrTooLarge: a bundle over MaxBundle, or one that would decode to more than 16 MiB.
var ErrTooLarge = errors.New("attestation bundle too large")

// DecodeSnappy expands a bundle as GitHub serves it from bundle_url (snappy's block format). The
// decoded length is read from the header and checked before anything is allocated.
func DecodeSnappy(b []byte) ([]byte, error) {
	if len(b) > MaxBundle {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, len(b))
	}
	n, err := snappy.DecodedLen(b)
	if err != nil {
		return nil, fmt.Errorf("attestation bundle: %w", err)
	}
	if n > maxDecoded {
		return nil, fmt.Errorf("%w: it would expand to %d bytes", ErrTooLarge, n)
	}
	out, err := snappy.Decode(nil, b)
	if err != nil {
		return nil, fmt.Errorf("attestation bundle: %w", err)
	}
	return out, nil
}
