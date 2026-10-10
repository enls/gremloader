package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// absRoot is a root that is absolute on the host OS (a bare "\" is not absolute on Windows).
func absRoot() string {
	if runtime.GOOS == "windows" {
		return `C:\`
	}
	return "/"
}

func env(goos, home string, vars map[string]string) Env {
	return Env{GOOS: goos, Home: home, Getenv: func(k string) string { return vars[k] }}
}

func TestWindowsLayout(t *testing.T) {
	home := filepath.Join("C:", "Users", "ian")
	p, err := Resolve(env("windows", home, map[string]string{
		"APPDATA": `C:\Users\ian\AppData\Roaming`, "LOCALAPPDATA": `C:\Users\ian\AppData\Local`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Config != filepath.Join(`C:\Users\ian\AppData\Roaming`, "GremLoader") {
		t.Errorf("Config = %q", p.Config)
	}
	if p.State != filepath.Join(`C:\Users\ian\AppData\Local`, "GremLoader") {
		t.Errorf("State = %q", p.State)
	}
	if p.Cache != filepath.Join(p.State, "cache") {
		t.Errorf("Cache = %q", p.Cache)
	}
}

func TestWindowsFallsBackToHome(t *testing.T) {
	p, err := Resolve(env("windows", "H", nil))
	if err != nil {
		t.Fatal(err)
	}
	if p.Config != filepath.Join("H", "AppData", "Roaming", "GremLoader") || p.State != filepath.Join("H", "AppData", "Local", "GremLoader") {
		t.Errorf("fallback paths = %+v", p)
	}
}

func TestLinuxXDG(t *testing.T) {
	abs := func(s string) string { return filepath.Join(absRoot(), s) }
	p, err := Resolve(env("linux", abs("home/ian"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if p.Config != filepath.Join(abs("home/ian"), ".config", "gremloader") ||
		p.State != filepath.Join(abs("home/ian"), ".local", "state", "gremloader") ||
		p.Cache != filepath.Join(abs("home/ian"), ".cache", "gremloader") {
		t.Errorf("defaults = %+v", p)
	}

	p, err = Resolve(env("linux", abs("home/ian"), map[string]string{
		"XDG_CONFIG_HOME": abs("x/cfg"), "XDG_STATE_HOME": abs("x/st"), "XDG_CACHE_HOME": abs("x/ca"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Config != filepath.Join(abs("x/cfg"), "gremloader") || p.State != filepath.Join(abs("x/st"), "gremloader") || p.Cache != filepath.Join(abs("x/ca"), "gremloader") {
		t.Errorf("XDG paths = %+v", p)
	}
}

func TestRelativeXDGIsIgnored(t *testing.T) {
	home := filepath.Join(absRoot(), "home", "ian")
	p, err := Resolve(env("linux", home, map[string]string{"XDG_CONFIG_HOME": "relative/dir"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.Config, filepath.Join(home, ".config")) {
		t.Errorf("relative XDG_CONFIG_HOME was honoured: %q", p.Config)
	}
}

func TestHomeOverrideWinsEverywhere(t *testing.T) {
	dir := t.TempDir()
	for _, goos := range []string{"windows", "linux"} {
		p, err := Resolve(env(goos, "", map[string]string{"GREMLOADER_HOME": dir, "XDG_CONFIG_HOME": "/ignored", "APPDATA": `C:\ignored`}))
		if err != nil {
			t.Fatal(err)
		}
		if p.Config != filepath.Join(dir, "config") || p.State != filepath.Join(dir, "state") || p.Cache != filepath.Join(dir, "cache") {
			t.Errorf("%s: %+v", goos, p)
		}
	}
}

func TestNoHomeIsAnError(t *testing.T) {
	if _, err := Resolve(env("linux", "", nil)); err == nil {
		t.Error("linux with no home and no override must fail")
	}
	if _, err := Resolve(env("windows", "", nil)); err == nil {
		t.Error("windows with no env and no override must fail")
	}
}

func TestStatePaths(t *testing.T) {
	p := Paths{State: filepath.Join("S")}
	want := map[string]string{
		p.StateFile():             filepath.Join("S", "state.json"),
		p.EventsLog():             filepath.Join("S", "events.jsonl"),
		p.RunLock():               filepath.Join("S", "run.lock"),
		p.Staging("photocraft"):   filepath.Join("S", "staging", "photocraft"),
		p.Journal("photocraft"):   filepath.Join("S", "journal", "photocraft"),
		p.Previous("photocraft"):  filepath.Join("S", "previous", "photocraft"),
		p.Manifests("photocraft"): filepath.Join("S", "manifests", "photocraft"),
	}
	for got, w := range want {
		if got != w {
			t.Errorf("got %q, want %q", got, w)
		}
	}
}

func TestLocalOverride(t *testing.T) {
	if got := LocalOverride(filepath.Join("a", "gremloader.toml")); got != filepath.Join("a", "gremloader.local.toml") {
		t.Errorf("LocalOverride = %q", got)
	}
	if got := LocalOverride("x.toml"); got != "x.local.toml" {
		t.Errorf("LocalOverride = %q", got)
	}
}

func TestValidPackageName(t *testing.T) {
	for _, s := range []string{"photocraft", "a", "docker-engine", "gangsheet.stack", "a_b", "0ad", strings.Repeat("a", 63)} {
		if !ValidPackageName(s) {
			t.Errorf("ValidPackageName(%q) = false", s)
		}
	}
	for _, s := range []string{"", "Photocraft", "-a", ".a", "a b", "a/b", `a\b`, "a..b", "../x", "a:b", "é", strings.Repeat("a", 64), "con\x00"} {
		if ValidPackageName(s) {
			t.Errorf("ValidPackageName(%q) = true", s)
		}
	}
}

func TestFindPackageFile(t *testing.T) {
	cwd, cfg := t.TempDir(), t.TempDir()
	p := Paths{Config: cfg}
	noEnv := env("linux", "/h", nil)
	write := func(dir string) string {
		f := filepath.Join(dir, "gremloader.toml")
		if err := os.WriteFile(f, []byte("schema_version = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}

	if _, err := FindPackageFile("", noEnv, cwd, p); err == nil || !strings.Contains(err.Error(), "no package file found") {
		t.Errorf("nothing exists: %v", err)
	}
	inCfg := write(cfg)
	if got, err := FindPackageFile("", noEnv, cwd, p); err != nil || got != inCfg {
		t.Errorf("config dir: %q, %v", got, err)
	}
	inCwd := write(cwd)
	if got, err := FindPackageFile("", noEnv, cwd, p); err != nil || got != inCwd {
		t.Errorf("cwd must beat config: %q, %v", got, err)
	}
	other := filepath.Join(t.TempDir(), "other.toml")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	withEnv := env("linux", "/h", map[string]string{"GREMLOADER_FILE": other})
	if got, err := FindPackageFile("", withEnv, cwd, p); err != nil || got != other {
		t.Errorf("env must beat cwd: %q, %v", got, err)
	}
	flag := write(t.TempDir())
	if got, err := FindPackageFile(flag, withEnv, cwd, p); err != nil || got != flag {
		t.Errorf("--file must beat env: %q, %v", got, err)
	}
	if _, err := FindPackageFile(filepath.Join(cwd, "missing.toml"), noEnv, cwd, p); err == nil {
		t.Error("explicit missing file must be an error, not a fall-through")
	}
	if _, err := FindPackageFile("", env("linux", "/h", map[string]string{"GREMLOADER_FILE": filepath.Join(cwd, "nope.toml")}), cwd, p); err == nil {
		t.Error("GREMLOADER_FILE pointing nowhere must be an error")
	}
}

// Package names become folder names under state\staging, journal and previous. On Windows a device
// name (with or without an extension) cannot be one, and a trailing dot is silently dropped, so
// "foo." would share a folder with "foo". A package file must mean the same on every OS, so the
// rule applies everywhere.
func TestPackageNamesThatWindowsCannotUseAreRejected(t *testing.T) {
	for _, s := range []string{
		"con", "prn", "aux", "nul", "com1", "com9", "lpt1", "lpt9", "com0", "lpt0",
		"con.txt", "nul.v1", "aux.tar.gz", "foo.", "foo...", "a.",
	} {
		if ValidPackageName(s) {
			t.Errorf("ValidPackageName(%q) = true; Windows cannot use it as a folder name", s)
		}
	}
	// Close relatives are fine.
	for _, s := range []string{"console", "com", "com10", "lpt", "nullable", "my-con", "a.con", "aux-tools", "x.nul"} {
		if !ValidPackageName(s) {
			t.Errorf("ValidPackageName(%q) = false", s)
		}
	}
}

// A scheduled task or service can run with no user profile variable at all. On Windows nothing
// else needs it when %APPDATA% and %LOCALAPPDATA% are set, so System must not fail on its own;
// Resolve is the place that says what is missing.
func TestSystemDoesNotFailOnAMissingHomeThatResolveDoesNotNeed(t *testing.T) {
	for _, k := range []string{"HOME", "USERPROFILE", "GREMLOADER_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(k, "")
	}
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	_, err := Resolve(System())
	if runtime.GOOS == "windows" && err != nil {
		t.Errorf("Resolve: %v", err)
	}
	if runtime.GOOS != "windows" && (err == nil || !strings.Contains(err.Error(), "GREMLOADER_HOME")) {
		t.Errorf("Resolve without a home: %v, want an error naming GREMLOADER_HOME", err)
	}
}
