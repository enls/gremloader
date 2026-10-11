package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang/snappy"

	"github.com/enls/gremloader/internal/engine"
	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/state"
	"github.com/enls/gremloader/internal/testutil/archives"
	"github.com/enls/gremloader/internal/testutil/fakerelease"
	"github.com/enls/gremloader/internal/testutil/fixtures"
)

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// The whole path, as wired for the real program: the attestation list from the API, the bundle
// from the bundle store (snappy), the trust root, sigstore-go. The bundle is cli/cli's real
// provenance, published here for app's zip: it verifies, but not for these bytes, so the install is
// refused (exit 5) and nothing is installed.
func TestAttestationThatDoesNotMatchIsRefused(t *testing.T) {
	r := newRig(t, appFile)
	zip := r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	bundle, err := snappy.Decode(nil, fixtures.Read(t, "ghcli-v2.102.0/gh_2.102.0_windows_amd64.zip.provenance.sn"))
	if err != nil {
		t.Fatal(err)
	}
	r.srv.AddAttestation("o", "app", shaOf(zip), fakerelease.Attestation{Bundle: bundle})

	code, res := r.runJSON("install", "app", "--yes")
	var it jsonItem
	res.item(t, 0, &it)
	if code != exitcode.VerifyFailed || !strings.Contains(it.Error, "none shows it was built by a workflow of o/app") {
		t.Fatalf("exit %d: %+v", code, it)
	}
	if r.appExe() != "" {
		t.Error("installed anyway")
	}
	if n := r.srv.RequestCount("/blob/"); n != 1 {
		t.Errorf("%d bundle fetches", n)
	}
	for _, rq := range r.srv.Requests() {
		if rq.Origin == "blob" && rq.Auth != "" {
			t.Errorf("a token went to the bundle store: %+v", rq)
		}
	}
	// --accept-sha256 answers a hash question and does not change that.
	if code, _, errOut := r.run("install", "app@1.0.0", "--yes", "--accept-sha256", shaOf(zip)); code != exitcode.VerifyFailed {
		t.Errorf("with --accept-sha256: exit %d\n%s", code, errOut)
	}
}

// P3-10 from the command line: an update away from an attested version stops, and
// --allow-attestation-regression (never --yes) lets it through.
func TestAllowAttestationRegression(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	if code, out, errOut := r.run("install", "app", "--yes"); code != exitcode.OK {
		t.Fatalf("install: exit %d\n%s%s", code, out, errOut)
	}
	// As if 1.0.0's attestation had passed (a real one cannot be made for a test's zip).
	p := filepath.Join(r.stateDir(), "state.json")
	st, err := state.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	st.Packages["app"].Attested = true
	st.Packages["app"].VerifiedBy = append(st.Packages["app"].VerifiedBy, "attestation")
	if err := state.Save(p, st); err != nil {
		t.Fatal(err)
	}
	r.publish("2.0.0", 5, archives.F("app.exe", "2"))

	code, out, errOut := r.run("update", "app", "--yes")
	if code != exitcode.VerifyFailed || !strings.Contains(out+errOut, "--allow-attestation-regression") || r.appExe() != "1" {
		t.Fatalf("update: exit %d\n%s%s", code, out, errOut)
	}
	code, out, errOut = r.run("update", "app", "--yes", "--allow-attestation-regression")
	if code != exitcode.OK || r.appExe() != "2" || !strings.Contains(out, "attestation regression allowed") {
		t.Fatalf("allowed: exit %d\n%s%s", code, out, errOut)
	}
}

// A passing attestation is shown under the item: who built it.
func TestSignerIsShown(t *testing.T) {
	var b strings.Builder
	printItems(&b, []*engine.Item{{Package: "app", From: "1.0.0", To: "2.0.0", Action: engine.ActUpdate, Result: engine.ResultUpdated,
		Signer: "https://github.com/o/app/.github/workflows/release.yml@refs/tags/v2.0.0"}}, false)
	if !strings.Contains(b.String(), "  attested: built by https://github.com/o/app/.github/workflows/release.yml@refs/tags/v2.0.0\n") {
		t.Errorf("%q", b.String())
	}
	j := jsonItems([]*engine.Item{{Package: "app", Signer: "x"}})
	if j[0].Signer != "x" {
		t.Errorf("%+v", j)
	}
}
