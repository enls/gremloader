package attest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/theupdateframework/go-tuf/v2/metadata/fetcher"
)

// TUFHost is public-good Sigstore's TUF repository, the only host the trust-root fetch may reach.
const TUFHost = "tuf-repo-cdn.sigstore.dev"

// ErrUnreachable: the trust root, the attestation list or a bundle could not be fetched. The engine
// treats it as "could not check", which is never "attested".
var ErrUnreachable = errors.New("attestation check unreachable")

// tufTimeout bounds each TUF request; the TUF client has no context of its own.
const tufTimeout = 30 * time.Second

// TrustedRoot returns public-good Sigstore's trusted root through TUF (ADR 0014): bootstrapped from
// the root embedded in sigstore-go, cached in dir, refreshed at most once a day. When the refresh
// fails, a cached root is used as long as its metadata has not expired. hc carries the transport
// (tests, proxies); it is restricted to TUFHost over https.
func TrustedRoot(ctx context.Context, dir string, hc *http.Client) (root.TrustedMaterial, error) {
	tr, err := root.FetchTrustedRootWithOptions(tufOptions(ctx, dir, hc, false))
	if err == nil {
		return tr, nil
	}
	if _, serr := os.Stat(dir); serr == nil {
		if tr, ferr := root.FetchTrustedRootWithOptions(tufOptions(ctx, dir, hc, true)); ferr == nil {
			return tr, nil
		}
	}
	return nil, fmt.Errorf("%w: Sigstore's trust root: %w", ErrUnreachable, err)
}

func tufOptions(ctx context.Context, dir string, hc *http.Client, forceCache bool) *tuf.Options {
	o := tuf.DefaultOptions().WithCachePath(dir).WithCacheValidity(1).WithContext(ctx)
	o.ForceCache = forceCache
	// The TUF client starts from o.Root and writes it over the cached root.json. Started from the
	// embedded root, which is older than Sigstore's current one, it cannot verify the cached
	// timestamp and goes to the network every time. The cached root was verified along the root
	// chain when it was stored, so it is the one to start from.
	if b, err := os.ReadFile(filepath.Join(dir, tuf.URLToPath(o.RepositoryBaseURL), "root.json")); err == nil { //nolint:gosec // G304: our own TUF cache under the cache folder
		o.Root = b
	}
	f := fetcher.NewDefaultFetcher()
	f.SetHTTPClient(onlyTUF(ctx, hc))
	f.SetHTTPUserAgent("gremloader")
	o.Fetcher = f
	return o
}

// onlyTUF wraps hc so every request, redirects included, goes to TUFHost over https, bounded by
// tufTimeout and cancelled with ctx.
func onlyTUF(ctx context.Context, hc *http.Client) *http.Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c := *hc
	c.Timeout = tufTimeout
	c.Transport = tufTransport{ctx: ctx, base: base}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return allowedTUF(req)
	}
	return &c
}

type tufTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t tufTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := allowedTUF(req); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req.WithContext(t.ctx))
}

func allowedTUF(req *http.Request) error {
	if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Hostname(), TUFHost) || req.URL.Port() != "" {
		return fmt.Errorf("the trust root may only come from https://%s, not %s://%s", TUFHost, req.URL.Scheme, req.URL.Host)
	}
	return nil
}
