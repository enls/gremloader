package attest

import (
	"regexp"
	"testing"
)

// The SAN pattern decides which workflow may have built a release. Real bundles cannot show the
// edge cases, so they are checked against the pattern itself.
func TestSANPattern(t *testing.T) {
	const base = "https://github.com/o/app/.github/workflows/"
	for _, tc := range []struct {
		p    Policy
		san  string
		want bool
	}{
		{Policy{Repo: "o/app"}, base + "release.yml@refs/tags/v1", true},
		{Policy{Repo: "O/App"}, base + "release.yml@refs/tags/v1", true},
		{Policy{Repo: "o/app"}, "https://github.com/o/app-evil/.github/workflows/release.yml@refs/tags/v1", false},
		{Policy{Repo: "o/app"}, "https://github.com/other/app/.github/workflows/release.yml@refs/tags/v1", false},
		{Policy{Repo: "o/app"}, "https://evil.example/https://github.com/o/app/x", false},

		{Policy{Repo: "o/app", SignerWorkflow: ".github/workflows/release.yml"}, base + "release.yml@refs/tags/v1", true},
		{Policy{Repo: "o/app", SignerWorkflow: ".github/workflows/release.yml"}, base + "release.yml@0123abcd", true},
		{Policy{Repo: "o/app", SignerWorkflow: ".github/workflows/release.yml"}, base + "release.yml", true},
		{Policy{Repo: "o/app", SignerWorkflow: ".github/workflows/release.yml"}, base + "release.yml.evil.yml@refs/tags/v1", false},
		{Policy{Repo: "o/app", SignerWorkflow: ".github/workflows/release.yml"}, base + "Release.yml@refs/tags/v1", false},
		{Policy{Repo: "o/app", SignerWorkflow: ".github/workflows/release.yml"}, base + "release.yml@not-a-ref", false},

		{Policy{Repo: "o/app", SignerWorkflow: ".github/workflows/release.yml@refs/heads/main"}, base + "release.yml@refs/heads/main", true},
		{Policy{Repo: "o/app", SignerWorkflow: ".github/workflows/release.yml@refs/heads/main"}, base + "release.yml@refs/heads/mainline", false},
		{Policy{Repo: "o/app", SignerWorkflow: ".github/workflows/release.yml@refs/heads/main"}, base + "release.yml@refs/heads/main@refs/heads/x", false},
	} {
		if got := regexp.MustCompile(tc.p.SANPattern()).MatchString(tc.san); got != tc.want {
			t.Errorf("%+v against %s: %v, want %v (pattern %s)", tc.p, tc.san, got, tc.want, tc.p.SANPattern())
		}
	}
}

func TestValidate(t *testing.T) {
	for _, p := range []Policy{{Repo: "o/app"}, {Repo: "o-x/a.b_c"}, {Repo: "o/app", SignerWorkflow: ".github/workflows/r.yaml@refs/tags/v1"}} {
		if err := p.Validate(); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	for _, p := range []Policy{{Repo: "o"}, {Repo: "o/a/b"}, {Repo: "o/.."}, {Repo: "o/app", SignerWorkflow: "r.yml"},
		{Repo: "o/app", SignerWorkflow: ".github/workflows/../r.yml"}, {Repo: "o/app", SignerWorkflow: "Xgithub/workflows/r.yml"}} {
		if err := p.Validate(); err == nil {
			t.Errorf("%+v: no error", p)
		}
	}
	if None.String() != "none" || Passed.String() != "passed" || Failed.String() != "failed" {
		t.Error("Outcome.String")
	}
}
