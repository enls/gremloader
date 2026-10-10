// Package download is the only code that talks to the network.
//
// It enforces, in one place, the rules every request has to obey: https only; a host allow-list
// for the first URL and for every redirect hop; at most five redirects; the token goes only to the
// hosts in Policy.TokenHosts and is removed again by us, not by the HTTP library, when a redirect
// leaves them; timeouts for connect, TLS and response headers plus a stall timeout on the body;
// three attempts with exponential backoff and jitter on network errors, 5xx and rate-limit
// answers, honouring Retry-After up to a cap; and a size cap on every body. Errors never carry a
// query string, because GitHub's signed asset URLs carry their credential there.
//
// Downloads are sequential (one request at a time per Client) and are not resumed: an attempt
// that fails starts again from byte zero. See ADR 0009.
package download

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/enls/gremloader/internal/clock"
	"github.com/enls/gremloader/internal/redact"
	"github.com/enls/gremloader/internal/version"
)

const (
	defaultMaxRedirects = 5
	defaultAttempts     = 3
	defaultStall        = 30 * time.Second
	defaultBackoff      = 500 * time.Millisecond
	defaultMaxRetryWait = 60 * time.Second
	defaultGetLimit     = 16 << 20
)

// Config configures a Client. Only Policy is required; every other zero value means "the default".
type Config struct {
	Policy Policy
	// Token is sent as a bearer token to Policy.TokenHosts only. Empty means unauthenticated.
	Token string
	// UserAgent defaults to "gremloader/<version>".
	UserAgent string
	// HTTPClient supplies the transport (tests use this). The default has 10 s connect, 10 s TLS
	// and 30 s response-header timeouts and takes the proxy from the environment. Its CheckRedirect
	// is replaced; TLS verification is never configurable here.
	HTTPClient *http.Client

	MaxRedirects int           // default 5
	Attempts     int           // default 3
	StallTimeout time.Duration // default 30 s: no progress for this long fails the attempt
	BaseBackoff  time.Duration // default 500 ms, doubled per attempt, plus up to 50% jitter
	MaxRetryWait time.Duration // cap on honouring Retry-After, default 60 s

	Clock  clock.Clock                                      // default the system clock (Retry-After dates)
	Sleep  func(ctx context.Context, d time.Duration) error // default a context-aware sleep
	Jitter func(max time.Duration) time.Duration            // returns a value in [0, max]; default random
}

// Client performs requests under a Policy. It is safe to share but serialises requests.
type Client struct {
	cfg      Config
	follow   *http.Client
	noFollow *http.Client
	mu       sync.Mutex
}

// New returns a Client.
func New(cfg Config) *Client {
	if cfg.MaxRedirects <= 0 {
		cfg.MaxRedirects = defaultMaxRedirects
	}
	if cfg.Attempts <= 0 {
		cfg.Attempts = defaultAttempts
	}
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = defaultStall
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = defaultBackoff
	}
	if cfg.MaxRetryWait <= 0 {
		cfg.MaxRetryWait = defaultMaxRetryWait
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "gremloader/" + version.Version
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System{}
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	if cfg.Jitter == nil {
		cfg.Jitter = jitter
	}

	c := &Client{cfg: cfg}
	base := cfg.HTTPClient
	if base == nil {
		base = &http.Client{Transport: newTransport()}
	}
	f := *base
	f.CheckRedirect = c.checkRedirect
	n := *base
	n.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.follow, c.noFollow = &f, &n
	return c
}

func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = http.ProxyFromEnvironment
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = 30 * time.Second
	return t
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return rand.N(max + 1) //nolint:gosec // retry spacing, not security-sensitive
}

// HasToken reports whether a token is configured.
func (c *Client) HasToken() bool { return c.cfg.Token != "" }

// Allowed reports whether raw may be requested under the policy (a *PolicyError if not). Callers
// that follow a redirect by hand use it on the target.
func (c *Client) Allowed(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return &PolicyError{URL: redact.URL(raw), Reason: "not a valid URL"}
	}
	if err := c.cfg.Policy.check(u); err != nil {
		return &PolicyError{URL: redact.URL(raw), Reason: err.Error()}
	}
	return nil
}

func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > c.cfg.MaxRedirects {
		return &PolicyError{URL: redact.URL(req.URL.String()), Redirect: true,
			Reason: fmt.Sprintf("more than %d redirects", c.cfg.MaxRedirects)}
	}
	if err := c.cfg.Policy.check(req.URL); err != nil {
		return &PolicyError{URL: redact.URL(req.URL.String()), Redirect: true, Reason: err.Error()}
	}
	// The library copies the original headers across a redirect when the host name matches, which
	// includes a different port on the same machine. Decide again, per hop.
	req.Header.Del("Authorization")
	c.addToken(req)
	return nil
}

