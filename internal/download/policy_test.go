package download

import (
	"net/url"
	"testing"
)

func TestGitHubPolicyHosts(t *testing.T) {
	p := GitHubPolicy()
	cases := []struct {
		url  string
		want bool
	}{
		{"https://github.com/enls/gremloader/releases/download/v1/x.zip", true},
		{"https://api.github.com/repos/o/r/releases", true},
		{"https://release-assets.githubusercontent.com/x?sig=1", true},
		{"https://objects.githubusercontent.com/x", true},
		{"https://GITHUB.com/x", true},
		{"https://githubusercontent.com/x", false},  // the wildcard is for subdomains only
		{"https://.githubusercontent.com/x", false}, // a leading-dot name is not a subdomain either
		{"https://evilgithubusercontent.com/x", false},
		{"https://github.com.evil.example/x", false},
		{"https://raw.githubusercontent.com.evil.example/x", false},
		{"https://api.github.com.evil.example/x", false},
		{"https://evil.example/github.com", false},
		{"https://github.com./x", false},
		{"http://github.com/x", false}, // plain http
		{"ftp://github.com/x", false},
		{"https://user:pass@github.com/x", false},
		{"https:///x", false},
	}
	for _, c := range cases {
		u, err := url.Parse(c.url)
		if err != nil {
			t.Fatalf("parse %q: %v", c.url, err)
		}
		if got := p.check(u) == nil; got != c.want {
			t.Errorf("allowed(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

func TestGitHubPolicyTokenHosts(t *testing.T) {
	p := GitHubPolicy()
	cases := []struct {
		url  string
		want bool
	}{
		{"https://api.github.com/repos/o/r/releases", true},
		{"https://github.com/o/r/releases/download/v1/x", false},
		{"https://release-assets.githubusercontent.com/x", false},
		{"https://api.github.com:8443/x", false}, // another port of the same name is another service
		{"http://api.github.com/x", false},
	}
	for _, c := range cases {
		u, _ := url.Parse(c.url)
		if got := p.tokenAllowed(u); got != c.want {
			t.Errorf("tokenAllowed(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

// BundlePolicy adds GitHub's bundle storage (ADR 0014) and nothing else; the token still goes to
// the API only.
func TestBundlePolicy(t *testing.T) {
	p := BundlePolicy()
	for raw, want := range map[string]bool{
		"https://tmaproduction.blob.core.windows.net/attestations/1/x.json.sn?sig=1": true,
		"https://other.blob.core.windows.net/x":                                      true,
		"https://api.github.com/repos/o/r/attestations/sha256:x":                     true,
		"https://blob.core.windows.net/x":                                            false,
		"https://x.blob.core.windows.net.evil.example/x":                             false,
		"http://tmaproduction.blob.core.windows.net/x":                               false,
		"https://evil.example/x":                                                     false,
	} {
		u, _ := url.Parse(raw)
		if got := p.check(u) == nil; got != want {
			t.Errorf("allowed(%q) = %v, want %v", raw, got, want)
		}
		if p.tokenAllowed(u) && u.Host != "api.github.com" {
			t.Errorf("token allowed for %q", raw)
		}
	}
	// GitHubPolicy, used for assets, is not widened by it.
	u, _ := url.Parse("https://tmaproduction.blob.core.windows.net/x")
	if GitHubPolicy().check(u) == nil {
		t.Error("GitHubPolicy allows the bundle storage")
	}
}

func TestZeroPolicyAllowsNothing(t *testing.T) {
	u, _ := url.Parse("https://github.com/x")
	if (Policy{}).check(u) == nil {
		t.Error("the zero Policy allowed a request")
	}
}
