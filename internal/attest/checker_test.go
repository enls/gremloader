package attest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"

	"github.com/enls/gremloader/internal/testutil/fixtures"
)

type checkerRig struct {
	lists, roots int
	raws         []Raw
	listErr      error
	rootErr      error
}

func (r *checkerRig) checker(t *testing.T) *Checker {
	return &Checker{
		List: func(context.Context, string, string) ([]Raw, error) {
			r.lists++
			return r.raws, r.listErr
		},
		Root: func(context.Context) (root.TrustedMaterial, error) {
			r.roots++
			if r.rootErr != nil {
				return nil, r.rootErr
			}
			return root.NewTrustedRootFromJSON(fixtures.Read(t, "sigstore/trusted_root.json"))
		},
	}
}

var cliPolicy = Policy{Repo: "cli/cli"}

func TestCheckerNoneNeverFetchesTheRoot(t *testing.T) {
	r := &checkerRig{}
	res, err := r.checker(t).Check(context.Background(), "cli/cli", zipSHA256, cliPolicy)
	if err != nil || res.Outcome != None || r.roots != 0 {
		t.Errorf("%+v %v, roots %d", res, err, r.roots)
	}
}

func TestCheckerPassesAndFetchesTheRootOnce(t *testing.T) {
	r := &checkerRig{raws: []Raw{{Data: fixtures.Read(t, "ghcli-v2.102.0/gh_2.102.0_windows_amd64.zip.provenance.sn"), Snappy: true},
		{Data: release(t)}}}
	c := r.checker(t)
	for range 2 {
		res, err := c.Check(context.Background(), "cli/cli", zipSHA256, cliPolicy)
		if err != nil || res.Outcome != Passed || len(res.Skipped) != 1 {
			t.Fatalf("%+v %v", res, err)
		}
	}
	if r.roots != 1 {
		t.Errorf("root fetched %d times", r.roots)
	}
}

func TestCheckerUnreachable(t *testing.T) {
	r := &checkerRig{listErr: errors.New("connection refused")}
	if _, err := r.checker(t).Check(context.Background(), "cli/cli", zipSHA256, cliPolicy); !errors.Is(err, ErrUnreachable) || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("list: %v", err)
	}
	r = &checkerRig{raws: []Raw{{Data: provenance(t)}}, rootErr: errors.New("no TUF")}
	c := r.checker(t)
	if _, err := c.Check(context.Background(), "cli/cli", zipSHA256, cliPolicy); !errors.Is(err, ErrUnreachable) {
		t.Errorf("root: %v", err)
	}
	// A failed root fetch is tried again next time.
	r.rootErr = nil
	if res, err := c.Check(context.Background(), "cli/cli", zipSHA256, cliPolicy); err != nil || res.Outcome != Passed || r.roots != 2 {
		t.Errorf("%+v %v, roots %d", res, err, r.roots)
	}
}

func TestCheckerUndecodableIsSkippedWithItsReason(t *testing.T) {
	r := &checkerRig{raws: []Raw{{Data: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, Snappy: true}}}
	res, err := r.checker(t).Check(context.Background(), "cli/cli", zipSHA256, cliPolicy)
	if err != nil || res.Outcome != None || len(res.Skipped) != 1 || !strings.HasPrefix(res.Skipped[0], "bundle 1: attestation bundle: ") {
		t.Errorf("%+v %v", res, err)
	}
}

func TestCheckerBadPolicySendsNothing(t *testing.T) {
	r := &checkerRig{}
	for _, tc := range []struct {
		repo string
		p    Policy
	}{{"cli/cli", Policy{Repo: "cli"}}, {"cli/cli", Policy{Repo: "cli/other"}}} {
		if _, err := r.checker(t).Check(context.Background(), tc.repo, zipSHA256, tc.p); err == nil || errors.Is(err, ErrUnreachable) {
			t.Errorf("%+v: %v", tc, err)
		}
	}
	if r.lists != 0 {
		t.Errorf("listed %d times", r.lists)
	}
}