func (c *Client) addToken(req *http.Request) {
	if c.cfg.Token != "" && c.cfg.Policy.tokenAllowed(req.URL) {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
}

// Request is one GET.
type Request struct {
	URL    string
	Header http.Header // extra headers, e.g. Accept
	// MaxBytes caps the body of Get (default 16 MiB). Fetch takes its cap from FetchOptions.
	MaxBytes int64
	// NoFollow returns a 3xx answer to the caller instead of following it (the caller must check
	// the Location with Allowed).
	NoFollow bool
}

// Response is a fully read answer.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Get fetches a small body into memory. 2xx is success (and 3xx when NoFollow is set); any other
// status is an error: *RateLimitError, or *StatusError.
func (c *Client) Get(ctx context.Context, rq Request) (*Response, error) {
	limit := rq.MaxBytes
	if limit <= 0 {
		limit = defaultGetLimit
	}
	var out *Response
	err := c.run(ctx, rq, func(resp *http.Response, body io.Reader) error {
		if resp.ContentLength > limit {
			return fmt.Errorf("%w (%d bytes announced, limit %d)", ErrTooLarge, resp.ContentLength, limit)
		}
		b, err := io.ReadAll(io.LimitReader(body, limit+1))
		if err != nil {
			return err
		}
		if int64(len(b)) > limit {
			return fmt.Errorf("%w (limit %d)", ErrTooLarge, limit)
		}
		out = &Response{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: b}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FetchOptions configures Fetch.
type FetchOptions struct {
	// MaxBytes is the largest body accepted; required.
	MaxBytes int64
	Header   http.Header
}

// Result describes a completed Fetch.
type Result struct {
	SHA256 string // lower-case hex, computed while streaming
	Size   int64
}

// Fetch streams a body to dst. The bytes are hashed as they arrive and written to dst+".part",
// which replaces dst only after a complete, size-checked transfer; on any failure nothing is
// left behind. dst is chosen by the caller, never taken from the network. The caller compares the
// returned SHA256 with the checksum it trusts.
func (c *Client) Fetch(ctx context.Context, rawURL, dst string, o FetchOptions) (Result, error) {
	if o.MaxBytes <= 0 {
		return Result{}, errors.New("download: Fetch needs a positive MaxBytes")
	}
	var res Result
	tmp := dst + ".part"
	err := c.run(ctx, Request{URL: rawURL, Header: o.Header}, func(resp *http.Response, body io.Reader) (err error) {
		if resp.ContentLength > o.MaxBytes {
			return fmt.Errorf("%w (%d bytes announced, limit %d)", ErrTooLarge, resp.ContentLength, o.MaxBytes)
		}
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // dst is chosen by the engine, not the network
		if err != nil {
			return &writeError{err}
		}
		defer func() {
			if err != nil {
				_ = f.Close()
				_ = os.Remove(tmp)
			}
		}()

		h := sha256.New()
		var n int64
		buf := make([]byte, 64<<10)
		for {
			nr, rerr := body.Read(buf)
			if nr > 0 {
				n += int64(nr)
				if n > o.MaxBytes {
					return fmt.Errorf("%w (limit %d)", ErrTooLarge, o.MaxBytes)
				}
				h.Write(buf[:nr])
				if _, werr := f.Write(buf[:nr]); werr != nil {
					return &writeError{werr}
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				return rerr
			}
		}
		if resp.ContentLength >= 0 && n != resp.ContentLength {
			return io.ErrUnexpectedEOF
		}
		if err := f.Close(); err != nil {
			return &writeError{err}
		}
		if err := os.Rename(tmp, dst); err != nil {
			return &writeError{err}
		}
		res = Result{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

// run performs rq with retries. handle is given a successful response and its (stall-watched)
// body, and is called once per attempt that got one.
func (c *Client) run(ctx context.Context, rq Request, handle func(*http.Response, io.Reader) error) error {
	u, err := url.Parse(rq.URL)
	if err != nil {
		return &PolicyError{URL: redact.URL(rq.URL), Reason: "not a valid URL"}
	}
	if err := c.cfg.Policy.check(u); err != nil {
		return &PolicyError{URL: redact.URL(rq.URL), Reason: err.Error()}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for attempt := 1; ; attempt++ {
		o := c.once(ctx, u, rq, handle)
		if o.err == nil {
			return nil
		}
		if !o.retry || attempt >= c.cfg.Attempts {
			return o.err
		}
		wait := o.wait
		if wait <= 0 {
			d := c.cfg.BaseBackoff << (attempt - 1)
			wait = d + c.cfg.Jitter(d/2)
		}
		if serr := c.cfg.Sleep(ctx, wait); serr != nil {
			return serr
		}
	}
}

// outcome is the result of one attempt: err is nil on success; retry says whether trying again
// could help; wait is the server's requested pause (already capped), if it gave one.
type outcome struct {
	err   error
	retry bool
	wait  time.Duration
}

func fail(err error) outcome { return outcome{err: err} }

func retryable(err error) outcome { return outcome{err: err, retry: true} }

// once makes one attempt.
func (c *Client) once(ctx context.Context, u *url.URL, rq Request, handle func(*http.Response, io.Reader) error) outcome {
	shown := redact.URL(u.String())

	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	timer := time.AfterFunc(c.cfg.StallTimeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer timer.Stop()

	req, err := http.NewRequestWithContext(actx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fail(&PolicyError{URL: shown, Reason: "not a valid request"})
	}
	for k, vs := range rq.Header {
		req.Header[k] = append([]string(nil), vs...)
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	c.addToken(req)

	cl := c.follow
	if rq.NoFollow {
		cl = c.noFollow
	}
	resp, err := cl.Do(req)
	if err != nil {
		return c.networkError(ctx, &stalled, shown, err)
	}
	defer resp.Body.Close()

	code := resp.StatusCode
	if (code >= 200 && code < 300) || (rq.NoFollow && code >= 300 && code < 400) {
		body := &progressReader{r: resp.Body, timer: timer, d: c.cfg.StallTimeout}
		if herr := handle(resp, body); herr != nil {
			return c.handlerError(ctx, &stalled, shown, herr)
		}
		return outcome{}
	}
	return c.statusError(resp, shown)
}

func (c *Client) networkError(ctx context.Context, stalled *atomic.Bool, shown string, err error) outcome {
	var pe *PolicyError
	if errors.As(err, &pe) {
		return fail(pe)
	}
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}
	if stalled.Load() {
		return retryable(fmt.Errorf("GET %s: %w", shown, ErrStalled))
	}
	// A *url.Error repeats the full URL, query string included; keep only what went wrong.
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var cert *tls.CertificateVerificationError
	if errors.As(err, &cert) {
		return fail(fmt.Errorf("GET %s: %w", shown, err))
	}
	return retryable(fmt.Errorf("GET %s: %w", shown, err))
}

func (c *Client) handlerError(ctx context.Context, stalled *atomic.Bool, shown string, err error) outcome {
	var we *writeError
	switch {
	case ctx.Err() != nil:
		return fail(ctx.Err())
	case stalled.Load():
		return retryable(fmt.Errorf("GET %s: %w", shown, ErrStalled))
	case errors.Is(err, ErrTooLarge):
		return fail(fmt.Errorf("GET %s: %w", shown, err))
	case errors.As(err, &we):
		return fail(fmt.Errorf("saving download: %w", we.err))
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return retryable(fmt.Errorf("GET %s: %w", shown, err))
}

func (c *Client) statusError(resp *http.Response, shown string) outcome {
	code := resp.StatusCode
	h := resp.Header
	retryAfter := c.retryAfter(h.Get("Retry-After"))
	capped := min(retryAfter, c.cfg.MaxRetryWait)

	limited := code == http.StatusTooManyRequests ||
		(code == http.StatusForbidden && (h.Get("Retry-After") != "" || h.Get("X-RateLimit-Remaining") == "0"))
	if limited {
		rl := &RateLimitError{URL: shown, Status: code, RetryAfter: retryAfter}
		if v, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			rl.Reset = time.Unix(v, 0)
		}
		// An exhausted hourly quota will not clear within a run: no point waiting, and the caller
		// should fall over to another path.
		if h.Get("Retry-After") == "" && h.Get("X-RateLimit-Remaining") == "0" {
			rl.Primary = true
			return fail(rl)
		}
		return outcome{err: rl, retry: true, wait: capped}
	}
	se := &StatusError{URL: shown, Code: code}
	if code >= 500 {
		return outcome{err: se, retry: true, wait: capped}
	}
	return fail(se)
}

// retryAfter parses a Retry-After header: delay-seconds or an HTTP date. Unusable means zero.
func (c *Client) retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(c.cfg.Clock.Now()); d > 0 {
			return d
		}
	}
	return 0
}

// progressReader restarts the stall timer whenever data arrives.
type progressReader struct {
	r     io.Reader
	timer *time.Timer
	d     time.Duration
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.timer.Reset(p.d)
	}
	return n, err
}
