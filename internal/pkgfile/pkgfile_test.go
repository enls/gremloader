package pkgfile

import (
	"errors"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/platform"
)

// craftFile is the plan's E.2.1 example, made concrete. Windows paths use TOML literal strings
// ('...'), which is what the README will recommend: no backslash escaping.
const craftFile = `schema_version = 1

[vars]
root = 'C:\CraftWerk'

[settings]
channel = "stable"
api     = "auto"
verify  = { require = ["checksum"], attestation = "if-present" }

[preset.craft]
source    = "github-release"
checksums = "SHA256SUMS.txt"
asset = { "windows/amd64" = "{name}-{version}-windows-x64-portable.zip",
          "windows/arm64" = "{name}-{version}-windows-arm64-portable.zip",
          "windows/386"   = "{name}-{version}-windows-x86-portable.zip",
          "linux/amd64"   = "{name}-{version}-linux-x86_64.tar.gz",
          "linux/arm64"   = "{name}-{version}-linux-aarch64.tar.gz" }
dir  = { windows = '{root}\{Name}', linux = "~/.local" }
version_from_exe = { windows = "{name}.exe" }
post_install = { windows = [["powershell", "-NoProfile", "-File", '{root}\_update\register-apps.ps1', "-Only", "{name}"]] }
post_update  = { windows = [["powershell", "-NoProfile", "-File", '{root}\_update\register-apps.ps1', "-Only", "{name}"]] }
post_remove  = { windows = [["powershell", "-NoProfile", "-File", '{root}\_update\register-apps.ps1', "-Only", "{name}"]] }

[package.photocraft]
use = "craft"
repo = "storytold/photocraft"

[package.pdfcraft]
use  = "craft"
repo = "storytold/pdfcraft"
`

var (
	winAmd64   = platform.Platform{OS: "windows", Arch: "amd64"}
	linuxAmd64 = platform.Platform{OS: "linux", Arch: "amd64"}
)

func load(t *testing.T, main string, local string, opts Options) (*Config, error) {
	t.Helper()
	var ld []byte
	if local != "" {
		ld = []byte(local)
	}
	return parse("gremloader.toml", []byte(main), "gremloader.local.toml", ld, opts)
}

func mustLoad(t *testing.T, main, local string) *Config {
	t.Helper()
	c, err := load(t, main, local, Options{})
	if err != nil {
		t.Fatalf("load failed:\n%v", err)
	}
	return c
}

// problems returns the error's problems, failing the test if it is not an *Error.
func problems(t *testing.T, err error) []Problem {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error = %v (%T), want *Error", err, err)
	}
	return e.Problems
}

func wantProblem(t *testing.T, err error, line int, contains ...string) {
	t.Helper()
	for _, p := range problems(t, err) {
		if line != 0 && p.Line != line {
			continue
		}
		s := p.String()
		ok := true
		for _, c := range contains {
			if !strings.Contains(s, c) {
				ok = false
			}
		}
		if ok {
			return
		}
	}
	t.Errorf("no problem at line %d containing %q in:\n%v", line, contains, err)
}

