package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/enls/gremloader/internal/attest"
	"github.com/enls/gremloader/internal/events"
	"github.com/enls/gremloader/internal/pkgfile"
	"github.com/enls/gremloader/internal/redact"
)

// Attester checks the attestations of a downloaded asset (ADR 0014); *attest.Checker is one.
type Attester interface {
	Check(ctx context.Context, repo, sha256hex string, p attest.Policy) (attest.Result, error)
}

// levelAttestation is the verification level a passing attestation adds.
const levelAttestation = "attestation"

// attestation runs the attestation step for the bytes received (E.3.2 step 2, after the hash
// checks), whatever let them through: --accept-sha256 answers a different question. It reports
// whether a build-provenance bundle passed. Refusals are VerifyErrors (exit 5); a required check
// that could not be made carries attest.ErrUnreachable (offline, exit 7 when nothing else ran).
func (e *Engine) attestation(ctx context.Context, it *Item, pkg *pkgfile.Package, sha256hex string, o ApplyOptions) (bool, error) {
	required := slices.Contains(pkg.VerifyRequire, levelAttestation)
	if pkg.Attestation == "off" && !required {
		return false, nil
	}
	ev := events.Event{Package: it.Package, Version: it.To, URL: it.Asset.URL}
	res, err := attest.Result{}, fmt.Errorf("%w: no attestation checker", attest.ErrUnreachable)
	if e.cfg.Attest != nil {
		res, err = e.cfg.Attest.Check(ctx, pkg.Repo, sha256hex, attest.Policy{Repo: pkg.Repo, SignerWorkflow: pkg.SignerWorkflow})
	}
	switch {
	case err != nil && !errors.Is(err, attest.ErrUnreachable):
		return false, &VerifyError{Package: it.Package, Msg: "attestation: " + err.Error()}

	case err != nil:
		ev.Level, ev.Event, ev.Msg = events.Warn, "attest.skipped", redact.Text(err.Error())
		e.emit(ev)
		if required {
			return false, fmt.Errorf("%s: verify.require lists attestation, and it could not be checked: %w", it.Package, err)
		}
		if rec := e.installed(it.Package); rec != nil && rec.Attested {
			return false, e.attestationRegression(it, o, "could not be checked for one ("+redact.Text(err.Error())+")")
		}
		it.notef("attestation not checked: %s", redact.Text(err.Error()))
		return false, nil

	case res.Outcome == attest.Passed:
		ev.Level, ev.Event, ev.Result = events.Info, "attest.ok", res.Workflow
		e.emit(ev)
		it.Signer = res.Workflow
		return true, nil

	case res.Outcome == attest.Failed:
		msg := fmt.Sprintf("%s has attestations, and none shows it was built by a workflow of %s", it.Asset.Name, pkg.Repo)
		if pkg.SignerWorkflow != "" {
			msg += " (" + pkg.SignerWorkflow + ")"
		}
		msg += ":\n  " + strings.Join(res.Failures, "\n  ") + "\nNothing was installed."
		ev.Level, ev.Event, ev.Code, ev.Msg = events.Error, "attest.fail", "attestation-failed", msg
		e.emit(ev)
		return false, &VerifyError{Package: it.Package, Msg: msg}
	}

	ev.Level, ev.Event, ev.Msg = events.Info, "attest.none", strings.Join(res.Skipped, "; ")
	e.emit(ev)
	if required {
		return false, &VerifyError{Package: it.Package, Msg: fmt.Sprintf("verify.require lists attestation, and %s %s publishes no build provenance for %s",
			pkg.Repo, it.Release.Tag, it.Asset.Name)}
	}
	return false, e.attestationRegression(it, o, "has no build provenance")
}

// attestationRegression applies P3-10: when the installed version was attested and this one is
// not, the change is refused unless --allow-attestation-regression was given.
func (e *Engine) attestationRegression(it *Item, o ApplyOptions, what string) error {
	rec := e.installed(it.Package)
	if rec == nil || !rec.Attested {
		return nil
	}
	if o.fetchOnly {
		msg := fmt.Sprintf("the installed %s was recorded as attested, and the release of %s %s now; "+
			"check where that release came from", rec.Version, it.To, what)
		e.emit(events.Event{Level: events.Error, Event: "attest.fail", Package: it.Package, Version: it.To, Code: "attestation-regression", Msg: msg})
		return &VerifyError{Package: it.Package, Msg: msg}
	}
	if o.AllowAttestationRegression {
		e.emit(events.Event{Level: events.Warn, Event: "attest.regression", Package: it.Package, Version: it.To,
			Code: "attestation-regression", Msg: fmt.Sprintf("%s was attested, and %s %s; allowed by --allow-attestation-regression", rec.Version, it.To, what)})
		it.notef("attestation regression allowed: %s was attested, %s %s", rec.Version, it.To, what)
		return nil
	}
	msg := fmt.Sprintf("the installed %s was attested, and %s %s. Refused (an attestation regression): "+
		"nothing was installed. `--allow-attestation-regression` installs it anyway.", rec.Version, it.To, what)
	e.emit(events.Event{Level: events.Error, Event: "attest.fail", Package: it.Package, Version: it.To, Code: "attestation-regression", Msg: msg})
	return &VerifyError{Package: it.Package, Msg: msg}
}
