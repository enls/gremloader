package pkgfile

import (
	"cmp"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/enls/gremloader/internal/archive"
	"github.com/enls/gremloader/internal/platform"
)

// Package is one package's effective settings.
type Package struct {
	Name            string
	Source          string // "github-release"
	Repo            string // "owner/name"
	TagPrefix       string
	Channel         string // "stable" | "prerelease"
	API             string // "auto" | "api" | "web"
	Checksums       Template
	VerifyRequire   []string // subset of checksum, digest, attestation
	Attestation     string   // "if-present" | "off"
	Strip           archive.Strip
	Pin             string // a version, normalised; "" = none
	Hold            bool
	CooldownDays    int
	CooldownUnknown string // "deny" | "allow"
	Limits          Limits
	Enabled         bool
	HookFail        string // "warn" | "fail"
	HookTimeout     time.Duration

	platforms []platform.Platform
	keyed     map[string]*keyed // asset, dir, version_from_exe, hook events
	vars      map[string]string
	origins   map[string]Origin
}

// Limits are the per-package caps on what is downloaded and unpacked.
type Limits struct {
	MaxDownload  int64
	MaxExtracted int64
	MaxFiles     int
}

// ArchiveLimits returns the extractor limits with this package's caps applied.
func (l Limits) ArchiveLimits() archive.Limits {
	a := archive.DefaultLimits()
	a.MaxTotalBytes = l.MaxExtracted
	a.MaxFileBytes = min(a.MaxFileBytes, l.MaxExtracted)
	a.MaxEntries = l.MaxFiles
	return a
}

// check reports what a package must have to be installable anywhere.
func (p *Package) check(s *src, key []string) {
	at := func(k ...string) []string { return append(append([]string{}, key...), k...) }
	if p.Repo == "" {
		s.problem(at("repo"), "missing (owner/name of the GitHub repository)")
	}
	asset := p.keyed[kAsset]
	if asset == nil || (asset.all == nil && len(asset.byKey) == 0) {
		s.problem(at("asset"), "missing (the release file to install, or an os/arch table of them)")
	}
	if d := p.keyed[kDir]; d == nil || (d.all == nil && len(d.byKey) == 0) {
		s.problem(at("dir"), "missing (where to install, or an OS table of folders)")
	}
	if slices.Contains(p.VerifyRequire, "checksum") && p.Checksums.IsZero() {
		s.problem(at("checksums"), `missing, but verify.require includes "checksum" (name the release's checksum file, or set verify.require = [] to install unverified)`)
	}
	for _, pl := range p.platforms {
		if _, ok := asset.get(pl.String()); !ok {
			s.problem(at("platforms"), "%s is listed but asset has no entry for it", pl)
		}
	}
	if len(p.VerifyRequire) == 0 {
		s.b.warn(fmt.Sprintf("%s: package %s is installed without any verification (verify.require = [])", s.name, p.Name))
	}
}

func (b *builder) warn(msg string) { b.warnings = append(b.warnings, msg) }

// Values are what is known about a release when a template is expanded.
type Values struct {
	Version  string // "0.6.0"
	Tag      string // "v0.6.0"
	Platform platform.Platform
}

func (p *Package) vals(v Values) map[string]string {
	m := maps.Clone(p.vars)
	if m == nil {
		m = map[string]string{}
	}
	m[varName] = p.Name
	m[varNameCap] = capitalise(p.Name)
	m[varOS] = v.Platform.OS
	m[varArch] = v.Platform.Arch
	if v.Version != "" {
		m[varVersion] = v.Version
	}
	if v.Tag != "" {
		m[varTag] = v.Tag
	}
	return m
}

// AssetPattern is the asset name (or wildcard pattern) to install on v.Platform.
func (p *Package) AssetPattern(v Values) (string, error) {
	t, ok := p.template(kAsset, v.Platform.String())
	if !ok {
		return "", fmt.Errorf("package %s has no asset for %s (it has: %s)", p.Name, v.Platform, p.keysOf(kAsset))
	}
	s, err := t.expand(p.vals(v), fileNameValue)
	if err != nil {
		return "", fmt.Errorf("package %s: asset %w", p.Name, err)
	}
	if !validFileName(s) {
		return "", fmt.Errorf("package %s: asset %q is not a file name", p.Name, s)
	}
	return s, nil
}

