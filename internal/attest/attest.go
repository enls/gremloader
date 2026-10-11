// Package attest holds what the engine needs to know about attestations (ADR 0014): which build
// counts (Policy), what a check found (Result), and the error for a check that could not be made.
// The checking itself is in attest/sigstore, the only package that imports sigstore-go (ADR 0006),
// so the engine and everything that imports it stay free of that dependency graph.
package attest

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	// GitHubIssuer is the OIDC issuer of GitHub Actions workflows.
	GitHubIssuer = "https://token.actions.githubusercontent.com"
	// ProvenanceV1 is the predicate type of a build-provenance attestation.
	ProvenanceV1 = "https://slsa.dev/provenance/v1"
)

// ErrUnreachable: the trust root, the attestation list or a bundle could not be fetched. The engine
// treats it as "could not check", which is never "attested".
var ErrUnreachable = errors.New("attestation check unreachable")

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
	// None: no bundle of the kind checked (none published, or only other kinds).
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

// SANPattern is the regular expression the signing certificate's SAN must match. The repository
// part ignores case, as GitHub does; a pinned workflow is matched exactly and anchored at both
// ends, so release.yml does not admit release.yml.evil.yml, and a pinned ref does not admit a
// second ref after it.
func (p Policy) SANPattern() string {
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

// Raw is a bundle as it was fetched: JSON, or snappy's block format from a bundle_url.
type Raw struct {
	Data   []byte
	Snappy bool
}
