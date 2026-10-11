// Package pkgfile reads gremloader.toml (what the user wants installed, hand-written) and
// gremloader.local.toml (this machine's pins and holds, machine-written), checks them, and
// produces one effective setting per package together with where each value came from.
//
// The formats are the plan's E.2.1 and E.2.2; ADR 0011 records the rules. In short:
//
//   - Unknown keys and wrong types are errors with line and column (go-toml's strict mode). An
//     older GremLoader reading a newer file therefore refuses, which is the safe direction.
//   - A file whose schema_version is newer than this build is refused before anything else.
//   - Layers, least to most specific: [settings], local [settings], the package's preset
//     (`use`), [package.<name>], local [package.<name>]. The most specific layer that sets a key
//     wins. Tables merge per key (one platform's asset can be overridden); a plain value covers
//     every platform; lists replace.
//   - A preset cannot use another preset. `use`, `repo` and `pin` belong to a package.
//   - The local file may only set pin, hold, channel, cooldown_days and enabled per package and
//     channel, cooldown_days in [settings]: never a command, URL or path, so a local override can
//     never change what runs.
//   - Templates are checked when the file is loaded (see Template).
package pkgfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/enls/gremloader/internal/archive"
	"github.com/enls/gremloader/internal/paths"
	"github.com/enls/gremloader/internal/platform"
	"github.com/enls/gremloader/internal/source/github"
	"github.com/enls/gremloader/internal/ver"
)

// SchemaVersion is the package file format this build understands.
const SchemaVersion = 1

// MaxFileBytes caps a package file. A real one is a few kilobytes.
const MaxFileBytes = 1 << 20

// ErrNewerSchema: the file was written for a newer GremLoader.
var ErrNewerSchema = errors.New("written for a newer version of GremLoader")

// Options are inputs from outside the files.
type Options struct {
	// Vars are command-line variables (--root sets "root"). They override [vars] and may
	// define new ones.
	Vars map[string]string
}

// Config is the loaded configuration.
type Config struct {
	File      string // the package file
	LocalFile string // the local override file, "" when there is none
	Vars      map[string]string
	Packages  map[string]*Package
	Warnings  []string // problems that do not stop a run
}

// Names returns the package names in order.
func (c *Config) Names() []string { return slices.Sorted(maps.Keys(c.Packages)) }

// Load reads path and, if it exists, the local override file beside it.
func Load(path string, opts Options) (*Config, error) {
	data, err := readCapped(path)
	if err != nil {
		return nil, err
	}
	localPath := paths.LocalOverride(path)
	local, err := readCapped(localPath)
	if errors.Is(err, fs.ErrNotExist) {
		localPath, local, err = "", nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parse(path, data, localPath, local, opts)
}

func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the package file the user named
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("%s: larger than %d KiB; this is not a package file", path, MaxFileBytes>>10)
	}
	return data, nil
}

// ---- raw shapes (what go-toml decodes into; strict mode rejects everything else) ----

type rawFile struct {
	SchemaVersion int64                `toml:"schema_version"`
	Vars          map[string]string    `toml:"vars"`
	Settings      *rawLayer            `toml:"settings"`
	Preset        map[string]*rawLayer `toml:"preset"`
	Package       map[string]*rawLayer `toml:"package"`
}

