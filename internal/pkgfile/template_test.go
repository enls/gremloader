package pkgfile

import (
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestTemplateSyntax(t *testing.T) {
	user := set("root")
	ok := map[string]string{
		"plain.zip":            "plain.zip",
		"{name}-{version}.zip": "photocraft-0.6.0.zip",
		"{Name}":               "Photocraft",
		"{{literal}}-{name}":   "{literal}-photocraft",
		"a{{b":                 "a{b",
		"a}}b":                 "a}b",
		"{{{name}}}":           "{photocraft}",
		"{root}/x":             "/r/x",
		"{tag}_{os}_{arch}":    "v0.6.0_windows_amd64",
		"ünïcode-{name}":       "ünïcode-photocraft",
		"":                     "",
	}
	vals := map[string]string{"name": "photocraft", "Name": "Photocraft", "version": "0.6.0", "tag": "v0.6.0",
		"os": "windows", "arch": "amd64", "root": "/r"}
	for src, want := range ok {
		tpl, err := parseTemplate(src, forAsset, user)
		if err != nil {
			t.Errorf("parse %q: %v", src, err)
			continue
		}
		got, err := tpl.expand(vals, nil)
		if err != nil || got != want {
			t.Errorf("expand %q = %q, %v; want %q", src, got, err, want)
		}
		if tpl.String() != src {
			t.Errorf("String() = %q, want the source %q", tpl.String(), src)
		}
	}

	bad := map[string]string{
		"{name":                   "not closed",
		"name}":                   "lone '}'",
		"{}":                      "not a variable name",
		"{na me}":                 "not a variable name",
		"{1x}":                    "not a variable name",
		"{nam}":                   "unknown variable {nam}",
		"{asset}":                 "cannot be used here",
		"{a{name}}":               "not a variable name",
		"\xff":                    "UTF-8",
		strings.Repeat("x", 2000): "longer than",
	}
	for src, want := range bad {
		if _, err := parseTemplate(src, forAsset, user); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parse %q = %v, want an error containing %q", src, err, want)
		}
	}
}

// Which variables each kind of setting may use.
func TestTemplateVariablesPerSetting(t *testing.T) {
	for _, c := range []struct {
		allowed map[string]bool
		src     string
		ok      bool
	}{
		{forDir, "{root}/{name}", true},
		{forDir, "/opt/{name}-{version}", false}, // an install dir that moves with every update
		{forDir, "/opt/{tag}", false},
		{forExe, "{name}.exe", true},
		{forExe, "{version}/x.exe", false},
		{forChecksums, "{asset}.sha256", true},
		{forAsset, "{asset}", false},
		{forHook, "{version}", true},
		{forHook, "{asset}", false},
	} {
		_, err := parseTemplate(c.src, c.allowed, set("root"))
		if (err == nil) != c.ok {
			t.Errorf("%q: err = %v, want ok=%v", c.src, err, c.ok)
		}
	}
}

// The "escaping" rules: a substituted value is text, never a template, never a wildcard and
// never a path in a file name.
func TestSubstitutedValuesAreNotReinterpreted(t *testing.T) {
	main := `schema_version = 1
[vars]
weird = "{version}"
star = "*"
sub = "a/b"
space = "it's here"
[package.x]
repo = "o/x"
checksums = "{asset}.sha256"
asset = { "windows/amd64" = "{name}-{weird}.zip", "linux/amd64" = "{name}-{star}.zip", "linux/arm64" = "{sub}.zip", "darwin/arm64" = "x-*.zip" }
dir = { linux = "/opt/{weird}", darwin = "/opt/{weird}", windows = 'C:\opt\{weird}' }
post_install = [["/bin/echo", "{space}", "{weird}", "{version}"]]
`
	c := mustLoad(t, main, "")
	p := c.Packages["x"]
	v := Values{Version: "1.0.0", Tag: "v1.0.0", Platform: winAmd64}

	// A value that looks like a template stays literal.
	if s, err := p.AssetPattern(v); err != nil || s != "x-{version}.zip" {
		t.Errorf("asset = %q, %v; want the variable's text, not re-expanded", s, err)
	}
	// A value that would turn a name into a wildcard or a path is refused.
	v.Platform = linuxAmd64
	if s, err := p.AssetPattern(v); err == nil {
		t.Errorf("asset with a substituted '*' = %q, want an error", s)
	}
	v.Platform.Arch = "arm64"
	if s, err := p.AssetPattern(v); err == nil {
		t.Errorf("asset with a substituted '/' = %q, want an error", s)
	}
	// A wildcard the author wrote is theirs to write.
	v.Platform.OS = "darwin"
	if s, err := p.AssetPattern(v); err != nil || s != "x-*.zip" {
		t.Errorf("authored wildcard = %q, %v", s, err)
	}
	// The checksum file name from an asset name.
	if s, _, err := p.ChecksumsName(v, "x-1.zip"); err != nil || s != "x-1.zip.sha256" {
		t.Errorf("checksums = %q, %v", s, err)
	}
	if s, _, err := p.ChecksumsName(v, "../x.zip"); err == nil {
		t.Errorf("checksums from an asset with a slash = %q, want an error", s)
	}
	if s, _, err := p.ChecksumsName(v, "x-*.zip"); err == nil {
		t.Errorf("checksums from a wildcard asset = %q, want an error", s)
	}
	// Hook arguments are argv elements: a space or an apostrophe is not a word boundary, and
	// there is no shell to escape for.
	h, err := p.Hooks(PostInstall, Values{Version: "1.0.0", Platform: linuxAmd64})
	if err != nil || !reflect.DeepEqual(h, [][]string{{"/bin/echo", "it's here", "{version}", "1.0.0"}}) {
		t.Errorf("hooks = %q, %v", h, err)
	}
	// dir: the variable is text, and the result must be absolute.
	if d, err := p.Dir(runtime.GOOS, ""); err != nil || !strings.HasSuffix(d, "{version}") {
		t.Errorf("dir = %q, %v", d, err)
	}
}