func TestCraftExample(t *testing.T) {
	c := mustLoad(t, craftFile, "")
	if got := c.Names(); !reflect.DeepEqual(got, []string{"pdfcraft", "photocraft"}) {
		t.Fatalf("Names = %v", got)
	}
	p := c.Packages["photocraft"]
	v := Values{Version: "0.6.0", Tag: "v0.6.0", Platform: winAmd64}

	if a, err := p.AssetPattern(v); err != nil || a != "photocraft-0.6.0-windows-x64-portable.zip" {
		t.Errorf("windows asset = %q, %v", a, err)
	}
	v.Platform = linuxAmd64
	if a, err := p.AssetPattern(v); err != nil || a != "photocraft-0.6.0-linux-x86_64.tar.gz" {
		t.Errorf("linux asset = %q, %v", a, err)
	}
	if _, err := p.AssetPattern(Values{Version: "1.0.0", Platform: platform.Platform{OS: "darwin", Arch: "arm64"}}); err == nil ||
		!strings.Contains(err.Error(), "linux/amd64") {
		t.Errorf("darwin asset error = %v, want one that lists the platforms there are", err)
	}
	if n, ok, err := p.ChecksumsName(v, "x"); err != nil || !ok || n != "SHA256SUMS.txt" {
		t.Errorf("checksums = %q %v %v", n, ok, err)
	}

	if runtime.GOOS == "windows" {
		if d, err := p.Dir("windows", `C:\Users\ian`); err != nil || d != `C:\CraftWerk\Photocraft` {
			t.Errorf("windows dir = %q, %v", d, err)
		}
		if exe, ok, err := p.VersionFromExe("windows"); err != nil || !ok || exe != "photocraft.exe" {
			t.Errorf("version_from_exe = %q %v %v", exe, ok, err)
		}
	} else {
		if d, err := p.Dir("linux", "/home/ian"); err != nil || d != "/home/ian/.local" {
			t.Errorf("linux dir = %q, %v", d, err)
		}
	}
	if _, ok, _ := p.VersionFromExe("linux"); ok {
		t.Error("linux has a version_from_exe")
	}

	hooks, err := p.Hooks(PostInstall, Values{Version: "0.6.0", Platform: winAmd64})
	want := [][]string{{"powershell", "-NoProfile", "-File", `C:\CraftWerk\_update\register-apps.ps1`, "-Only", "photocraft"}}
	if err != nil || !reflect.DeepEqual(hooks, want) {
		t.Errorf("post_install = %q, %v", hooks, err)
	}
	if h, _ := p.Hooks(PostInstall, Values{Platform: linuxAmd64}); h != nil {
		t.Errorf("linux post_install = %q, want none", h)
	}
	if h, _ := p.Hooks(PreRemove, Values{Platform: winAmd64}); h != nil {
		t.Errorf("pre_remove = %q, want none", h)
	}

	// Defaults.
	if p.TagPrefix != "v" || p.Channel != "stable" || p.API != "auto" || !p.Strip.Auto || !p.Enabled ||
		p.HookTimeout != 120*time.Second || p.HookFail != "warn" || p.CooldownUnknown != "deny" ||
		!reflect.DeepEqual(p.VerifyRequire, []string{"checksum"}) {
		t.Errorf("defaults: %+v", p)
	}
	if got := len(p.Platforms()); got != 5 {
		t.Errorf("Platforms = %v, want the 5 asset keys", p.Platforms())
	}
}

func TestUnknownKeysAreErrorsWithPosition(t *testing.T) {
	_, err := load(t, `schema_version = 1
[package.photocraft]
repo = "storytold/photocraft"
dri = "x"
verify = { requires = ["checksum"] }
`, "", Options{})
	ps := problems(t, err)
	if len(ps) != 2 {
		t.Fatalf("want 2 problems, got:\n%v", err)
	}
	if ps[0].Line != 4 || ps[0].Col == 0 || !strings.Contains(ps[0].Key, "dri") {
		t.Errorf("first problem = %+v", ps[0])
	}
	if ps[1].Line != 5 || !strings.Contains(ps[1].Key, "requires") {
		t.Errorf("second problem = %+v", ps[1])
	}
	if !strings.HasPrefix(err.Error(), "gremloader.toml:4:") {
		t.Errorf("message does not start with file:line: %v", err)
	}
}

// A hash override is accepted only on the command line, for one package and version at a time
// (plan E.3.4): a package file, local or not, can never carry one.
func TestHashOverrideCannotComeFromAFile(t *testing.T) {
	for _, place := range []string{"[settings]\naccept_sha256 = \"" + strings.Repeat("ab", 32) + "\"\n",
		"[package.photocraft]\nrepo = \"storytold/photocraft\"\naccept_sha256 = \"" + strings.Repeat("ab", 32) + "\"\n",
		"[package.photocraft]\nrepo = \"storytold/photocraft\"\nverify = { accept_sha256 = \"x\" }\n"} {
		if _, err := load(t, "schema_version = 1\n"+place, "", Options{}); err == nil || !strings.Contains(err.Error(), "accept_sha256") {
			t.Errorf("accepted:\n%s\nerr = %v", place, err)
		}
	}
	if _, err := load(t, craftFile, "[package.photocraft]\naccept_sha256 = \"x\"\n", Options{}); err == nil {
		t.Error("the local override accepted a hash override")
	}
}