type rawLayer struct {
	Use             *string    `toml:"use"`
	Source          *string    `toml:"source"`
	Repo            *string    `toml:"repo"`
	TagPrefix       *string    `toml:"tag_prefix"`
	Channel         *string    `toml:"channel"`
	API             *string    `toml:"api"`
	Asset           any        `toml:"asset"`
	Checksums       *string    `toml:"checksums"`
	Verify          *rawVerify `toml:"verify"`
	Dir             any        `toml:"dir"`
	SharedDir       any        `toml:"shared_dir"`
	Strip           any        `toml:"strip"`
	VersionFromExe  any        `toml:"version_from_exe"`
	Platforms       *[]string  `toml:"platforms"`
	Pin             *string    `toml:"pin"`
	Hold            *bool      `toml:"hold"`
	CooldownDays    *int64     `toml:"cooldown_days"`
	CooldownUnknown *string    `toml:"cooldown_unknown"`
	Limits          *rawLimits `toml:"limits"`
	Enabled         *bool      `toml:"enabled"`
	HookFail        *string    `toml:"hook_fail"`
	HookTimeout     *string    `toml:"hook_timeout"`
	PostInstall     any        `toml:"post_install"`
	PostUpdate      any        `toml:"post_update"`
	PreRemove       any        `toml:"pre_remove"`
	PostRemove      any        `toml:"post_remove"`
}

type rawVerify struct {
	Require     *[]string `toml:"require"`
	Attestation *string   `toml:"attestation"`
}

type rawLimits struct {
	MaxDownload  any    `toml:"max_download"`
	MaxExtracted any    `toml:"max_extracted"`
	MaxFiles     *int64 `toml:"max_files"`
}

// Hook events.
const (
	PostInstall = "post_install"
	PostUpdate  = "post_update"
	PreRemove   = "pre_remove"
	PostRemove  = "post_remove"
)

// HookEvents lists the hook events in the order they are explained.
var HookEvents = []string{PostInstall, PostUpdate, PreRemove, PostRemove}

// ---- problems ----

// Problem is one thing wrong with a file. Line and Col are 0 when unknown.
type Problem struct {
	File string
	Line int
	Col  int
	Key  string // dotted key path, "" for a syntax error
	Msg  string
}

func (p Problem) String() string {
	var b strings.Builder
	b.WriteString(p.File)
	if p.Line > 0 {
		fmt.Fprintf(&b, ":%d", p.Line)
		if p.Col > 0 {
			fmt.Fprintf(&b, ":%d", p.Col)
		}
	}
	b.WriteString(": ")
	if p.Key != "" {
		b.WriteString(p.Key + ": ")
	}
	b.WriteString(p.Msg)
	return b.String()
}

// Error lists every problem found, in file and line order, so one run shows them all.
type Error struct {
	Problems []Problem
}

func (e *Error) Error() string {
	lines := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		lines[i] = p.String()
	}
	return strings.Join(lines, "\n")
}

// ---- decoding ----

func decode(name string, data []byte, v any, current int64, describe func(key string) string) []Problem {
	var probe struct {
		SchemaVersion *int64 `toml:"schema_version"`
	}
	if err := toml.Unmarshal(data, &probe); err != nil {
		return tomlProblems(name, err, describe)
	}
	switch {
	case probe.SchemaVersion == nil:
		return []Problem{{File: name, Line: 1, Msg: fmt.Sprintf("schema_version is missing (put schema_version = %d at the top)", current)}}
	case *probe.SchemaVersion > current:
		return []Problem{{File: name, Key: "schema_version",
			Msg: fmt.Sprintf("%v (schema_version %d; this one understands %d): update GremLoader", ErrNewerSchema, *probe.SchemaVersion, current)}}
	case *probe.SchemaVersion < 1:
		return []Problem{{File: name, Key: "schema_version", Msg: "must be 1 or more"}}
	}
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return tomlProblems(name, err, describe)
	}
	return nil
}

func tomlProblems(name string, err error, describe func(key string) string) []Problem {
	var sme *toml.StrictMissingError
	if errors.As(err, &sme) {
		var out []Problem
		for _, de := range sme.Errors {
			row, col := de.Position()
			key := dotted(de.Key())
			out = append(out, Problem{File: name, Line: row, Col: col, Key: key, Msg: describe(key)})
		}
		return out
	}
	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, col := de.Position()
		return []Problem{{File: name, Line: row, Col: col, Msg: strings.TrimPrefix(de.Error(), "toml: ")}}
	}
	return []Problem{{File: name, Msg: strings.TrimPrefix(err.Error(), "toml: ")}}
}

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// dotted writes a key path the way it would appear in TOML.
func dotted(k []string) string {
	parts := make([]string, len(k))
	for i, s := range k {
		if bareKey.MatchString(s) {
			parts[i] = s
		} else {
			parts[i] = strconv.Quote(s)
		}
	}
	return strings.Join(parts, ".")
}