func TestExpansionChecksTheResult(t *testing.T) {
	c := mustLoad(t, `schema_version = 1
[vars]
rel = "relative/dir"
ctl = "a`+"\\u0007"+`b"
[package.x]
repo = "o/x"
checksums = "s"
asset = "{ctl}.zip"
dir = { linux = "{rel}", windows = "~/Apps" }
version_from_exe = { linux = "../outside", windows = "bin/{name}.exe" }
`, "")
	p := c.Packages["x"]
	if _, err := p.Dir("linux", "/home/ian"); err == nil || !strings.Contains(err.Error(), "not an absolute path") {
		t.Errorf("relative dir: %v", err)
	}
	if _, err := p.Dir("windows", ""); err == nil || !strings.Contains(err.Error(), "home directory") {
		t.Errorf("~ without a home: %v", err)
	}
	if _, err := p.Dir("darwin", "/Users/ian"); err == nil {
		t.Error("dir for an OS with no entry succeeded")
	}
	if _, _, err := p.VersionFromExe("linux"); err == nil {
		t.Error("version_from_exe outside the install dir accepted")
	}
	if rel, ok, err := p.VersionFromExe("windows"); err != nil || !ok || !strings.HasSuffix(rel, "x.exe") {
		t.Errorf("version_from_exe = %q %v %v", rel, ok, err)
	}
	if _, err := p.AssetPattern(Values{Platform: winAmd64}); err == nil {
		t.Error("asset with a control character accepted")
	}
}

func TestVersionNeededButNotGiven(t *testing.T) {
	c := mustLoad(t, craftFile, "")
	if _, err := c.Packages["photocraft"].AssetPattern(Values{Platform: winAmd64}); err == nil || !strings.Contains(err.Error(), "{version}") {
		t.Errorf("asset without a version: %v", err)
	}
}

func TestVariables(t *testing.T) {
	_, err := load(t, "schema_version = 1\n[vars]\nversion = \"x\"\n\"bad name\" = \"y\"\n", "", Options{})
	wantProblem(t, err, 3, "built in")
	wantProblem(t, err, 4, "variable name")

	// The command line (--root) beats [vars], and may define a variable the file does not.
	c, err := load(t, craftFile, "", Options{Vars: map[string]string{"root": `D:\Copy`, "extra": "e"}})
	if err != nil {
		t.Fatal(err)
	}
	h, _ := c.Packages["photocraft"].Hooks(PostInstall, Values{Platform: winAmd64})
	if len(h) != 1 || h[0][3] != `D:\Copy\_update\register-apps.ps1` {
		t.Errorf("hooks with --root = %q", h)
	}
	if c.Vars["extra"] != "e" {
		t.Errorf("vars = %v", c.Vars)
	}
	_, err = load(t, craftFile, "", Options{Vars: map[string]string{"name": "x"}})
	wantProblem(t, err, 0, "command line", "built in")
}

func TestCapitalize(t *testing.T) {
	for in, want := range map[string]string{"photocraft": "Photocraft", "x": "X", "": "", "1abc": "1abc", "éa": "Éa"} {
		if got := capitalize(in); got != want {
			t.Errorf("capitalize(%q) = %q", in, got)
		}
	}
}
