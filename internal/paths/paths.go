// Package paths decides where GremLoader keeps its files, per OS, and finds the package file.
//
// Layout (see the Phase 1 plan, E.2.7):
//
//	config  %APPDATA%\GremLoader                 or  $XDG_CONFIG_HOME/gremloader (~/.config/gremloader)
//	state   %LOCALAPPDATA%\GremLoader            or  $XDG_STATE_HOME/gremloader  (~/.local/state/gremloader)
//	cache   %LOCALAPPDATA%\GremLoader\cache      or  $XDG_CACHE_HOME/gremloader  (~/.cache/gremloader)
//
// GREMLOADER_HOME collapses all three under one folder (config/, state/, cache/): that is how
// tests, the acceptance run and portable use are kept away from the real directories.
//
// The environment is passed in (Env) so every OS's rules can be tested on any OS.
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Env is everything path resolution reads from the outside world.
type Env struct {
	GOOS   string
	Getenv func(string) string
	Home   string // the user's home directory
}

// System returns the real environment. A missing home directory is not an error here: a
// GREMLOADER_HOME override, or %APPDATA% and %LOCALAPPDATA% on Windows, make it unnecessary, and
// Resolve reports it when a layout really needs it.
func System() Env {
	home, _ := os.UserHomeDir()
	return Env{GOOS: runtime.GOOS, Getenv: os.Getenv, Home: home}
}

// Paths are the three base directories.
type Paths struct {
	Config string
	State  string
	Cache  string
}

// Resolve applies the layout rules. Relative XDG_* values are ignored, as the XDG spec says.
func Resolve(e Env) (Paths, error) {
	if h := e.Getenv("GREMLOADER_HOME"); h != "" {
		abs, err := filepath.Abs(h)
		if err != nil {
			return Paths{}, fmt.Errorf("GREMLOADER_HOME %q: %w", h, err)
		}
		return Paths{
			Config: filepath.Join(abs, "config"),
			State:  filepath.Join(abs, "state"),
			Cache:  filepath.Join(abs, "cache"),
		}, nil
	}

	if e.GOOS == "windows" {
		roaming := first(e.Getenv("APPDATA"), join(e.Home, "AppData", "Roaming"))
		local := first(e.Getenv("LOCALAPPDATA"), join(e.Home, "AppData", "Local"))
		if roaming == "" || local == "" {
			return Paths{}, errors.New("cannot find %APPDATA% or %LOCALAPPDATA%; set GREMLOADER_HOME")
		}
		return Paths{
			Config: filepath.Join(roaming, "GremLoader"),
			State:  filepath.Join(local, "GremLoader"),
			Cache:  filepath.Join(local, "GremLoader", "cache"),
		}, nil
	}

	xdg := func(name string, fallback ...string) string {
		if v := e.Getenv(name); v != "" && filepath.IsAbs(v) {
			return v
		}
		return join(e.Home, fallback...)
	}
	cfg, st, ca := xdg("XDG_CONFIG_HOME", ".config"), xdg("XDG_STATE_HOME", ".local", "state"), xdg("XDG_CACHE_HOME", ".cache")
	if cfg == "" || st == "" || ca == "" {
		return Paths{}, errors.New("cannot find the home directory; set GREMLOADER_HOME")
	}
	return Paths{
		Config: filepath.Join(cfg, "gremloader"),
		State:  filepath.Join(st, "gremloader"),
		Cache:  filepath.Join(ca, "gremloader"),
	}, nil
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func join(base string, elem ...string) string {
	if base == "" {
		return ""
	}
	return filepath.Join(append([]string{base}, elem...)...)
}

// Files and folders under State. Package names must pass ValidPackageName before they get here.

func (p Paths) StateFile() string         { return filepath.Join(p.State, "state.json") }
func (p Paths) EventsLog() string         { return filepath.Join(p.State, "events.jsonl") }
func (p Paths) RunLock() string           { return filepath.Join(p.State, "run.lock") }
func (p Paths) Staging(pkg string) string { return filepath.Join(p.State, "staging", pkg) }
func (p Paths) Journal(pkg string) string { return filepath.Join(p.State, "journal", pkg) }
func (p Paths) Previous(pkg string) string {
	return filepath.Join(p.State, "previous", pkg)
}
func (p Paths) Manifests(pkg string) string { return filepath.Join(p.State, "manifests", pkg) }

// LocalOverride is the machine-written override file that sits beside the package file.
func LocalOverride(packageFile string) string {
	ext := filepath.Ext(packageFile)
	return strings.TrimSuffix(packageFile, ext) + ".local" + ext
}

var packageName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidPackageName reports whether s may be used as a package name. Names become folder names
// and appear in URLs and hook arguments, so the rule is strict: lower-case letters, digits and
// ". _ -", at most 63 characters, starting with a letter or digit, never ".." inside, not ending
// in a dot, and not a Windows device name (con, nul, com1 ... with or without an extension). The
// last two matter only on Windows, but a package file has to mean the same everywhere.
func ValidPackageName(s string) bool {
	if !packageName.MatchString(s) || strings.Contains(s, "..") || strings.HasSuffix(s, ".") {
		return false
	}
	return !IsWindowsDeviceName(s)
}

// IsWindowsDeviceName reports whether one path component names a Windows device (con, NUL.txt,
// Com1.log ...), in any case. Linux has no such names, so filepath.IsLocal accepts them there;
// files that must mean the same on both systems check with this.
func IsWindowsDeviceName(component string) bool {
	stem, _, _ := strings.Cut(component, ".")
	return windowsDevice[strings.ToLower(strings.TrimRight(stem, " "))]
}

var windowsDevice = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com0": true, "com1": true, "com2": true, "com3": true, "com4": true,
	"com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt0": true, "lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true,
	"lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// FindPackageFile locates gremloader.toml. Order: explicit (--file), $GREMLOADER_FILE, then
// ./gremloader.toml, then <config>/gremloader.toml. An explicit path that does not exist is an
// error, not a reason to keep looking.
func FindPackageFile(explicit string, e Env, cwd string, p Paths) (string, error) {
	exists := func(path string) bool {
		st, err := os.Stat(path)
		return err == nil && st.Mode().IsRegular()
	}
	if explicit != "" {
		if !exists(explicit) {
			return "", fmt.Errorf("package file %q (from --file) does not exist", explicit)
		}
		return explicit, nil
	}
	if f := e.Getenv("GREMLOADER_FILE"); f != "" {
		if !exists(f) {
			return "", fmt.Errorf("package file %q (from GREMLOADER_FILE) does not exist", f)
		}
		return f, nil
	}
	candidates := []string{filepath.Join(cwd, "gremloader.toml"), filepath.Join(p.Config, "gremloader.toml")}
	for _, c := range candidates {
		if exists(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("no package file found; looked for:\n  %s\nuse --file or create one", strings.Join(candidates, "\n  "))
}