// ---- building ----

// Origin says where an effective value came from.
type Origin struct {
	File  string // "" for a built-in default
	Line  int
	Layer string // "[settings]", "[preset.craft]", "[package.photocraft]", "command line"
}

func (o Origin) String() string {
	switch {
	case o.File == "" && o.Layer == "":
		return "default"
	case o.File == "":
		return o.Layer
	case o.Line > 0:
		return fmt.Sprintf("%s:%d %s", o.File, o.Line, o.Layer)
	default:
		return o.File + " " + o.Layer
	}
}

type value struct {
	v      any
	origin Origin
}

// keyed is a setting that may differ per platform (asset: "os/arch" keys) or per OS (dir,
// version_from_exe, hooks: "os" keys).
type keyed struct {
	all   *value
	byKey map[string]value
}

func (k *keyed) get(key string) (value, bool) {
	if k == nil {
		return value{}, false
	}
	if v, ok := k.byKey[key]; ok {
		return v, true
	}
	if k.all != nil {
		return *k.all, true
	}
	return value{}, false
}

type layer struct {
	scalars map[string]value
	keyed   map[string]*keyed
}

func newLayer() *layer { return &layer{scalars: map[string]value{}, keyed: map[string]*keyed{}} }

type builder struct {
	problems []Problem
	warnings []string
	userVars map[string]bool
}

// src is one file being turned into layers.
type src struct {
	b    *builder
	name string
	pos  positions
}

func (s *src) problem(key []string, format string, a ...any) {
	s.b.problems = append(s.b.problems, Problem{File: s.name, Line: s.pos.line(key...), Key: dotted(key), Msg: fmt.Sprintf(format, a...)})
}

func (s *src) origin(key []string, where string) Origin {
	return Origin{File: s.name, Line: s.pos.line(key...), Layer: where}
}