func TestSyntaxErrorHasPosition(t *testing.T) {
	_, err := load(t, "schema_version = 1\n[package.x]\nrepo = \"a/b\n", "", Options{})
	ps := problems(t, err)
	if len(ps) != 1 || ps[0].Line != 3 {
		t.Errorf("problems = %+v", ps)
	}
}

func TestWrongTypeIsAnError(t *testing.T) {
	_, err := load(t, "schema_version = 1\n[package.x]\nrepo = \"a/b\"\nhold = \"yes\"\n", "", Options{})
	if err == nil {
		t.Fatal("hold = \"yes\" accepted")
	}
	if ps := problems(t, err); ps[0].Line != 4 {
		t.Errorf("type error at line %d, want 4: %v", ps[0].Line, err)
	}
}

func TestSchemaVersion(t *testing.T) {
	_, err := load(t, "[package.x]\nrepo = \"a/b\"\n", "", Options{})
	wantProblem(t, err, 1, "schema_version is missing")

	// Newer: refused before the unknown key is looked at.
	_, err = load(t, "schema_version = 2\nbrand_new_key = true\n", "", Options{})
	if err == nil || !strings.Contains(err.Error(), ErrNewerSchema.Error()) || strings.Contains(err.Error(), "unknown key") {
		t.Errorf("newer schema: %v", err)
	}
	_, err = load(t, "schema_version = 0\n", "", Options{})
	wantProblem(t, err, 0, "must be 1 or more")
	_, err = load(t, craftFile, "schema_version = 2\n", Options{})
	wantProblem(t, err, 0, "gremloader.local.toml", ErrNewerSchema.Error())
}

const mergeFile = `schema_version = 1

[settings]
cooldown_days = 7
channel = "stable"
verify = { require = ["checksum", "digest"] }

[preset.base]
checksums = "SHA256SUMS.txt"
asset = { "windows/amd64" = "{name}-win.zip", "linux/amd64" = "{name}-linux.tar.gz" }
dir = { windows = 'C:\Apps\{name}', linux = "/opt/{name}" }
cooldown_days = 3
limits = { max_download = "500MiB", max_files = 100 }

[package.a]
use = "base"
repo = "o/a"
asset."windows/amd64" = "{name}-special.zip"
verify.require = ["digest"]
limits.max_files = 5

[package.b]
use = "base"
repo = "o/b"
asset = "{name}-{os}-{arch}.zip"
cooldown_days = 0

[package.c]
repo = "o/c"
asset = "c.zip"
dir = "/srv/c"
checksums = "c.sha256"
`

func TestPresetMerge(t *testing.T) {
	c := mustLoad(t, mergeFile, "")
	a, b, cc := c.Packages["a"], c.Packages["b"], c.Packages["c"]

	// One platform overridden, the other kept from the preset.
	if s, _ := a.AssetPattern(Values{Platform: winAmd64}); s != "a-special.zip" {
		t.Errorf("a windows asset = %q", s)
	}
	if s, _ := a.AssetPattern(Values{Platform: linuxAmd64}); s != "a-linux.tar.gz" {
		t.Errorf("a linux asset = %q", s)
	}
	// A plain value replaces the whole table.
	if s, _ := b.AssetPattern(Values{Platform: linuxAmd64}); s != "b-linux-amd64.zip" {
		t.Errorf("b linux asset = %q", s)
	}
	if b.Platforms() != nil {
		t.Errorf("b Platforms = %v, want nil (a single asset string names no platforms)", b.Platforms())
	}
	// Lists replace; scalars: package > preset > settings.
	if !reflect.DeepEqual(a.VerifyRequire, []string{"digest"}) || !reflect.DeepEqual(b.VerifyRequire, []string{"checksum", "digest"}) {
		t.Errorf("verify.require a=%v b=%v", a.VerifyRequire, b.VerifyRequire)
	}
	if a.CooldownDays != 3 || b.CooldownDays != 0 || cc.CooldownDays != 7 {
		t.Errorf("cooldown a=%d b=%d c=%d, want 3 0 7", a.CooldownDays, b.CooldownDays, cc.CooldownDays)
	}
	// Tables merge per key.
	if a.Limits.MaxFiles != 5 || a.Limits.MaxDownload != 500<<20 || b.Limits.MaxFiles != 100 {
		t.Errorf("limits a=%+v b=%+v", a.Limits, b.Limits)
	}
	if cc.Limits.MaxDownload != DefaultMaxDownload {
		t.Errorf("c limits = %+v, want defaults", cc.Limits)
	}
}