// ChecksumsName is the checksum file to download, given the selected asset. ok is false when the
// package names none.
func (p *Package) ChecksumsName(v Values, asset string) (name string, ok bool, err error) {
	if p.Checksums.IsZero() {
		return "", false, nil
	}
	vals := p.vals(v)
	vals[varAsset] = asset
	s, err := p.Checksums.expand(vals, fileNameValue)
	if err != nil {
		return "", true, fmt.Errorf("package %s: checksums %w", p.Name, err)
	}
	if !validFileName(s) || strings.ContainsAny(s, "*?[") {
		return "", true, fmt.Errorf("package %s: checksums %q is not a file name", p.Name, s)
	}
	return s, true, nil
}

// Dir is the absolute install directory on goos. A leading "~/" (or "~\") is the user's home.
func (p *Package) Dir(goos, home string) (string, error) {
	t, ok := p.template(kDir, goos)
	if !ok {
		return "", fmt.Errorf("package %s has no dir for %s (it has: %s)", p.Name, goos, p.keysOf(kDir))
	}
	s, err := t.expand(p.vals(Values{Platform: platform.Platform{OS: goos}}), controlValue)
	if err != nil {
		return "", fmt.Errorf("package %s: dir %w", p.Name, err)
	}
	if rest, ok := strings.CutPrefix(s, "~"); ok && (rest == "" || rest[0] == '/' || rest[0] == '\\') {
		if home == "" {
			return "", fmt.Errorf("package %s: dir %q needs the home directory, which is unknown", p.Name, s)
		}
		s = home + rest
	}
	if !filepath.IsAbs(s) {
		return "", fmt.Errorf("package %s: dir %q is not an absolute path", p.Name, s)
	}
	return filepath.Clean(s), nil
}

// SharedDir reports whether the install directory on goos is shared with other packages and the
// user (shared_dir, e.g. ~/.local): it need not be empty for a first install, its top-level
// folders are never GremLoader's, and it is never deleted whole.
func (p *Package) SharedDir(goos string) bool {
	v, ok := p.keyed[kSharedDir].get(goos)
	return ok && v.v.(bool)
}

// VersionFromExe is the file, relative to the install directory, whose embedded version
// describes an existing install (Windows adoption). ok is false when none is set for goos.
func (p *Package) VersionFromExe(goos string) (rel string, ok bool, err error) {
	t, ok := p.template(kExe, goos)
	if !ok {
		return "", false, nil
	}
	s, err := t.expand(p.vals(Values{Platform: platform.Platform{OS: goos}}), controlValue)
	if err != nil {
		return "", true, fmt.Errorf("package %s: version_from_exe %w", p.Name, err)
	}
	if !filepath.IsLocal(filepath.FromSlash(s)) {
		return "", true, fmt.Errorf("package %s: version_from_exe %q must be a path inside the install directory", p.Name, s)
	}
	return filepath.FromSlash(s), true, nil
}

// Hooks returns the commands (argv lists) to run for event on v.Platform.OS.
func (p *Package) Hooks(event string, v Values) ([][]string, error) {
	val, ok := p.keyed[event].get(v.Platform.OS)
	if !ok {
		return nil, nil
	}
	vals := p.vals(v)
	var out [][]string
	for _, cmd := range val.v.([][]Template) {
		argv := make([]string, len(cmd))
		for i, t := range cmd {
			s, err := t.expand(vals, controlValue)
			if err != nil {
				return nil, fmt.Errorf("package %s: %s %w", p.Name, event, err)
			}
			argv[i] = s
		}
		out = append(out, argv)
	}
	return out, nil
}

// Platforms is the list `gremloader lock` resolves: the `platforms` setting, or else every
// platform the asset table names. A single asset string with no `platforms` gives nil (the
// caller decides; locking only the current machine would be a guess).
func (p *Package) Platforms() []platform.Platform {
	if p.platforms != nil {
		return slices.Clone(p.platforms)
	}
	var out []platform.Platform
	if a := p.keyed[kAsset]; a != nil && a.all == nil {
		for _, k := range slices.Sorted(maps.Keys(a.byKey)) {
			if pl, err := platform.Parse(k); err == nil {
				out = append(out, pl)
			}
		}
	}
	return out
}