func parse(name string, data []byte, localName string, localData []byte, opts Options) (*Config, error) {
	b := &builder{}
	var raw rawFile
	if ps := decode(name, data, &raw, SchemaVersion, unknownKey); ps != nil {
		return nil, &Error{Problems: ps}
	}
	main := &src{b: b, name: name, pos: indexPositions(data)}

	cfg := &Config{File: name, Vars: map[string]string{}, Packages: map[string]*Package{}}
	b.userVars = map[string]bool{}
	for _, k := range slices.Sorted(maps.Keys(raw.Vars)) {
		if err := checkVarName(k); err != nil {
			main.problem([]string{"vars", k}, "%v", err)
			continue
		}
		cfg.Vars[k] = raw.Vars[k]
		b.userVars[k] = true
	}
	for _, k := range slices.Sorted(maps.Keys(opts.Vars)) {
		if err := checkVarName(k); err != nil {
			b.problems = append(b.problems, Problem{File: "command line", Key: k, Msg: err.Error()})
			continue
		}
		cfg.Vars[k] = opts.Vars[k]
		b.userVars[k] = true
	}

	settings := main.layer(raw.Settings, []string{"settings"}, "[settings]", kindSettings)
	presets := map[string]*layer{}
	for _, n := range slices.Sorted(maps.Keys(raw.Preset)) {
		key := []string{"preset", n}
		if !paths.ValidPackageName(n) {
			main.problem(key, "preset name must look like a package name (lower-case letters, digits, . _ -)")
			continue
		}
		presets[n] = main.layer(raw.Preset[n], key, "[preset."+n+"]", kindPreset)
	}

	var localRaw Local
	localSettings, localPkgs := newLayer(), map[string]*layer{}
	if localData != nil {
		cfg.LocalFile = localName
		if ps := decode(localName, localData, &localRaw, SchemaVersion, notAllowedLocally); ps != nil {
			b.problems = append(b.problems, ps...)
		} else {
			ls := &src{b: b, name: localName, pos: indexPositions(localData)}
			localSettings = ls.layer(localRaw.Settings.raw(), []string{"settings"}, "[settings]", kindSettings)
			for _, n := range slices.Sorted(maps.Keys(localRaw.Package)) {
				key := []string{"package", n}
				if _, ok := raw.Package[n]; !ok {
					b.warn(fmt.Sprintf("%s:%d: [package.%s] is not in %s; ignored", localName, ls.pos.line(key...), n, name))
					continue
				}
				localPkgs[n] = ls.layer(localRaw.Package[n].raw(), key, "[package."+n+"]", kindPackage)
			}
		}
	}

	for _, n := range slices.Sorted(maps.Keys(raw.Package)) {
		key := []string{"package", n}
		if !paths.ValidPackageName(n) {
			main.problem(key, "package name %q is not valid: lower-case letters, digits and . _ -, at most 63 characters, not a Windows device name", n)
			continue
		}
		rp := raw.Package[n]
		layers := []*layer{settings, localSettings}
		if rp != nil && rp.Use != nil {
			p, ok := presets[*rp.Use]
			if !ok {
				main.problem(append(key, "use"), "no preset named %q (defined: %s)", *rp.Use, orNone(slices.Sorted(maps.Keys(raw.Preset))))
				continue
			}
			layers = append(layers, p)
		}
		layers = append(layers, main.layer(rp, key, "[package."+n+"]", kindPackage))
		if l, ok := localPkgs[n]; ok {
			layers = append(layers, l)
		}
		pkg := b.effective(n, layers, cfg.Vars)
		if pkg != nil {
			pkg.check(main, key)
			cfg.Packages[n] = pkg
		}
	}

	cfg.Warnings = b.warnings
	if len(b.problems) > 0 {
		slices.SortStableFunc(b.problems, func(x, y Problem) int {
			if x.File != y.File {
				// The package file first, then the local file.
				if x.File == name {
					return -1
				}
				if y.File == name {
					return 1
				}
			}
			return x.Line - y.Line
		})
		return nil, &Error{Problems: b.problems}
	}
	return cfg, nil
}

func unknownKey(string) string { return "unknown key" }

