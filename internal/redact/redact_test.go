package redact

import (
	"strings"
	"testing"
)

func TestURL(t *testing.T) {
	// The shape of a real release-asset redirect (values replaced).
	signed := "https://release-assets.githubusercontent.com/github-production-release-asset/1398080271/ee0fe0fa-1f41?sp=r&sv=2018-11-09&sig=SECRETSIG&jwt=eyJSECRET&response-content-disposition=attachment%3B%20filename%3Dx.zip"
	cases := map[string]string{
		signed: "https://release-assets.githubusercontent.com/github-production-release-asset/1398080271/ee0fe0fa-1f41",
		"https://github.com/o/r/releases/download/v1/a.zip":      "https://github.com/o/r/releases/download/v1/a.zip",
		"https://user:pass@example.com/a?token=abc#frag":         "https://redacted@example.com/a",
		"https://api.github.com/repos/o/r/releases?per_page=100": "https://api.github.com/repos/o/r/releases",
		"not a url?secret=1": "not a url",
		"relative/path#x":    "relative/path",
		"":                   "",
	}
	for in, want := range cases {
		got := URL(in)
		if got != want {
			t.Errorf("URL(%q) = %q, want %q", in, got, want)
		}
		for _, secret := range []string{"SECRET", "pass", "token=", "sig="} {
			if strings.Contains(got, secret) {
				t.Errorf("URL(%q) still contains %q: %q", in, secret, got)
			}
		}
	}
}

func TestText(t *testing.T) {
	in := `GET https://x.example/a?sig=SECRETSIG failed: Get "https://y.example/b?jwt=SECRETJWT": EOF (see http://z.example/c#SECRETFRAG)`
	got := Text(in)
	for _, secret := range []string{"SECRETSIG", "SECRETJWT", "SECRETFRAG"} {
		if strings.Contains(got, secret) {
			t.Errorf("Text left %q in %q", secret, got)
		}
	}
	if !strings.Contains(got, "https://x.example/a") || !strings.Contains(got, "failed:") {
		t.Errorf("Text mangled the message: %q", got)
	}
}

func TestToken(t *testing.T) {
	if Token("") != "" || Token("ghp_abcdef") != "***" {
		t.Error("Token")
	}
}

func FuzzURL(f *testing.F) {
	f.Add("https://a.example/b?c=d#e")
	f.Add("%%%")
	f.Add("ftp://u:p@h/x")
	f.Fuzz(func(t *testing.T, s string) {
		got := URL(s)
		if strings.ContainsAny(got, "?#") {
			t.Fatalf("URL(%q) = %q still has a raw ? or #", s, got)
		}
	})
}

// Credentials are not always written as scheme://user:password@host. Each of these leaked: the
// first parses with the user name as the scheme, the second has no scheme, the third no slashes
// at all, and the fourth has a raw "/" in the password, which ends the authority early.
func TestCredentialsWithoutSchemeSlashSlashAreRedacted(t *testing.T) {
	for _, in := range []string{
		"user:hunter2@host.example/x",
		"//user:hunter2@host.example/x?sig=1",
		"https:user:hunter2@host.example",
		"https://user:hun/ter2@host.example/x",
	} {
		got := URL(in)
		for _, secret := range []string{"hunter2", "hun/ter2", "user", "sig="} {
			if strings.Contains(got, secret) {
				t.Errorf("URL(%q) = %q still contains %q", in, got, secret)
			}
		}
		if !strings.Contains(got, "host.example") {
			t.Errorf("URL(%q) = %q lost the host", in, got)
		}
	}
}

// A URL that does not parse still must not give its credentials away: the fallback used to cut
// only the query string and returned the userinfo untouched.
func TestUnparsableURLStillLosesItsCredentials(t *testing.T) {
	for _, in := range []string{
		"https://user:p%zzw@host.example/x?sig=1",   // bad escape in the password
		"http://user:hunter2@[::1/x#frag",           // unterminated IPv6 host
		"https://user:hunter2#x@host.example/p",     // raw # in the password
		"https://user:hunter2@host.example/a b?q=1", // space in the path
	} {
		got := URL(in)
		for _, secret := range []string{"p%zzw", "hunter2", "user:", "sig=", "q=1", "frag"} {
			if strings.Contains(got, secret) {
				t.Errorf("URL(%q) = %q still contains %q", in, got, secret)
			}
		}
		if !strings.Contains(got, "://") {
			t.Errorf("URL(%q) = %q lost its shape", in, got)
		}
	}
}
