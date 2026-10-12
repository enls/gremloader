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
// parse as a URL is returned with everything after the first "?" or "#" removed and, if it has the
// shape scheme://user:password@host, without the user:password part.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return cutAt(dropUserinfo(raw), "?#") // userinfo first: a password may hold a raw ? or #
	}
	if strings.Contains(u.Opaque, "@") {
		// "user:pass@host/x" parses with the user name as the scheme, and "https:user:pass@host"
		// has no slashes: no userinfo is recognized, so whatever precedes the last @ is dropped.
		return cutAt("redacted"+raw[strings.LastIndexByte(raw, '@'):], "?#")
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

// dropUserinfo replaces the user:pw part of a "scheme://user:pw@host" or "//user:pw@host" string
// that url.Parse would not accept or would not read as having a scheme (a bad escape, an
// unterminated IPv6 host, a raw "/" in the password), where nothing else could remove it. It
// looks for the last @ in the whole remainder, not only before the first "/": a password may
// contain one, and on a string that is already malformed, losing a host that is followed by an @
// in its path is the safer mistake than keeping a credential.
func dropUserinfo(s string) string {
	var prefix string
	switch i := strings.Index(s, "://"); {
	case i >= 0:
		prefix = s[:i+3]
	case strings.HasPrefix(s, "//"):
		prefix = "//"
	default:
		return s
	}
	rest := s[len(prefix):]
	if at := strings.LastIndexByte(rest, '@'); at >= 0 {
		return prefix + "redacted" + rest[at:]
	}
	return s
}

func cutAt(s, chars string) string {
	if i := strings.IndexAny(s, chars); i >= 0 {
		return s[:i]
	}
	return s
}