func notAllowedLocally(key string) string {
	return "not allowed in the local override file (it may set only pin, hold, channel, cooldown_days, enabled per package, and channel, cooldown_days under [settings])"
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

var varNameRule = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func checkVarName(k string) error {
	if !varNameRule.MatchString(k) {
		return fmt.Errorf("variable name %q: use letters, digits and _", k)
	}
	if slices.Contains(builtins, k) {
		return fmt.Errorf("{%s} is built in and cannot be redefined", k)
	}
	return nil
}

type layerKind int

const (
	kindSettings layerKind = iota
	kindPreset
	kindPackage
)

// Setting names, as they appear in explain output. Nested tables are flattened with dots.
const (
	kSource          = "source"
	kRepo            = "repo"
	kTagPrefix       = "tag_prefix"
	kChannel         = "channel"
	kAPI             = "api"
	kAsset           = "asset"
	kChecksums       = "checksums"
	kVerifyRequire   = "verify.require"
	kVerifyAttest    = "verify.attestation"
	kDir             = "dir"
	kSharedDir       = "shared_dir"
	kStrip           = "strip"
	kExe             = "version_from_exe"
	kPlatforms       = "platforms"
	kPin             = "pin"
	kHold            = "hold"
	kCooldownDays    = "cooldown_days"
	kCooldownUnknown = "cooldown_unknown"
	kMaxDownload     = "limits.max_download"
	kMaxExtracted    = "limits.max_extracted"
	kMaxFiles        = "limits.max_files"
	kEnabled         = "enabled"
	kHookFail        = "hook_fail"
	kHookTimeout     = "hook_timeout"
)

var tagPrefixRule = regexp.MustCompile(`^[A-Za-z0-9._+-]{0,64}$`)

// layer validates one table and turns it into typed values.
func (s *src) layer(r *rawLayer, key []string, where string, kind layerKind) *layer {
	l := newLayer()
	if r == nil {
		return l
	}
	at := func(k ...string) []string { return append(append([]string{}, key...), k...) }
	set := func(name string, v any, k ...string) {
		l.scalars[name] = value{v: v, origin: s.origin(at(k...), where)}
	}
	oneOf := func(p *string, name string, allowed ...string) {
		if p == nil {
			return
		}
		if !slices.Contains(allowed, *p) {
			s.problem(at(strings.Split(name, ".")...), "%q is not one of: %s", *p, strings.Join(allowed, ", "))
			return
		}
		set(name, *p, strings.Split(name, ".")...)
	}

	if kind != kindPackage {
		for k, isSet := range map[string]bool{"use": r.Use != nil, "repo": r.Repo != nil, "pin": r.Pin != nil} {
			if isSet {
				s.problem(at(k), "belongs to a [package.<name>] table, not %s", where)
			}
		}
	}

	oneOf(r.Source, kSource, "github-release")
	if r.Repo != nil && kind == kindPackage {
		if !github.ValidRepo(*r.Repo) {
			s.problem(at("repo"), "%q is not owner/name", *r.Repo)
		} else {
			set(kRepo, *r.Repo, "repo")
		}
	}
	if r.TagPrefix != nil {
		if !tagPrefixRule.MatchString(*r.TagPrefix) {
			s.problem(at("tag_prefix"), "%q may contain only letters, digits and . _ + -", *r.TagPrefix)
		} else {
			set(kTagPrefix, *r.TagPrefix, "tag_prefix")
		}
	}
	oneOf(r.Channel, kChannel, "stable", "prerelease")
	oneOf(r.API, kAPI, string(github.ModeAuto), string(github.ModeAPI), string(github.ModeWeb))

	if r.Asset != nil {
		l.keyed[kAsset] = s.keyedTemplates(r.Asset, at("asset"), where, forAsset, keyPlatform)
	}
	if r.Checksums != nil {
		if *r.Checksums == "" {
			set(kChecksums, Template{}, "checksums") // explicitly none
		} else if t, err := parseTemplate(*r.Checksums, forChecksums, s.b.userVars); err != nil {
			s.problem(at("checksums"), "%v", err)
		} else {
			set(kChecksums, t, "checksums")
		}
	}
	if v := r.Verify; v != nil {
		if v.Require != nil {
			req := *v.Require
			ok := true
			for _, lvl := range req {
				if !slices.Contains([]string{"checksum", "digest", "attestation"}, lvl) {
					s.problem(at("verify", "require"), "%q is not a verification level (checksum, digest, attestation)", lvl)
					ok = false
				}
			}
			if ok {
				set(kVerifyRequire, dedupe(req), "verify", "require")
			}
		}
		oneOf(v.Attestation, kVerifyAttest, "if-present", "off")
	}
	if r.Dir != nil {
		l.keyed[kDir] = s.keyedTemplates(r.Dir, at("dir"), where, forDir, keyOS)
	}
	if r.SharedDir != nil {
		l.keyed[kSharedDir] = s.keyedBools(r.SharedDir, at("shared_dir"), where)
	}
	if r.Strip != nil {
		if st, err := parseStrip(r.Strip); err != nil {
			s.problem(at("strip"), "%v", err)
		} else {
			set(kStrip, st, "strip")
		}
	}
	if r.VersionFromExe != nil {
		l.keyed[kExe] = s.keyedTemplates(r.VersionFromExe, at("version_from_exe"), where, forExe, keyOS)
	}
	if r.Platforms != nil {
		var ps []platform.Platform
		for _, p := range *r.Platforms {
			pl, err := platform.Parse(p)
			if err != nil {
				s.problem(at("platforms"), "%v", err)
				continue
			}
			if !slices.Contains(ps, pl) {
				ps = append(ps, pl)
			}
		}
		set(kPlatforms, ps, "platforms")
	}
	if r.Pin != nil && kind == kindPackage {
		v, err := ver.ParseLoose(*r.Pin)
		if err != nil {
			s.problem(at("pin"), "%v", err)
		} else {
			set(kPin, v.String(), "pin")
		}
	}
	if r.Hold != nil {
		set(kHold, *r.Hold, "hold")
	}
	if r.CooldownDays != nil {
		if *r.CooldownDays < 0 || *r.CooldownDays > 3650 {
			s.problem(at("cooldown_days"), "must be between 0 and 3650")
		} else {
			set(kCooldownDays, int(*r.CooldownDays), "cooldown_days")
		}
	}
	oneOf(r.CooldownUnknown, kCooldownUnknown, "deny", "allow")
	if lm := r.Limits; lm != nil {
		for _, x := range []struct {
			name, key string
			v         any
		}{{kMaxDownload, "max_download", lm.MaxDownload}, {kMaxExtracted, "max_extracted", lm.MaxExtracted}} {
			if x.v == nil {
				continue
			}
			n, err := parseSize(x.v)
			if err != nil {
				s.problem(at("limits", x.key), "%v", err)
				continue
			}
			set(x.name, n, "limits", x.key)
		}
		if lm.MaxFiles != nil {
			if *lm.MaxFiles < 1 || *lm.MaxFiles > 1_000_000 {
				s.problem(at("limits", "max_files"), "must be between 1 and 1000000")
			} else {
				set(kMaxFiles, int(*lm.MaxFiles), "limits", "max_files")
			}
		}
	}
	if r.Enabled != nil {
		set(kEnabled, *r.Enabled, "enabled")
	}
	oneOf(r.HookFail, kHookFail, "warn", "fail")
	if r.HookTimeout != nil {
		d, err := time.ParseDuration(*r.HookTimeout)
		if err != nil || d < time.Second || d > time.Hour {
			s.problem(at("hook_timeout"), "%q: write a duration between 1s and 1h, for example \"90s\" or \"5m\"", *r.HookTimeout)
		} else {
			set(kHookTimeout, d, "hook_timeout")
		}
	}
	for ev, h := range map[string]any{PostInstall: r.PostInstall, PostUpdate: r.PostUpdate, PreRemove: r.PreRemove, PostRemove: r.PostRemove} {
		if h != nil {
			l.keyed[ev] = s.keyedHooks(h, at(ev), where)
		}
	}
	return l
}

func dedupe(s []string) []string {
	out := []string{}
	for _, x := range s {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

type keyKind int

const (
	keyPlatform keyKind = iota // "os/arch"
	keyOS                      // "os"
)

func (s *src) checkKey(k string, kind keyKind, key []string) bool {
	if kind == keyPlatform {
		if _, err := platform.Parse(k); err != nil {
			s.problem(key, "%v", err)
			return false
		}
		return true
	}
	if !platform.ValidOS(k) {
		s.problem(key, "%q is not an operating system (%s)", k, strings.Join(platform.OSes, ", "))
		return false
	}
	return true
}

// keyedTemplates reads a string, or a table of strings keyed by platform or OS.
func (s *src) keyedTemplates(v any, key []string, where string, allowed map[string]bool, kind keyKind) *keyed {
	k := &keyed{byKey: map[string]value{}}
	parse := func(str string, key []string) (value, bool) {
		t, err := parseTemplate(str, allowed, s.b.userVars)
		if err != nil {
			s.problem(key, "%v", err)
			return value{}, false
		}
		if t.IsZero() {
			s.problem(key, "must not be empty")
			return value{}, false
		}
		return value{v: t, origin: s.origin(key, where)}, true
	}
	switch v := v.(type) {
	case string:
		if val, ok := parse(v, key); ok {
			k.all = &val
		}
	case map[string]any:
		for _, name := range slices.Sorted(maps.Keys(v)) {
			sub := append(append([]string{}, key...), name)
			if !s.checkKey(name, kind, sub) {
				continue
			}
			str, ok := v[name].(string)
			if !ok {
				s.problem(sub, "must be a string")
				continue
			}
			if val, ok := parse(str, sub); ok {
				k.byKey[name] = val
			}
		}
	default:
		what := "an os/arch table"
		if kind == keyOS {
			what = "an OS table"
		}
		s.problem(key, "must be a string or %s", what)
	}
	return k
}

// keyedBools reads true or false, or a table of those keyed by OS.
func (s *src) keyedBools(v any, key []string, where string) *keyed {
	k := &keyed{byKey: map[string]value{}}
	switch v := v.(type) {
	case bool:
		k.all = &value{v: v, origin: s.origin(key, where)}
	case map[string]any:
		for _, name := range slices.Sorted(maps.Keys(v)) {
			sub := append(append([]string{}, key...), name)
			if !s.checkKey(name, keyOS, sub) {
				continue
			}
			b, ok := v[name].(bool)
			if !ok {
				s.problem(sub, "must be true or false")
				continue
			}
			k.byKey[name] = value{v: b, origin: s.origin(sub, where)}
		}
	default:
		s.problem(key, "must be true or false, or an OS table of those")
	}
	return k
}

// keyedHooks reads a list of argv arrays, or a table of those keyed by OS.
func (s *src) keyedHooks(v any, key []string, where string) *keyed {
	k := &keyed{byKey: map[string]value{}}
	parse := func(v any, key []string) (value, bool) {
		list, ok := v.([]any)
		if !ok {
			s.problem(key, `must be a list of commands, each a list of strings: [["program", "arg"]]`)
			return value{}, false
		}
		var cmds [][]Template
		for i, c := range list {
			argv, ok := c.([]any)
			if !ok || len(argv) == 0 {
				s.problem(key, "command %d must be a non-empty list of strings (argv; no shell is used)", i+1)
				return value{}, false
			}
			var ts []Template
			for j, a := range argv {
				str, ok := a.(string)
				if !ok || (j == 0 && str == "") {
					s.problem(key, "command %d, argument %d must be a string (the program must not be empty)", i+1, j+1)
					return value{}, false
				}
				t, err := parseTemplate(str, forHook, s.b.userVars)
				if err != nil {
					s.problem(key, "command %d, argument %d: %v", i+1, j+1, err)
					return value{}, false
				}
				ts = append(ts, t)
			}
			cmds = append(cmds, ts)
		}
		return value{v: cmds, origin: s.origin(key, where)}, true
	}
	switch v := v.(type) {
	case []any:
		if val, ok := parse(v, key); ok {
			k.all = &val
		}
	case map[string]any:
		for _, name := range slices.Sorted(maps.Keys(v)) {
			sub := append(append([]string{}, key...), name)
			if !s.checkKey(name, keyOS, sub) {
				continue
			}
			if val, ok := parse(v[name], sub); ok {
				k.byKey[name] = val
			}
		}
	default:
		s.problem(key, `must be a list of commands ([["program", "arg"]]) or an OS table of them`)
	}
	return k
}

func parseStrip(v any) (archive.Strip, error) {
	switch v := v.(type) {
	case string:
		if v == "auto" {
			return archive.StripAuto, nil
		}
	case int64:
		if v >= 0 && v <= 32 {
			return archive.Strip{N: int(v)}, nil
		}
		return archive.Strip{}, errors.New("must be between 0 and 32")
	}
	return archive.Strip{}, errors.New(`must be "auto" or a number of leading folders to remove`)
}

var sizeRule = regexp.MustCompile(`^([0-9]+)\s*(B|KiB|MiB|GiB)?$`)

const maxSize = 64 << 30

// parseSize reads a byte count: a number, or a string with a binary unit ("500MiB", "2GiB").
func parseSize(v any) (int64, error) {
	var n int64
	switch v := v.(type) {
	case int64:
		n = v
	case string:
		m := sizeRule.FindStringSubmatch(v)
		if m == nil {
			return 0, fmt.Errorf("%q: write a number of bytes or a size such as \"500MiB\" or \"2GiB\"", v)
		}
		x, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is too large", v)
		}
		shift := map[string]uint{"": 0, "B": 0, "KiB": 10, "MiB": 20, "GiB": 30}[m[2]]
		if x > maxSize>>shift {
			return 0, fmt.Errorf("%q is larger than 64GiB", v)
		}
		n = x << shift
	default:
		return 0, errors.New(`must be a number of bytes or a size such as "500MiB"`)
	}
	if n < 1 || n > maxSize {
		return 0, errors.New("must be between 1 byte and 64GiB")
	}
	return n, nil
}

// ---- merging ----

// effective merges the layers (least specific first) into one package.
func (b *builder) effective(name string, layers []*layer, vars map[string]string) *Package {
	p := &Package{Name: name, vars: vars, keyed: map[string]*keyed{}, origins: map[string]Origin{}}
	scalars := map[string]value{}
	for _, l := range layers {
		maps.Copy(scalars, l.scalars)
		for kname, k := range l.keyed {
			m := p.keyed[kname]
			if m == nil || k.all != nil {
				// A plain value at a more specific layer covers every platform.
				m = &keyed{byKey: map[string]value{}}
				p.keyed[kname] = m
			}
			if k.all != nil {
				all := *k.all
				m.all = &all
			}
			maps.Copy(m.byKey, k.byKey)
		}
	}

	str := func(k, def string) string {
		if v, ok := scalars[k]; ok {
			return v.v.(string)
		}
		return def
	}
	p.Source = str(kSource, "github-release")
	p.Repo = str(kRepo, "")
	p.TagPrefix = str(kTagPrefix, "v")
	p.Channel = str(kChannel, "stable")
	p.API = str(kAPI, string(github.ModeAuto))
	p.Attestation = str(kVerifyAttest, "if-present")
	p.CooldownUnknown = str(kCooldownUnknown, "deny")
	p.HookFail = str(kHookFail, "warn")
	p.Pin = str(kPin, "")

	p.VerifyRequire = []string{"checksum"}
	p.Strip = archive.StripAuto
	p.Enabled = true
	p.HookTimeout = 120 * time.Second
	def := archive.DefaultLimits()
	p.Limits = Limits{MaxDownload: DefaultMaxDownload, MaxExtracted: def.MaxTotalBytes, MaxFiles: def.MaxEntries}
	for k, v := range scalars {
		p.origins[k] = v.origin
		switch k {
		case kChecksums:
			p.Checksums = v.v.(Template)
		case kVerifyRequire:
			p.VerifyRequire = v.v.([]string)
		case kStrip:
			p.Strip = v.v.(archive.Strip)
		case kPlatforms:
			p.platforms = v.v.([]platform.Platform)
		case kHold:
			p.Hold = v.v.(bool)
		case kCooldownDays:
			p.CooldownDays = v.v.(int)
		case kMaxDownload:
			p.Limits.MaxDownload = v.v.(int64)
		case kMaxExtracted:
			p.Limits.MaxExtracted = v.v.(int64)
		case kMaxFiles:
			p.Limits.MaxFiles = v.v.(int)
		case kEnabled:
			p.Enabled = v.v.(bool)
		case kHookTimeout:
			p.HookTimeout = v.v.(time.Duration)
		}
	}
	return p
}

// DefaultMaxDownload caps one downloaded asset unless limits.max_download says otherwise.
const DefaultMaxDownload = 4 << 30
