package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/attest"
	"github.com/enls/gremloader/internal/events"
	"github.com/enls/gremloader/internal/exitcode"
)

// fakeAttest answers attestation checks: by asset hash when listed, else def (or err).
type fakeAttest struct {
	calls  int
	bySHA  map[string]attest.Result
	def    attest.Result
	err    error
	policy attest.Policy
}

func (f *fakeAttest) Check(_ context.Context, repo, sum string, p attest.Policy) (attest.Result, error) {
	f.calls++
	f.policy = p
	if f.err != nil {
		return attest.Result{}, f.err
	}
	if r, ok := f.bySHA[sum]; ok {
		return r, nil
	}
	return f.def, nil
}

var (
	passed         = attest.Result{Outcome: attest.Passed, Workflow: "https://github.com/o/app/.github/workflows/release.yml@refs/tags/v1"}
	failedCheck    = attest.Result{Outcome: attest.Failed, Failures: []string{"bundle 1: expected SAN value to match"}}
	errUnreachable = fmt.Errorf("%w: connection refused", attest.ErrUnreachable)
)

// attestedV1 installs v1 with a passing attestation and publishes v2: an update from an attested
// version.
func attestedV1(t *testing.T, file string) (*rig, *fakeAttest) {
	t.Helper()
	r := newRig(t, file)
	fa := &fakeAttest{def: passed}
	r.attest = fa
	r.publish("app", "1.0.0", 30, v1Files...)
	if it := item(t, r.apply(r.engine(), Request{InstallMissing: true}), "app"); it.Result != ResultInstalled {
		t.Fatalf("installing v1: %+v", it)
	}
	if rec := r.state().Packages["app"]; !rec.Attested || !slices.Contains(rec.VerifiedBy, "attestation") {
		t.Fatalf("v1 record: %+v", rec)
	}
	r.publish("app", "2.0.0", 20, v2Files...)
	return r, fa
}

// With nothing published (the Craft apps), an update installs and is not attested. The real
// checker asked the fake API once and never needed the trust root.
func TestAttestationNonePublished(t *testing.T) {
	r := atV1(t)
	r.srv.ResetRequests()
	it := item(t, r.apply(r.engine(), Request{}), "app")
	if it.Result != ResultUpdated || it.Signer != "" || len(it.Notes) != 0 {
		t.Fatalf("%+v", it)
	}
	if rec := r.state().Packages["app"]; rec.Attested || slices.Contains(rec.VerifiedBy, "attestation") {
		t.Errorf("record: %+v", rec)
	}
	if n := r.srv.RequestCount("/attestations/sha256:"); n != 1 {
		t.Errorf("%d attestation requests", n)
	}
	if !hasEvent(r.events(), "attest.none") {
		t.Error("no attest.none event")
	}
}

func TestAttestationPasses(t *testing.T) {
	r, fa := attestedV1(t, appFile+"verify.signer_workflow = \".github/workflows/release.yml\"\n")
	it := item(t, r.apply(r.engine(), Request{}), "app")
	if it.Result != ResultUpdated || it.Signer != passed.Workflow {
		t.Fatalf("%+v", it)
	}
	if rec := r.state().Packages["app"]; !rec.Attested || !slices.Equal(rec.VerifiedBy, []string{"checksum", "attestation"}) {
		t.Errorf("record: %+v", rec)
	}
	if fa.policy != (attest.Policy{Repo: "o/app", SignerWorkflow: ".github/workflows/release.yml"}) {
		t.Errorf("policy %+v", fa.policy)
	}
	if !hasEvent(r.events(), "attest.ok") {
		t.Error("no attest.ok event")
	}
}

// A failing attestation refuses the change like a hash mismatch: exit 5, the old version
// untouched, and no second download (the bytes matched their hash).
func TestAttestationFailsHard(t *testing.T) {
	r, fa := attestedV1(t, appFile)
	fa.def = failedCheck
	before := tree(t, r.dir())
	r.srv.ResetRequests()
	p := r.apply(r.engine(), Request{})
	it := item(t, p, "app")
	var ve *VerifyError
	if it.Result != ResultFailed || !errors.As(it.Err, &ve) || !strings.Contains(it.Err.Error(), "expected SAN value") || p.ExitCode(false) != exitcode.VerifyFailed {
		t.Fatalf("%+v (exit %d)", it, p.ExitCode(false))
	}
	assertTree(t, r.dir(), before)
	if rec := r.state().Packages["app"]; rec.Version != "1.0.0" || !rec.Attested {
		t.Errorf("record: %+v", rec)
	}
	if n := r.srv.RequestCount("app-2.0.0.zip"); n != 1 {
		t.Errorf("downloaded %d times", n)
	}
}

// P3-10: an attested package does not silently move to a version without one.
func TestAttestationRegression(t *testing.T) {
	for _, tc := range []struct {
		name string
		def  attest.Result
		err  error
		want string
	}{
		{"none published", attest.Result{}, nil, "has no build provenance"},
		{"unreachable", attest.Result{}, errUnreachable, "could not be checked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, fa := attestedV1(t, appFile)
			fa.def, fa.err = tc.def, tc.err
			before := tree(t, r.dir())
			p := r.apply(r.engine(), Request{})
			it := item(t, p, "app")
			if it.Result != ResultFailed || !strings.Contains(it.Err.Error(), tc.want) || !strings.Contains(it.Err.Error(), "--allow-attestation-regression") ||
				p.ExitCode(false) != exitcode.VerifyFailed {
				t.Fatalf("%+v (exit %d)", it, p.ExitCode(false))
			}
			assertTree(t, r.dir(), before)

			p, err := r.applyWith(r.engine(), Request{}, ApplyOptions{AllowAttestationRegression: true})
			it = item(t, p, "app")
			if err != nil || it.Result != ResultUpdated || !slices.ContainsFunc(it.Notes, func(n string) bool { return strings.Contains(n, "regression allowed") }) {
				t.Fatalf("allowed: %+v %v", it, err)
			}
			if rec := r.state().Packages["app"]; rec.Version != "2.0.0" || rec.Attested {
				t.Errorf("record: %+v", rec)
			}
			if !hasEvent(r.events(), "attest.regression") {
				t.Error("no attest.regression event")
			}
		})
	}
}

