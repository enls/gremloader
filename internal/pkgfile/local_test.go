package pkgfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalOverrideApplies(t *testing.T) {
	c := mustLoad(t, mergeFile, `schema_version = 1

[settings]
channel = "prerelease"
cooldown_days = 1

[package.a]
pin = "2.0.0-rc.1"
hold = true
enabled = false

[package.b]
cooldown_days = 9
`)
	a, b, cc := c.Packages["a"], c.Packages["b"], c.Packages["c"]
	if a.Pin != "2.0.0-rc.1" || !a.Hold || a.Enabled {
		t.Errorf("a = pin %q hold %v enabled %v", a.Pin, a.Hold, a.Enabled)
	}
	if b.CooldownDays != 9 {
		t.Errorf("b cooldown = %d, want the local package value 9", b.CooldownDays)
	}
	// Local [settings] beats the file's [settings], but not a preset or package value.
	if cc.CooldownDays != 1 || a.CooldownDays != 3 {
		t.Errorf("cooldown c=%d a=%d, want 1 (local settings) and 3 (preset)", cc.CooldownDays, a.CooldownDays)
	}
	if cc.Channel != "prerelease" {
		t.Errorf("c channel = %q", cc.Channel)
	}
	if c.LocalFile != "gremloader.local.toml" {
		t.Errorf("LocalFile = %q", c.LocalFile)
	}
}

// The whitelist: nothing in the local file may change what runs, where, or from where.
func TestLocalOverrideWhitelist(t *testing.T) {
	_, err := load(t, mergeFile, `schema_version = 1

[settings]
api = "web"

[package.a]
pin = "1.0.0"
dir = "C:/Elsewhere"
repo = "evil/fork"
post_install = [["calc.exe"]]
asset = "evil.zip"
verify = { require = [] }

[vars]
root = "D:/"

[preset.base]
asset = "x"
`, Options{})
	for line, key := range map[int]string{4: "settings.api", 8: "package.a.dir", 9: "package.a.repo", 10: "package.a.post_install",
		11: "package.a.asset", 12: "package.a.verify", 14: "vars", 17: "preset"} {
		wantProblem(t, err, line, "gremloader.local.toml", key, "not allowed in the local override file")
	}
}

func TestLocalValuesAreValidated(t *testing.T) {
	_, err := load(t, mergeFile, "schema_version = 1\n[package.a]\npin = \"newest\"\nchannel = \"nightly\"\n[settings]\ncooldown_days = 99999\n", Options{})
	wantProblem(t, err, 3, "gremloader.local.toml", "pin")
	wantProblem(t, err, 4, "gremloader.local.toml", `"nightly"`)
	wantProblem(t, err, 6, "gremloader.local.toml", "3650")
}

func TestLocalPackageNotInTheFileIsAWarning(t *testing.T) {
	c := mustLoad(t, mergeFile, "schema_version = 1\n\n[package.removed]\npin = \"1.0.0\"\n")
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "gremloader.local.toml:3: [package.removed] is not in gremloader.toml") {
		t.Errorf("warnings = %q", c.Warnings)
	}
	if _, ok := c.Packages["removed"]; ok {
		t.Error("a package that exists only in the local file was created")
	}
}

func TestLoadReadsTheLocalFileBesideThePackageFile(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "gremloader.toml")
	write(t, main, mergeFile)

	c, err := Load(main, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c.LocalFile != "" || c.Packages["a"].Hold {
		t.Errorf("without a local file: LocalFile %q, hold %v", c.LocalFile, c.Packages["a"].Hold)
	}

	write(t, filepath.Join(dir, "gremloader.local.toml"), "schema_version = 1\n[package.a]\nhold = true\n")
	c, err = Load(main, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c.LocalFile != filepath.Join(dir, "gremloader.local.toml") || !c.Packages["a"].Hold {
		t.Errorf("with a local file: LocalFile %q, hold %v", c.LocalFile, c.Packages["a"].Hold)
	}

	if _, err := Load(filepath.Join(dir, "missing.toml"), Options{}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load of a missing file = %v", err)
	}
	big := filepath.Join(dir, "big.toml")
	write(t, big, "schema_version = 1\n#"+strings.Repeat("x", MaxFileBytes))
	if _, err := Load(big, Options{}); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("Load of an oversized file = %v", err)
	}
}

func TestSaveLocalRoundTrip(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "gremloader.toml")
	write(t, main, mergeFile)

	l, err := LoadLocal(main)
	if err != nil || l.Package != nil {
		t.Fatalf("LoadLocal with no file = %+v, %v", l, err)
	}
	pin, ch, yes, days := "1.2.3", "prerelease", true, int64(2)
	l.Package = map[string]*LocalPackage{"b": {Hold: &yes}, "a": {Pin: &pin}}
	l.Settings = &LocalSettings{Channel: &ch, CooldownDays: &days}
	if err := SaveLocal(main, l); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(dir, "gremloader.local.toml"))
	if !strings.HasPrefix(string(first), "# Written by GremLoader") {
		t.Errorf("no header:\n%s", first)
	}
	if strings.Index(string(first), "[package.a]") > strings.Index(string(first), "[package.b]") {
		t.Errorf("packages not in name order:\n%s", first)
	}

	// What was written loads, and applies.
	c, err := Load(main, Options{})
	if err != nil {
		t.Fatalf("Load after SaveLocal:\n%v\nfile:\n%s", err, first)
	}
	if c.Packages["a"].Pin != "1.2.3" || !c.Packages["b"].Hold || c.Packages["c"].Channel != "prerelease" {
		t.Errorf("saved overrides not applied: %+v", c.Packages["a"])
	}

	// Writing the same thing again gives the same bytes.
	l2, err := LoadLocal(main)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveLocal(main, l2); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(dir, "gremloader.local.toml"))
	if string(first) != string(second) {
		t.Errorf("rewrite changed the file:\n%s\n---\n%s", first, second)
	}
}

func TestSaveLocalRefusesBadValues(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "gremloader.toml")
	bad, name := "nightly", "1.0"
	for _, l := range []*Local{
		{Package: map[string]*LocalPackage{"a": {Channel: &bad}}},
		{Package: map[string]*LocalPackage{"Bad Name": {Pin: &name}}},
		{Settings: &LocalSettings{Channel: &bad}},
	} {
		if err := SaveLocal(main, l); err == nil {
			t.Errorf("SaveLocal(%+v) succeeded", l)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "gremloader.local.toml")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a refused SaveLocal left a file behind")
	}
}

func TestLoadLocalRejectsForbiddenKeys(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "gremloader.toml")
	write(t, filepath.Join(dir, "gremloader.local.toml"), "schema_version = 1\n[package.a]\npost_install = [[\"x\"]]\n")
	_, err := LoadLocal(main)
	wantProblem(t, err, 3, "post_install", "not allowed")
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
