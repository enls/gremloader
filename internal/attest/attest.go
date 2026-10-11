// Package attest checks GitHub artifact attestations (ADR 0014): a build-provenance bundle, signed
// through public-good Sigstore by a workflow of the package's own repository, for the exact bytes
// that were downloaded. It is the only package that imports sigstore-go (ADR 0006).
//
// A bundle of another kind (GitHub's release attestation, an SBOM, one signed by GitHub's own
// Sigstore instance for a private repository) is skipped, not failed: it says nothing about how
// the asset was built. Skipping cannot let a forged bundle through; at worst the asset looks
// unattested, which the engine's regression rule catches.
package attest

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/golang/snappy"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const (
	// GitHubIssuer is the OIDC issuer of GitHub Actions workflows.
	GitHubIssuer = "https://token.actions.githubusercontent.com"
	// ProvenanceV1 is the predicate type of a build-provenance attestation.
	ProvenanceV1 = "https://slsa.dev/provenance/v1"
	// publicGoodOrg is the organisation of public-good Fulcio's certificates; GitHub's own
	// instance says "GitHub, Inc.".
	publicGoodOrg = "sigstore.dev"

	// MaxBundle is the largest compressed bundle read; maxDecoded the largest it may expand to.
	// Real ones are a few KiB to 12 KiB.
	MaxBundle  = 2 << 20
	maxDecoded = 16 << 20
)

// Policy says which build counts.
type Policy struct {
	// Repo is "owner/name": the repository whose workflow must have built the asset.
	Repo string
	// SignerWorkflow optionally pins the workflow file, as a path in Repo
	// (".github/workflows/release.yml"), with or without "@<ref>".
	SignerWorkflow string
}

// Outcome is what the bundles of one asset amount to.
type Outcome int

const (
	// None: no bundle of the kind checked here (none published, or only other kinds).
	None Outcome = iota
	// Passed: a build-provenance bundle verified and matched the policy.
	Passed
	// Failed: build-provenance bundles were published and none passed.
	Failed
)

func (o Outcome) String() string {
	switch o {
	case Passed:
		return "passed"
	case Failed:
		return "failed"
	}
	return "none"
}

// Result is the outcome with its reasons.
type Result struct {
	Outcome Outcome
	// Workflow is the passing bundle's signer: the workflow file and ref that built the asset.
	Workflow string
	// Skipped says why bundles were not checked; Failures why checked ones did not pass.
	Skipped  []string
	Failures []string
}

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

var (
	repoPattern     = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	workflowPattern = regexp.MustCompile(`^\.github/workflows/[^/@]+\.ya?ml(@.+)?$`)
	// refPattern matches the "@<ref>" that ends a workflow certificate's SAN: a "refs/" path or a
	// commit. A ref may contain "@" itself, so the separator is found by shape (as gh does).
	refPattern = `@(refs/.*|[0-9a-fA-F]+)`
	hasRef     = regexp.MustCompile(refPattern + `$`)
)

// Validate reports a policy that could never match anything.
func (p Policy) Validate() error {
	if !repoPattern.MatchString(p.Repo) || strings.Contains(p.Repo, "..") {
		return fmt.Errorf("%q is not owner/name", p.Repo)
	}
	if p.SignerWorkflow != "" && (!workflowPattern.MatchString(p.SignerWorkflow) || strings.Contains(p.SignerWorkflow, "..")) {
		return fmt.Errorf("signer workflow %q is not .github/workflows/<file>.yml, optionally with @<ref>", p.SignerWorkflow)
	}
	return nil
}

// sanPattern is the SAN the certificate must have. The repository part ignores case, as GitHub
// does; a pinned workflow is matched exactly and anchored at both ends, so release.yml does not
// admit release.yml.evil.yml, and a pinned ref does not admit a second ref after it.
func (p Policy) sanPattern() string {
	base := "(?i:" + regexp.QuoteMeta("https://github.com/"+p.Repo+"/") + ")"
	if p.SignerWorkflow == "" {
		return "^" + base
	}
	pat := "^" + base + regexp.QuoteMeta(p.SignerWorkflow)
	if hasRef.MatchString(p.SignerWorkflow) {
		return pat + "$"
	}
	return pat + "(" + refPattern + ")?$"
}

// Check verifies the bundles (decoded JSON) published for an asset whose SHA-256 is sha256hex.
// The error is for a policy or digest that cannot be used; what the bundles amount to is in the
// Result.
func (v *Verifier) Check(bundles [][]byte, sha256hex string, p Policy) (Result, error) {
	if err := p.Validate(); err != nil {
		return Result{}, err
	}
	digest, err := hex.DecodeString(sha256hex)
	if err != nil || len(digest) != 32 {
		return Result{}, fmt.Errorf("%q is not a SHA-256", sha256hex)
	}
	san, err := verify.NewSANMatcher("", p.sanPattern())
	if err != nil {
		return Result{}, err
	}
	issuer, err := verify.NewIssuerMatcher(GitHubIssuer, "")
	if err != nil {
		return Result{}, err
	}
	id, err := verify.NewCertificateIdentity(san, issuer, certificate.Extensions{})
	if err != nil {
		return Result{}, err
	}
	policy := verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(id))

	var r Result
	for i, raw := range bundles {
		signer, skip, fail := v.one(raw, policy, p)
		switch {
		case skip != "":
			r.Skipped = append(r.Skipped, fmt.Sprintf("bundle %d: %s", i+1, skip))
		case fail != "":
			r.Failures = append(r.Failures, fmt.Sprintf("bundle %d: %s", i+1, fail))
		case r.Outcome != Passed:
			r.Outcome, r.Workflow = Passed, signer
		}
	}
	if r.Outcome != Passed && len(r.Failures) > 0 {
		r.Outcome = Failed
	}
	return r, nil
}

// one checks one bundle: the signer when it passes, else why it was skipped or failed. What
// decides "skip" is read before verification, which is safe: a forged value can only make the
// bundle be ignored.
func (v *Verifier) one(raw []byte, policy verify.PolicyBuilder, p Policy) (signer, skip, fail string) {
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
	if pt := st.GetPredicateType(); pt != ProvenanceV1 {
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
	if res.Statement == nil || res.Statement.GetPredicateType() != ProvenanceV1 {
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
	if c.Issuer != GitHubIssuer {
		return fmt.Errorf("issuer %q, not %s", c.Issuer, GitHubIssuer)
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