func TestExplainSaysWhereEachValueCameFrom(t *testing.T) {
	c := mustLoad(t, mergeFile, "schema_version = 1\n\n[package.a]\npin = \"v1.2\"\n")
	got := map[string]Setting{}
	for _, s := range c.Packages["a"].Explain() {
		got[s.Key] = s
	}
	check := func(key, value, origin string) {
		t.Helper()
		s, ok := got[key]
		if !ok {
			t.Errorf("explain has no %s", key)
			return
		}
		if s.Value != value || s.Origin.String() != origin {
			t.Errorf("%s = %s from %q, want %s from %q", key, s.Value, s.Origin, value, origin)
		}
	}
	check(`asset."windows/amd64"`, `"{name}-special.zip"`, "gremloader.toml:18 [package.a]")
	check(`asset."linux/amd64"`, `"{name}-linux.tar.gz"`, "gremloader.toml:10 [preset.base]")
	check("cooldown_days", "3", "gremloader.toml:12 [preset.base]")
	check("channel", `"stable"`, "gremloader.toml:5 [settings]")
	check("limits.max_files", "5", "gremloader.toml:20 [package.a]")
	check("limits.max_download", "524288000", "gremloader.toml:13 [preset.base]")
	check("pin", `"1.2.0"`, "gremloader.local.toml:4 [package.a]")
	check("hold", "false", "default")
	check("strip", `"auto"`, "default")
}

func TestPresetRules(t *testing.T) {
	_, err := load(t, `schema_version = 1
[preset.a]
use = "b"
repo = "o/r"
[preset.b]
asset = "x"
[settings]
pin = "1.0.0"
[package.x]
use = "nope"
repo = "o/x"
`, "", Options{})
	wantProblem(t, err, 3, "use", "belongs to a [package.<name>] table")
	wantProblem(t, err, 4, "repo", "belongs to a [package.<name>] table")
	wantProblem(t, err, 8, "pin", "belongs to a [package.<name>] table")
	wantProblem(t, err, 10, `no preset named "nope"`, "a, b")
}

func TestRequiredKeys(t *testing.T) {
	_, err := load(t, "schema_version = 1\n[package.x]\nenabled = true\n", "", Options{})
	wantProblem(t, err, 0, "package.x.repo", "missing")
	wantProblem(t, err, 0, "package.x.asset", "missing")
	wantProblem(t, err, 0, "package.x.dir", "missing")
	wantProblem(t, err, 0, "package.x.checksums", `verify.require includes "checksum"`)

	// Opting out of verification is allowed, loudly.
	c := mustLoad(t, "schema_version = 1\n[package.x]\nrepo = \"o/x\"\nasset = \"x.zip\"\ndir = \"/x\"\nverify.require = []\n", "")
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "without any verification") {
		t.Errorf("warnings = %q", c.Warnings)
	}
}

func TestValueRules(t *testing.T) {
	_, err := load(t, `schema_version = 1
[package.x]
repo = "not-a-repo"
asset = { "window/amd64" = "x.zip", "linux/x86_64" = "y.zip", "linux/amd64" = 3 }
dir = { windws = "C:/x" }
checksums = "SUMS"
channel = "beta"
api = "graphql"
strip = "yes"
cooldown_days = -1
cooldown_unknown = "maybe"
limits = { max_download = "5 TB", max_files = 0, max_extracted = "65GiB" }
hook_fail = "explode"
hook_timeout = "2 hours"
tag_prefix = "app/v"
pin = "latest"
platforms = ["windows/amd64", "linux/riscv"]
post_install = "register.ps1"
post_update = [[]]
pre_remove = [["", "x"]]
verify = { require = ["gpg"], attestation = "required" }
`, "", Options{})
	for line, want := range map[int]string{
		3: "is not owner/name", 5: "not an operating system", 7: `"beta" is not one of`, 8: `"graphql"`,
		9: `"auto" or a number`, 10: "between 0 and 3650", 11: `"maybe"`, 13: `"explode"`, 14: "duration",
		15: "tag_prefix", 16: "latest", 17: "riscv", 18: "list of commands", 19: "command 1", 20: "program must not be empty",
	} {
		wantProblem(t, err, line, want)
	}
	wantProblem(t, err, 4, "window/amd64", "unknown operating system")
	wantProblem(t, err, 4, "x86_64", "unknown architecture")
	wantProblem(t, err, 4, "linux/amd64", "must be a string")
	wantProblem(t, err, 12, "max_download", "5 TB")
	wantProblem(t, err, 12, "max_files", "between 1 and")
	wantProblem(t, err, 12, "max_extracted", "larger than 64GiB")
	wantProblem(t, err, 21, "gpg", "not a verification level")
	wantProblem(t, err, 21, `"required" is not one of`)
}

