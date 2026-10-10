package ver

import (
	"math/rand"
	"slices"
	"strings"
	"testing"

	xsemver "golang.org/x/mod/semver"
)

func mustParse(t testing.TB, s string) Version {
	t.Helper()
	v, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return v
}

// The ordering example from the SemVer 2.0.0 specification, section 11.
func TestSpecPrecedenceChain(t *testing.T) {
	chain := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0",
		"2.0.0", "2.1.0", "2.1.1",
	}
	for i := range chain {
		for j := range chain {
			got := mustParse(t, chain[i]).Compare(mustParse(t, chain[j]))
			want := cmpInt(i, j)
			if got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", chain[i], chain[j], got, want)
			}
		}
	}
}

func TestBuildMetadataIgnoredForPrecedence(t *testing.T) {
	a, b := mustParse(t, "1.0.0+build.1"), mustParse(t, "1.0.0+other")
	if a.Compare(b) != 0 {
		t.Error("build metadata must not affect precedence")
	}
	if a.String() != "1.0.0+build.1" {
		t.Errorf("String() = %q", a.String())
	}
}

func TestNumericIdentifiersLongerThan64Bits(t *testing.T) {
	small := mustParse(t, "1.0.0-alpha.99999999999999999999")
	big := mustParse(t, "1.0.0-alpha.100000000000000000000")
	if small.Compare(big) != -1 || big.Compare(small) != 1 {
		t.Error("long numeric identifiers must compare as numbers")
	}
}

func TestStrictRejects(t *testing.T) {
	bad := []string{
		"", "1", "1.2", "1.2.3.4", "v1.2.3", "01.2.3", "1.02.3", "1.2.03",
		"1.2.3-", "1.2.3+", "1.2.3-01", "1.2.3-a..b", "1.2.3-é", "1.2.3 ",
		"-1.2.3", "1.2.-3", "a.b.c", "1.2.3-rc.01",
		"18446744073709551616.0.0", // 2^64
	}
	for _, s := range bad {
		if v, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) = %v, want error", s, v)
		}
	}
}

func TestStrictAccepts(t *testing.T) {
	good := map[string]string{
		"0.0.0":                    "0.0.0",
		"1.2.3":                    "1.2.3",
		"1.2.3-rc.5":               "1.2.3-rc.5",
		"1.2.3-alpha-1":            "1.2.3-alpha-1",
		"1.2.3+20260101":           "1.2.3+20260101",
		"1.2.3-rc.1+build.007":     "1.2.3-rc.1+build.007", // leading zero allowed in build
		"18446744073709551615.0.0": "18446744073709551615.0.0",
	}
	for in, want := range good {
		v, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}
		if v.String() != want {
			t.Errorf("Parse(%q).String() = %q, want %q", in, v.String(), want)
		}
	}
}

func TestLoose(t *testing.T) {
	ok := map[string]string{
		"v0.6.0":       "0.6.0",
		"V1.2.3":       "1.2.3",
		"1":            "1.0.0",
		"1.2":          "1.2.0",
		"v2":           "2.0.0",
		"0.6.0.0":      "0.6.0", // Windows ProductVersion
		"1.2.3.0":      "1.2.3",
		"v0.1.1-rc.5":  "0.1.1-rc.5",
		"1.2-beta":     "1.2.0-beta",
		"0.6.0.0+abc1": "0.6.0+abc1",
	}
	for in, want := range ok {
		v, err := ParseLoose(in)
		if err != nil {
			t.Errorf("ParseLoose(%q): %v", in, err)
			continue
		}
		if v.String() != want {
			t.Errorf("ParseLoose(%q) = %q, want %q", in, v.String(), want)
		}
	}
	bad := []string{"", "v", "1.2.3.4", "1.2.3.4.5", "vv1.0.0", "1..2", "x", "1.2.3.", "latest", "release-1"}
	for _, s := range bad {
		if v, err := ParseLoose(s); err == nil {
			t.Errorf("ParseLoose(%q) = %v, want error", s, v)
		}
	}
}

func TestFromTag(t *testing.T) {
	v, err := FromTag("v0.6.0", "v")
	if err != nil || v.String() != "0.6.0" {
		t.Fatalf("FromTag v0.6.0 = %v, %v", v, err)
	}
	if v, err := FromTag("release-1.2.0", "release-"); err != nil || v.String() != "1.2.0" {
		t.Errorf("custom prefix: %v, %v", v, err)
	}
	if v, err := FromTag("1.2.0", ""); err != nil || v.String() != "1.2.0" {
		t.Errorf("empty prefix: %v, %v", v, err)
	}
	// No prefix configured: the loose form applies in full, including the leading v. (Found by the
	// GitHub source tests: the first version of FromTag rejected every plain "v1.2.3" tag.)
	for tag, want := range map[string]string{"v1.2.3": "1.2.3", "V1.2.3": "1.2.3", "v0.1.1-rc.5": "0.1.1-rc.5", "v2": "2.0.0"} {
		if v, err := FromTag(tag, ""); err != nil || v.String() != want {
			t.Errorf("FromTag(%q, \"\") = %v, %v; want %s", tag, v, err, want)
		}
	}
	// A configured prefix is exact: "app-" does not also strip a "v".
	if v, err := FromTag("app-v1.2.3", "app-"); err == nil {
		t.Errorf("FromTag(app-v1.2.3, app-) = %v, want error", v)
	}
	if v, err := FromTag("vv1.2.3", "v"); err == nil {
		t.Errorf("FromTag(vv1.2.3, v) = %v, want error", v)
	}
	for _, tag := range []string{"1.2.0", "nightly", "v1.2.3/../x", "../../v1.0.0", "v1.0.0\n", ""} {
		if v, err := FromTag(tag, "v"); err == nil {
			t.Errorf("FromTag(%q) = %v, want error", tag, v)
		}
	}
}