// Not attested before, and the check cannot be made: a warning, the update goes ahead.
func TestAttestationUnreachableWarns(t *testing.T) {
	r := atV1(t)
	r.attest = &fakeAttest{err: errUnreachable}
	it := item(t, r.apply(r.engine(), Request{}), "app")
	if it.Result != ResultUpdated || !slices.ContainsFunc(it.Notes, func(n string) bool { return strings.Contains(n, "attestation not checked") }) {
		t.Fatalf("%+v", it)
	}
	if !hasEvent(r.events(), "attest.skipped") {
		t.Error("no attest.skipped event")
	}
}

func TestAttestationRequired(t *testing.T) {
	file := appFile + "verify.require = [\"checksum\", \"attestation\"]\n"
	for _, tc := range []struct {
		name   string
		fa     *fakeAttest
		result Result
		exit   int
	}{
		{"passes", &fakeAttest{def: passed}, ResultInstalled, exitcode.OK},
		{"none published", &fakeAttest{}, ResultFailed, exitcode.VerifyFailed},
		{"failed", &fakeAttest{def: failedCheck}, ResultFailed, exitcode.VerifyFailed},
		{"unreachable", &fakeAttest{err: errUnreachable}, ResultFailed, exitcode.Offline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, file)
			r.attest = tc.fa
			r.publish("app", "1.0.0", 30, v1Files...)
			p := r.apply(r.engine(), Request{InstallMissing: true})
			if it := item(t, p, "app"); it.Result != tc.result || p.ExitCode(false) != tc.exit {
				t.Fatalf("%+v (exit %d)", it, p.ExitCode(false))
			}
		})
	}
	// Without any checker at all, a required attestation is refused too.
	r := newRig(t, file)
	r.publish("app", "1.0.0", 30, v1Files...)
	e := r.engine()
	e.cfg.Attest = nil
	if p := r.apply(e, Request{InstallMissing: true}); item(t, p, "app").Result != ResultFailed {
		t.Errorf("nil checker: %+v", item(t, p, "app"))
	}
}

func TestAttestationOffIsNeverAsked(t *testing.T) {
	r := newRig(t, appFile+"verify.attestation = \"off\"\n")
	fa := &fakeAttest{def: failedCheck}
	r.attest = fa
	r.publish("app", "1.0.0", 30, v1Files...)
	if it := item(t, r.apply(r.engine(), Request{InstallMissing: true}), "app"); it.Result != ResultInstalled || fa.calls != 0 {
		t.Fatalf("%+v, %d calls", it, fa.calls)
	}
}

// --accept-sha256 answers a hash question; it does not get a failing attestation through.
func TestAcceptSHA256DoesNotPassAFailedAttestation(t *testing.T) {
	r := newRig(t, appFile)
	r.attest = &fakeAttest{def: failedCheck}
	received := r.publishBadSums("3.0.0")
	p, err := r.applyWith(r.engine(), Request{Packages: []string{"app"}, Want: "3.0.0", InstallMissing: true},
		ApplyOptions{Accept: &Acceptance{Package: "app", Version: "3.0.0", SHA256: received}})
	it := item(t, p, "app")
	if err != nil || it.Result != ResultFailed || !strings.Contains(it.Err.Error(), "expected SAN value") {
		t.Fatalf("%+v %v", it, err)
	}
}

// Rollback downloads nothing; the record it restores keeps its own attestation, both ways.
func TestRollbackRestoresAttested(t *testing.T) {
	r, fa := attestedV1(t, appFile)
	fa.def = attest.Result{}
	if _, err := r.applyWith(r.engine(), Request{}, ApplyOptions{AllowAttestationRegression: true}); err != nil {
		t.Fatal(err)
	}
	if r.state().Packages["app"].Attested {
		t.Fatal("v2 recorded as attested")
	}
	calls := fa.calls
	if it := r.engine().Rollback(context.Background(), "app"); it.Result != ResultRolledBack {
		t.Fatalf("%+v", it)
	}
	if rec := r.state().Packages["app"]; rec.Version != "1.0.0" || !rec.Attested || !slices.Contains(rec.VerifiedBy, "attestation") {
		t.Errorf("after rollback: %+v", rec)
	}
	if fa.calls != calls {
		t.Error("rollback checked an attestation")
	}
	// Forward again restores v2's record: not attested.
	if it := r.engine().Rollback(context.Background(), "app"); it.Result != ResultRolledBack {
		t.Fatalf("%+v", it)
	}
	if rec := r.state().Packages["app"]; rec.Version != "2.0.0" || rec.Attested {
		t.Errorf("forward again: %+v", rec)
	}
}

func hasEvent(evs []events.Event, name string) bool {
	return slices.ContainsFunc(evs, func(e events.Event) bool { return e.Event == name })
}