func TestSizes(t *testing.T) {
	for in, want := range map[any]int64{int64(1): 1, "1": 1, "10B": 10, "2KiB": 2048, "500MiB": 500 << 20, "2GiB": 2 << 30, "64GiB": 64 << 30, "3 MiB": 3 << 20} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%v) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []any{int64(0), int64(-5), "0", "1.5GiB", "2GB", "2 gib", "", "99999999999999999999", "65GiB", "68719476737", 1.5} {
		if got, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%v) = %d, want an error", in, got)
		}
	}
}

func TestStrip(t *testing.T) {
	c := mustLoad(t, "schema_version = 1\n[package.x]\nrepo = \"o/x\"\nasset = \"x.zip\"\ndir = \"/x\"\nchecksums = \"s\"\nstrip = 0\n", "")
	if s := c.Packages["x"].Strip; s.Auto || s.N != 0 {
		t.Errorf("strip = 0 gave %+v", s)
	}
}

func TestNamesAreValidated(t *testing.T) {
	_, err := load(t, "schema_version = 1\n[package.PhotoCraft]\nrepo = \"o/x\"\n[package.con]\nrepo = \"o/y\"\n[preset.\"Bad Preset\"]\n", "", Options{})
	wantProblem(t, err, 2, "PhotoCraft", "not valid")
	wantProblem(t, err, 4, "con", "not valid")
	wantProblem(t, err, 6, "preset name")
}

func TestManyProblemsAreReportedAtOnceInLineOrder(t *testing.T) {
	_, err := load(t, "schema_version = 1\n[package.x]\nchannel = \"a\"\napi = \"b\"\nhook_fail = \"c\"\n", "", Options{})
	ps := problems(t, err)
	if len(ps) < 3 {
		t.Fatalf("problems = %v", ps)
	}
	for i := 1; i < len(ps); i++ {
		if ps[i].Line != 0 && ps[i].Line < ps[i-1].Line {
			t.Errorf("problems out of order: %v", err)
		}
	}
}

// Gaps found by the mutation spot-check.
func TestBoundsAndSmallRules(t *testing.T) {
	base := "schema_version = 1\n[package.x]\nrepo = \"o/x\"\nasset = { \"linux/amd64\" = \"x.zip\" }\ndir = \"/x\"\nchecksums = \"s\"\n"
	for extra, want := range map[string]string{
		"strip = 33\n":                      "between 0 and 32",
		"hook_timeout = \"2h\"\n":           "between 1s and 1h",
		"hook_timeout = \"500ms\"\n":        "between 1s and 1h",
		"platforms = [\"windows/amd64\"]\n": "windows/amd64 is listed but asset has no entry",
	} {
		_, err := load(t, base+extra, "", Options{})
		wantProblem(t, err, 0, want)
	}

	c := mustLoad(t, base+"verify.require = [\"checksum\", \"digest\", \"checksum\"]\nhook_timeout = \"1h\"\nstrip = 32\n", "")
	p := c.Packages["x"]
	if !reflect.DeepEqual(p.VerifyRequire, []string{"checksum", "digest"}) {
		t.Errorf("verify.require = %v, want duplicates removed", p.VerifyRequire)
	}
	if p.HookTimeout != time.Hour || p.Strip.N != 32 {
		t.Errorf("bounds themselves rejected: %v %+v", p.HookTimeout, p.Strip)
	}

	// The checksum file is a name to download, not a pattern.
	c = mustLoad(t, strings.Replace(base, "checksums = \"s\"", "checksums = \"SUMS-*.txt\"", 1), "")
	if s, _, err := c.Packages["x"].ChecksumsName(Values{Platform: linuxAmd64}, "x.zip"); err == nil {
		t.Errorf("checksums with a wildcard = %q, want an error", s)
	}
}

