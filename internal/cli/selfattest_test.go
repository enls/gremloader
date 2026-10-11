package cli

import (
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"

	"github.com/enls/gremloader/internal/attest"
	"github.com/enls/gremloader/internal/attest/sigstore"
	"github.com/enls/gremloader/internal/testutil/fixtures"
)

// The real build provenance of our own release v0.0.2-rc.1, windows/amd64 zip
// (testdata/fixtures/README.md), and that zip's digest.
const (
	rcZipSHA256 = "d47dcd96d1634342cbefd050466a91c3dc2842f6a3f009201839e3dd6a88dea7"
	rcWorkflow  = "https://github.com/enls/gremloader/.github/workflows/release.yml@refs/tags/v0.0.2-rc.1"
)

// The built-in package's policy, as the engine builds it, passes what our release workflow really
// publishes, and nothing signed by another workflow or repository.
func TestSelfPolicyPassesOurRelease(t *testing.T) {
	cfg, err := selfConfig(t.TempDir(), "stable")
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Packages[selfName]
	policy := attest.Policy{Repo: p.Repo, SignerWorkflow: p.SignerWorkflow}

	tr, err := root.NewTrustedRootFromJSON(fixtures.Read(t, "sigstore/trusted_root.json"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := sigstore.NewVerifier(tr)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := sigstore.DecodeSnappy(fixtures.Read(t, "gremloader-v0.0.2-rc.1/gremloader-0.0.2-rc.1-windows-amd64.zip.provenance.sn"))
	if err != nil {
		t.Fatal(err)
	}
	check := func(digest string, pol attest.Policy) attest.Result {
		t.Helper()
		r, err := v.Check([][]byte{bundle}, digest, pol)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	if r := check(rcZipSHA256, policy); r.Outcome != attest.Passed || r.Workflow != rcWorkflow {
		t.Fatalf("our release: %+v", r)
	}
	for name, pol := range map[string]attest.Policy{
		"another workflow":   {Repo: p.Repo, SignerWorkflow: ".github/workflows/ci.yml"},
		"another repository": {Repo: "enls/craftwerk", SignerWorkflow: p.SignerWorkflow},
	} {
		if r := check(rcZipSHA256, pol); r.Outcome != attest.Failed {
			t.Errorf("%s: %+v", name, r)
		}
	}
	other := "00" + rcZipSHA256[2:]
	if r := check(other, policy); r.Outcome == attest.Passed {
		t.Errorf("another file's digest: %+v", r)
	}
}
