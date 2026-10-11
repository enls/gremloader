package attest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"

	"github.com/enls/gremloader/internal/testutil/fixtures"
)

// The real cli/cli v2.102.0 bundles (testdata/fixtures/README.md): the build provenance of the
// Windows zip, signed through public-good Sigstore by cli/cli's deployment workflow, and GitHub's
// release attestation, one bundle for every asset of the release.
const (
	zipSHA256       = "ae64e556ecc240b200f7eba60d550e4bb60d78e860e69dd88c449405b86067f4"
	checksumsSHA256 = "afe49e9affa232faa8212aed035417166f6ade9b9470acb53d4dbd28c0504e8d"
	ghWorkflow      = "https://github.com/cli/cli/.github/workflows/deployment.yml@refs/heads/trunk"
)

func verifier(t *testing.T) *Verifier {
	t.Helper()
	tr, err := root.NewTrustedRootFromJSON(fixtures.Read(t, "sigstore/trusted_root.json"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(tr)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func bundleFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := DecodeSnappy(fixtures.Read(t, "ghcli-v2.102.0/"+name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func provenance(t *testing.T) []byte {
	return bundleFixture(t, "gh_2.102.0_windows_amd64.zip.provenance.sn")
}
func release(t *testing.T) []byte { return bundleFixture(t, "gh_2.102.0_checksums.txt.bundle.sn") }

func check(t *testing.T, bundles [][]byte, digest string, p Policy) Result {
	t.Helper()
	r, err := verifier(t).Check(bundles, digest, p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestProvenancePasses(t *testing.T) {
	for _, repo := range []string{"cli/cli", "CLI/Cli"} {
		r := check(t, [][]byte{provenance(t)}, zipSHA256, Policy{Repo: repo})
		if r.Outcome != Passed || r.Workflow != ghWorkflow {
			t.Errorf("%s: %+v", repo, r)
		}
	}
}

// What GitHub's API returns for the zip: the release attestation first, then the provenance. The
// release attestation is skipped, whichever order they come in.
func TestReleaseAttestationIsSkipped(t *testing.T) {
	r := check(t, [][]byte{release(t)}, checksumsSHA256, Policy{Repo: "cli/cli"})
	if r.Outcome != None || len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0], "release") || len(r.Failures) != 0 {
		t.Errorf("release attestation alone: %+v", r)
	}
	for _, order := range [][][]byte{{release(t), provenance(t)}, {provenance(t), release(t)}} {
		if r := check(t, order, zipSHA256, Policy{Repo: "cli/cli"}); r.Outcome != Passed || len(r.Skipped) != 1 {
			t.Errorf("both: %+v", r)
		}
	}
}

func TestNoBundlesIsNone(t *testing.T) {
	if r := check(t, nil, zipSHA256, Policy{Repo: "cli/cli"}); r.Outcome != None || r.Workflow != "" {
		t.Errorf("%+v", r)
	}
}

func TestFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		digest string
		p      Policy
		want   string
	}{
		{"another file's digest", checksumsSHA256, Policy{Repo: "cli/cli"}, "digest"},
		{"another repository", zipSHA256, Policy{Repo: "cli/other"}, "SAN"},
		{"another owner", zipSHA256, Policy{Repo: "other/cli"}, "SAN"},
		{"another workflow", zipSHA256, Policy{Repo: "cli/cli", SignerWorkflow: ".github/workflows/release.yml"}, "SAN"},
		{"a prefix of the workflow", zipSHA256, Policy{Repo: "cli/cli", SignerWorkflow: ".github/workflows/deploy.yml"}, "SAN"},
		{"another ref", zipSHA256, Policy{Repo: "cli/cli", SignerWorkflow: ".github/workflows/deployment.yml@refs/heads/main"}, "SAN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := check(t, [][]byte{provenance(t)}, tc.digest, tc.p)
			if r.Outcome != Failed || len(r.Failures) != 1 || !strings.Contains(r.Failures[0], tc.want) {
				t.Errorf("%+v", r)
			}
		})
	}
}

func TestSignerWorkflowPasses(t *testing.T) {
	for _, wf := range []string{".github/workflows/deployment.yml", ".github/workflows/deployment.yml@refs/heads/trunk"} {
		if r := check(t, [][]byte{provenance(t)}, zipSHA256, Policy{Repo: "cli/cli", SignerWorkflow: wf}); r.Outcome != Passed {
			t.Errorf("%s: %+v", wf, r)
		}
	}
}

// A bundle whose signature was changed fails; it is not skipped.
func TestTamperedSignatureFails(t *testing.T) {
	var b map[string]any
	if err := json.Unmarshal(provenance(t), &b); err != nil {
		t.Fatal(err)
	}
	sig := b["dsseEnvelope"].(map[string]any)["signatures"].([]any)[0].(map[string]any)
	s := sig["sig"].(string)
	flip := "A"
	if s[10:11] == "A" {
		flip = "B"
	}
	sig["sig"] = s[:10] + flip + s[11:]
	tampered, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	r := check(t, [][]byte{tampered}, zipSHA256, Policy{Repo: "cli/cli"})
	if r.Outcome != Failed {
		t.Errorf("%+v", r)
	}
	// A failing bundle does not hide a passing one.
	if r := check(t, [][]byte{tampered, provenance(t)}, zipSHA256, Policy{Repo: "cli/cli"}); r.Outcome != Passed || len(r.Failures) != 1 {
		t.Errorf("tampered + good: %+v", r)
	}
}

func TestUnreadableIsSkipped(t *testing.T) {
	r := check(t, [][]byte{[]byte("{"), []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`)}, zipSHA256, Policy{Repo: "cli/cli"})
	if r.Outcome != None || len(r.Skipped) != 2 {
		t.Errorf("%+v", r)
	}
}

func TestBadPolicyOrDigest(t *testing.T) {
	v := verifier(t)
	for _, p := range []Policy{{Repo: "cli"}, {Repo: "cli/cli/x"}, {Repo: "../cli"}, {Repo: "cli/.."}, {Repo: ""},
		{Repo: "cli/cli", SignerWorkflow: "deployment.yml"}, {Repo: "cli/cli", SignerWorkflow: ".github/workflows/a/b.yml"},
		{Repo: "cli/cli", SignerWorkflow: ".github/workflows/../x.yml"}, {Repo: "cli/cli", SignerWorkflow: ".github/workflows/x.txt"}} {
		if _, err := v.Check(nil, zipSHA256, p); err == nil {
			t.Errorf("%+v: no error", p)
		}
	}
	for _, d := range []string{"", "xyz", zipSHA256[:62], zipSHA256 + "00"} {
		if _, err := v.Check(nil, d, Policy{Repo: "cli/cli"}); err == nil {
			t.Errorf("digest %q: no error", d)
		}
	}
}

func TestDecodeSnappyLimits(t *testing.T) {
	if _, err := DecodeSnappy(make([]byte, MaxBundle+1)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversize input: %v", err)
	}
	// A header that claims 17 MiB (uvarint), with nothing behind it.
	n, bomb := maxDecoded+1<<20, []byte{}
	for ; n >= 0x80; n >>= 7 {
		bomb = append(bomb, byte(n)|0x80)
	}
	bomb = append(bomb, byte(n))
	if _, err := DecodeSnappy(bomb); !errors.Is(err, ErrTooLarge) {
		t.Errorf("bomb: %v", err)
	}
	if _, err := DecodeSnappy([]byte{0x05, 0x00}); err == nil || errors.Is(err, ErrTooLarge) {
		t.Errorf("corrupt: %v", err)
	}
}