func TestHomeExpansion(t *testing.T) {
	c := mustLoad(t, "schema_version = 1\n[package.x]\nrepo = \"o/x\"\nasset = \"x.zip\"\nchecksums = \"s\"\ndir = \"~/Apps/{Name}\"\n", "")
	home := "/home/ian"
	if runtime.GOOS == "windows" {
		home = `C:\Users\ian`
	}
	d, err := c.Packages["x"].Dir(runtime.GOOS, home)
	if err != nil || d != filepath.Join(home, "Apps", "X") {
		t.Errorf("dir = %q, %v; want %q", d, err, filepath.Join(home, "Apps", "X"))
	}
	// "~user" is not the home directory.
	c = mustLoad(t, "schema_version = 1\n[package.x]\nrepo = \"o/x\"\nasset = \"x.zip\"\nchecksums = \"s\"\ndir = \"~ian/Apps\"\n", "")
	if d, err := c.Packages["x"].Dir(runtime.GOOS, home); err == nil {
		t.Errorf("dir \"~ian/Apps\" = %q, want an error", d)
	}
}

// shared_dir (decision 2 of the M5 bug check): a folder several packages install into, such as
// ~/.local. A bool, or an OS table of bools, merged per OS like dir.
func TestSharedDir(t *testing.T) {
	c := mustLoad(t, `schema_version = 1
[preset.p]
checksums = "s"
asset = "x.zip"
dir = { windows = 'C:\Apps\{name}', linux = "~/.local" }
shared_dir = { linux = true }

[package.a]
use = "p"
repo = "o/a"

[package.b]
use = "p"
repo = "o/b"
shared_dir.windows = true

[package.c]
use = "p"
repo = "o/c"
shared_dir = false

[package.d]
repo = "o/d"
asset = "d.zip"
checksums = "s"
dir = "/d"
`, "")
	for _, x := range []struct {
		pkg, goos string
		want      bool
	}{
		{"a", "linux", true}, {"a", "windows", false},
		{"b", "linux", true}, {"b", "windows", true}, // one OS overridden, the other kept
		{"c", "linux", false}, {"c", "windows", false}, // a plain value covers every OS
		{"d", "linux", false}, // default
	} {
		if got := c.Packages[x.pkg].SharedDir(x.goos); got != x.want {
			t.Errorf("%s SharedDir(%s) = %v, want %v", x.pkg, x.goos, got, x.want)
		}
	}

	got := map[string]Setting{}
	for _, s := range c.Packages["b"].Explain() {
		got[s.Key] = s
	}
	if s := got[`shared_dir."linux"`]; s.Value != "true" || s.Origin.String() != "gremloader.toml:6 [preset.p]" {
		t.Errorf("explain shared_dir.linux = %+v", s)
	}
	if s := got[`shared_dir."windows"`]; s.Value != "true" || s.Origin.String() != "gremloader.toml:15 [package.b]" {
		t.Errorf("explain shared_dir.windows = %+v", s)
	}
	for _, s := range c.Packages["d"].Explain() {
		if s.Key == "shared_dir" && (s.Value != "false" || s.Origin.String() != "default") {
			t.Errorf("explain d shared_dir = %+v, want false from default", s)
		}
	}

	base := "schema_version = 1\n[package.x]\nrepo = \"o/x\"\nasset = \"x.zip\"\ndir = \"/x\"\nchecksums = \"s\"\n"
	_, err := load(t, base+"shared_dir = \"yes\"\n", "", Options{})
	wantProblem(t, err, 7, "shared_dir", "true or false")
	_, err = load(t, base+"shared_dir = { linx = true }\n", "", Options{})
	wantProblem(t, err, 7, "linx", "not an operating system")
	_, err = load(t, base+"shared_dir = { linux = 1 }\n", "", Options{})
	wantProblem(t, err, 7, "shared_dir.linux", "true or false")

	// Where a package installs is the package file's business, never the local override's.
	_, err = load(t, base, "schema_version = 1\n[package.x]\nshared_dir = true\n", Options{})
	wantProblem(t, err, 3, "gremloader.local.toml", "not allowed in the local override")
}
