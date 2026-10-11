package sigstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/sigstore/sigstore-go/pkg/root"

	"github.com/enls/gremloader/internal/attest"
)

// Checker lists an asset's bundles and checks them. The trust root is fetched the first time a
// bundle needs checking, so a package that publishes none costs one API request and nothing more.
type Checker struct {
	// List returns the bundles published for the asset of repo whose SHA-256 is sha256hex; none
	// is (nil, nil).
	List func(ctx context.Context, repo, sha256hex string) ([]attest.Raw, error)
	// Root returns the trusted root (TrustedRoot, or a fixture in tests).
	Root func(ctx context.Context) (root.TrustedMaterial, error)

	mu sync.Mutex
	v  *Verifier
}

// Check lists and checks the bundles for the asset. An error wrapping attest.ErrUnreachable means the
// bundles or the trust root could not be fetched; any other error is a policy or digest that
// cannot be used.
func (c *Checker) Check(ctx context.Context, repo, sha256hex string, p attest.Policy) (attest.Result, error) {
	if p.Repo != repo {
		return attest.Result{}, fmt.Errorf("policy for %q used for %q", p.Repo, repo)
	}
	if err := p.Validate(); err != nil {
		return attest.Result{}, err
	}
	raws, err := c.List(ctx, repo, sha256hex)
	if err != nil {
		return attest.Result{}, fmt.Errorf("%w: the attestations of %s: %w", attest.ErrUnreachable, repo, err)
	}
	if len(raws) == 0 {
		return attest.Result{}, nil
	}
	bundles := make([][]byte, len(raws))
	decodeErr := map[int]error{}
	for i, r := range raws {
		bundles[i] = r.Data
		if r.Snappy {
			if bundles[i], err = DecodeSnappy(r.Data); err != nil {
				bundles[i], decodeErr[i] = nil, err
			}
		}
	}
	v, err := c.verifier(ctx)
	if err != nil {
		return attest.Result{}, err
	}
	res, err := v.Check(bundles, sha256hex, p)
	if err != nil {
		return attest.Result{}, err
	}
	for i, err := range decodeErr {
		prefix := fmt.Sprintf("bundle %d: ", i+1)
		for j, s := range res.Skipped {
			if strings.HasPrefix(s, prefix) {
				res.Skipped[j] = prefix + err.Error()
			}
		}
	}
	return res, nil
}

func (c *Checker) verifier(ctx context.Context) (*Verifier, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.v != nil {
		return c.v, nil
	}
	tm, err := c.Root(ctx)
	if err != nil {
		if !errors.Is(err, attest.ErrUnreachable) {
			err = fmt.Errorf("%w: %w", attest.ErrUnreachable, err)
		}
		return nil, err
	}
	v, err := NewVerifier(tm)
	if err != nil {
		return nil, err
	}
	c.v = v
	return v, nil
}