func (p *Package) template(name, key string) (Template, bool) {
	v, ok := p.keyed[name].get(key)
	if !ok {
		return Template{}, false
	}
	return v.v.(Template), true
}

func (p *Package) keysOf(name string) string {
	k := p.keyed[name]
	if k == nil {
		return "none"
	}
	var keys []string
	if k.all != nil {
		keys = append(keys, "every platform")
	}
	keys = append(keys, slices.Sorted(maps.Keys(k.byKey))...)
	return orNone(keys)
}

// Setting is one line of `gremloader explain`.
type Setting struct {
	Key    string
	Value  string
	Origin Origin
}

// Explain lists every effective setting with where it came from, in a stable order.
func (p *Package) Explain() []Setting {
	var out []Setting
	add := func(k, v string) { out = append(out, Setting{Key: k, Value: v, Origin: p.origins[k]}) }
	q := strconv.Quote
	add(kSource, q(p.Source))
	add(kRepo, q(p.Repo))
	add(kTagPrefix, q(p.TagPrefix))
	add(kChannel, q(p.Channel))
	add(kAPI, q(p.API))
	p.explainKeyed(&out, kAsset)
	add(kChecksums, q(p.Checksums.String()))
	add(kVerifyRequire, list(p.VerifyRequire))
	add(kVerifyAttest, q(p.Attestation))
	p.explainKeyed(&out, kDir)
	if p.keyed[kSharedDir] == nil {
		add(kSharedDir, "false")
	} else {
		p.explainKeyed(&out, kSharedDir)
	}
	if p.Strip.Auto {
		add(kStrip, q("auto"))
	} else {
		add(kStrip, strconv.Itoa(p.Strip.N))
	}
	p.explainKeyed(&out, kExe)
	var pls []string
	for _, pl := range p.Platforms() {
		pls = append(pls, pl.String())
	}
	add(kPlatforms, list(pls))
	add(kPin, q(p.Pin))
	add(kHold, strconv.FormatBool(p.Hold))
	add(kCooldownDays, strconv.Itoa(p.CooldownDays))
	add(kCooldownUnknown, q(p.CooldownUnknown))
	add(kMaxDownload, strconv.FormatInt(p.Limits.MaxDownload, 10))
	add(kMaxExtracted, strconv.FormatInt(p.Limits.MaxExtracted, 10))
	add(kMaxFiles, strconv.Itoa(p.Limits.MaxFiles))
	add(kEnabled, strconv.FormatBool(p.Enabled))
	add(kHookFail, q(p.HookFail))
	add(kHookTimeout, p.HookTimeout.String())
	for _, ev := range HookEvents {
		p.explainKeyed(&out, ev)
	}
	return out
}

func (p *Package) explainKeyed(out *[]Setting, name string) {
	k := p.keyed[name]
	if k == nil {
		return
	}
	show := func(v value) string {
		switch x := v.v.(type) {
		case Template:
			return strconv.Quote(x.String())
		case [][]Template:
			var cmds []string
			for _, c := range x {
				var args []string
				for _, a := range c {
					args = append(args, a.String())
				}
				cmds = append(cmds, list(args))
			}
			return "[" + strings.Join(cmds, ", ") + "]"
		}
		return fmt.Sprint(v.v)
	}
	if k.all != nil {
		*out = append(*out, Setting{Key: name, Value: show(*k.all), Origin: k.all.origin})
	}
	keys := slices.SortedFunc(maps.Keys(k.byKey), cmp.Compare[string])
	for _, key := range keys {
		v := k.byKey[key]
		*out = append(*out, Setting{Key: name + "." + strconv.Quote(key), Value: show(v), Origin: v.origin})
	}
}

func list(s []string) string {
	q := make([]string, len(s))
	for i, x := range s {
		q[i] = strconv.Quote(x)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func validFileName(s string) bool {
	if s == "" || len(s) > 255 || s == "." || s == ".." || strings.ContainsAny(s, `/\:`) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
