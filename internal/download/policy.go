package download

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Policy says where requests may go and where the token may be sent.
//
// Everything that reaches the network goes through one Policy: the first URL and every redirect
// hop. The zero value allows nothing; start from GitHubPolicy.
type Policy struct {
	// Hosts are the host names (no port) a request may be sent to, matched case-insensitively. An
	// entry that starts with "*." matches any host below that domain but not the domain itself.
	Hosts []string
	// TokenHosts are the hosts (as in URL.Host: the port is included when the URL has one) that may
	// receive the Authorization header. Matching on host and port, not just the name, is deliberate:
	// Go's own redirect handling compares host names only and would forward the token to another
	// port of the same machine.
	TokenHosts []string
	// AllowHTTP permits plain http. It exists so tests can talk to a local fake server; the
	// command-line program never sets it.
	AllowHTTP bool
}

// GitHubPolicy is the policy of the real program: https only, GitHub's hosts, and the token
// only to the REST API (asset downloads from github.com are public and need no credential).
func GitHubPolicy() Policy {
	return Policy{
		Hosts:      []string{"github.com", "api.github.com", "*.githubusercontent.com"},
		TokenHosts: []string{"api.github.com"},
	}
}

// BundlePolicy is GitHubPolicy plus the Azure storage GitHub serves attestation bundles from
// (`bundle_url`, ADR 0014). It is for fetching bundles only, never assets; the token goes nowhere
// new. The bundle is verified cryptographically, so the host is reached, not trusted.
func BundlePolicy() Policy {
	p := GitHubPolicy()
	p.Hosts = append(p.Hosts, "*.blob.core.windows.net")
	return p
}

// check reports why u may not be requested, or nil.
func (p Policy) check(u *url.URL) error {
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && p.AllowHTTP:
	default:
		return fmt.Errorf("scheme %q is not allowed (https only)", u.Scheme)
	}
	if u.User != nil {
		return errors.New("credentials in the URL are not allowed")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return errors.New("URL has no host")
	}
	if !p.hostAllowed(host) {
		return fmt.Errorf("host %q is not on the allow-list", host)
	}
	return nil
}

func (p Policy) hostAllowed(host string) bool {
	for _, pat := range p.Hosts {
		pat = strings.ToLower(pat)
		if suffix, ok := strings.CutPrefix(pat, "*"); ok && strings.HasPrefix(suffix, ".") {
			if len(host) > len(suffix) && strings.HasSuffix(host, suffix) {
				return true
			}
			continue
		}
		if host == pat {
			return true
		}
	}
	return false
}

// tokenAllowed reports whether the Authorization header may be sent to u.
func (p Policy) tokenAllowed(u *url.URL) bool {
	if u.Scheme != "https" && (u.Scheme != "http" || !p.AllowHTTP) {
		return false
	}
	for _, h := range p.TokenHosts {
		if strings.EqualFold(u.Host, h) {
			return true
		}
	}
	return false
}

// Errors. URLs inside them are always redacted (no query string, no credentials): signed
// download URLs must never reach a log or a terminal.

// PolicyError means a URL was refused before any request was made for it: the first URL, or a
// redirect target.
type PolicyError struct {
	URL      string
	Redirect bool
	Reason   string
}

func (e *PolicyError) Error() string {
	if e.Redirect {
		return fmt.Sprintf("refusing redirect to %s: %s", e.URL, e.Reason)
	}
	return fmt.Sprintf("refusing %s: %s", e.URL, e.Reason)
}

// StatusError is a final HTTP failure: a status that is not retried, or one that stayed after
// the last attempt.
type StatusError struct {
	URL        string
	Code       int
	RetryAfter time.Duration // the server's requested pause; zero if none

	tooLong bool // RetryAfter was more than the client waits, so the request was not repeated
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d %s", e.URL, e.Code, http.StatusText(e.Code)) + pause(e.RetryAfter, e.tooLong)
}

// pause words a Retry-After for an error message, so the user can see why a run gave up.
func pause(d time.Duration, tooLong bool) string {
	if d <= 0 {
		return ""
	}
	if tooLong {
		return fmt.Sprintf(" (the server asked to retry after %v, longer than this run waits; not retried)", d)
	}
	return fmt.Sprintf(" (the server asked to retry after %v)", d)
}

// RateLimitError means GitHub refused because of a rate limit.
type RateLimitError struct {
	URL    string
	Status int
	// Primary is the hourly quota (nothing to wait for within a run); false is a secondary limit or
	// a 429 that asked for a pause and did not clear within the attempts.
	Primary    bool
	Reset      time.Time     // when the primary quota resets; zero if unknown
	RetryAfter time.Duration // the server's requested pause; zero if none

	tooLong bool // as in StatusError
}

func (e *RateLimitError) Error() string {
	kind := "secondary rate limit"
	if e.Primary {
		kind = "rate limit exceeded"
	}
	msg := fmt.Sprintf("GET %s: HTTP %d, %s", e.URL, e.Status, kind)
	if !e.Reset.IsZero() {
		msg += ", resets at " + e.Reset.UTC().Format(time.RFC3339)
	}
	return msg + pause(e.RetryAfter, e.tooLong)
}

var (
	// ErrTooLarge means a response exceeded the size the caller allowed.
	ErrTooLarge = errors.New("response is larger than the allowed size")
	// ErrStalled means no data arrived for the stall timeout.
	ErrStalled = errors.New("transfer stalled")
)

// writeError marks a failure of our own side (the destination file), which is never retried.
type writeError struct{ err error }

func (e *writeError) Error() string { return e.err.Error() }
func (e *writeError) Unwrap() error { return e.err }
