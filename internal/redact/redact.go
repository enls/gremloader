// Package redact removes secrets from anything that is about to be logged or shown.
//
// GitHub's asset downloads redirect to signed, expiring URLs that carry their credential in the
// query string (sig=..., a JWT). Logging such a URL leaks a working download link, so every URL
// that reaches the event log goes through here: the query string and fragment are dropped, and any
// user:password part is replaced.
package redact

import (
	"net/url"
	"regexp"
	"strings"
)

// URL returns raw without its query string, fragment and credentials. A string that does not
// parse as a URL is returned with everything after the first "?" or "#" removed.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return cutAt(raw, "?#")
	}
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	if u.User != nil {
		u.User = url.User("redacted")
	}
	return u.String()
}

var urlInText = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)

// Text redacts every URL embedded in free text (error messages, hook output).
func Text(s string) string {
	return urlInText.ReplaceAllStringFunc(s, URL)
}

// Token masks a credential for display: only its length class is kept.
func Token(s string) string {
	if s == "" {
		return ""
	}
	return "***"
}

func cutAt(s, chars string) string {
	if i := strings.IndexAny(s, chars); i >= 0 {
		return s[:i]
	}
	return s
}