func TestValidTag(t *testing.T) {
	good := []string{"v1.0.0", "0.6.0", "v0.1.1-rc.5", "build_7", "A1", "v1.0.0+meta", strings.Repeat("a", 128)}
	for _, s := range good {
		if !ValidTag(s) {
			t.Errorf("ValidTag(%q) = false", s)
		}
	}
	bad := []string{
		"", ".hidden", "-x", "+x", "_x", "a..b", "v1/2", `v1\2`, "v1:2", "v 1", "v1\x00", "v1\n",
		"../x", "..", "é", "v1?x=1", "v1#f", "v1%2e", strings.Repeat("a", 129),
	}
	for _, s := range bad {
		if ValidTag(s) {
			t.Errorf("ValidTag(%q) = true", s)
		}
	}
}

func TestIsPrerelease(t *testing.T) {
	if mustParse(t, "1.0.0").IsPrerelease() {
		t.Error("1.0.0 is not a pre-release")
	}
	if !mustParse(t, "1.0.0-rc.1").IsPrerelease() {
		t.Error("1.0.0-rc.1 is a pre-release")
	}
}

func TestSortingRealCraftTags(t *testing.T) {
	// Tags seen on storytold/photocraft (see testdata/fixtures).
	tags := []string{"v0.6.0", "v0.5.0", "v0.3.0", "v0.2.0", "v0.1.1", "v0.1.1-rc.5", "v0.1.1-rc.4", "v0.1.0"}
	var vs []Version
	for _, tg := range tags {
		v, err := FromTag(tg, "v")
		if err != nil {
			t.Fatal(err)
		}
		vs = append(vs, v)
	}
	r := rand.New(rand.NewSource(1))
	r.Shuffle(len(vs), func(i, j int) { vs[i], vs[j] = vs[j], vs[i] })
	slices.SortFunc(vs, func(a, b Version) int { return -Compare(a, b) }) // descending
	var got []string
	for _, v := range vs {
		got = append(got, "v"+v.String())
	}
	want := strings.Join(tags, " ")
	if strings.Join(got, " ") != want {
		t.Errorf("sorted = %v\nwant     %s", got, want)
	}
}

// Differential test: golang.org/x/mod/semver is a widely used, independently written
// implementation of the same rules. It is a test-only dependency used as an oracle.
func TestAgainstXModSemver(t *testing.T) {
	r := rand.New(rand.NewSource(20261010))
	pre := []string{"", "-alpha", "-alpha.1", "-alpha.beta", "-beta", "-beta.2", "-beta.11", "-rc.1", "-0", "-1", "-x-y", "-9.a"}
	build := []string{"", "+b1", "+007"}
	gen := func() string {
		return strings.Join([]string{
			itoa(r.Intn(4)), itoa(r.Intn(4)), itoa(r.Intn(4)),
		}, ".") + pre[r.Intn(len(pre))] + build[r.Intn(len(build))]
	}
	for i := 0; i < 5000; i++ {
		as, bs := gen(), gen()
		a, errA := Parse(as)
		b, errB := Parse(bs)
		if errA != nil || errB != nil {
			t.Fatalf("generator produced unparsable %q or %q: %v %v", as, bs, errA, errB)
		}
		want := xsemver.Compare("v"+as, "v"+bs)
		if got := a.Compare(b); got != want {
			t.Fatalf("Compare(%s, %s) = %d, x/mod/semver = %d", as, bs, got, want)
		}
	}
}

func itoa(n int) string { return string(rune('0' + n)) }

func FuzzParse(f *testing.F) {
	for _, s := range []string{"1.2.3", "v1.2.3-rc.1+b", "0.6.0.0", "1.0.0-alpha.beta", "", "1.2", "01.0.0", "1.2.3-"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, parse := range []func(string) (Version, error){Parse, ParseLoose} {
			v, err := parse(s)
			if err != nil {
				continue
			}
			// A successful parse must round-trip through the canonical form...
			again, err := Parse(v.String())
			if err != nil {
				t.Fatalf("canonical form %q of %q does not parse strictly: %v", v.String(), s, err)
			}
			if again.Compare(v) != 0 || again.String() != v.String() {
				t.Fatalf("round trip changed %q -> %q -> %q", s, v.String(), again.String())
			}
			// ...and comparison must be reflexive and antisymmetric.
			if v.Compare(v) != 0 || v.Compare(again) != -again.Compare(v) {
				t.Fatalf("comparison is inconsistent for %q", s)
			}
		}
	})
}
