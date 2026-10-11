package state

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/jsonfile"
)

var (
	hash = strings.Repeat("ab", 32)
	t0   = time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
)

func absDir(name string) string {
	if runtime.GOOS == "windows" {
		return `C:\CraftWerk\` + name
	}
	return "/opt/" + name
}

func installed() *Package {
	at := t0
	return &Package{
		Version: "0.6.0", Tag: "v0.6.0", Dir: absDir("Photocraft"),
		Asset: "photocraft-0.6.0-windows-x64-portable.zip", AssetSHA256: hash,
		InstalledAt: &at, VerifiedBy: []string{"checksum", "digest"},
		Manifest:   ManifestRel("photocraft", "0.6.0"),
		Previous:   &Previous{Version: "0.5.0", Undo: "previous/photocraft/undo.jsonl"},
		Rejected:   []string{"0.6.1"},
		LatestSeen: &Seen{Version: "0.6.1", PublishedAt: &at, At: t0},
		LastCheck:  &at,
	}
}

func TestMissingStateIsEmpty(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil || len(s.Packages) != 0 || s.SchemaVersion != SchemaVersion {
		t.Fatalf("Load of a missing file = %+v, %v", s, err)
	}
}

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "state.json")
	s := New()
	s.Packages["photocraft"] = installed()
	s.Packages["wordcraft"] = &Package{LatestSeen: &Seen{Version: "1.0.0", At: t0}, LastCheck: &t0, LastError: "rate limited"}
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Errorf("round trip changed the state:\n got %+v\nwant %+v", got.Packages["photocraft"], s.Packages["photocraft"])
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "null") {
		t.Errorf("unset fields are written as null:\n%s", raw)
	}
}

func TestDamagedStateIsAnErrorNotEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"packages":{"photocraft":{"version":"0.6`), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Load(path); !errors.Is(err, jsonfile.ErrCorrupt) || s != nil {
		t.Fatalf("Load of a truncated file = %v, %v; want ErrCorrupt and no state", s, err)
	}
}

func TestNewerStateIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":2,"packages":{},"history":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); !errors.Is(err, jsonfile.ErrNewerSchema) {
		t.Fatalf("Load = %v, want ErrNewerSchema", err)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]func(p *Package){
		"bad version":             func(p *Package) { p.Version = "latest" },
		"relative dir":            func(p *Package) { p.Dir = "CraftWerk/Photocraft" },
		"no installed_at":         func(p *Package) { p.InstalledAt = nil },
		"bad tag":                 func(p *Package) { p.Tag = "../v1" },
		"asset with a slash":      func(p *Package) { p.Asset = "../x.zip" },
		"short hash":              func(p *Package) { p.AssetSHA256 = "abc" },
		"upper-case hash":         func(p *Package) { p.AssetSHA256 = strings.ToUpper(hash) },
		"bad level":               func(p *Package) { p.VerifiedBy = []string{"Checksum!"} },
		"manifest escapes":        func(p *Package) { p.Manifest = "../../Windows/x.json" },
		"manifest absolute":       func(p *Package) { p.Manifest = "/etc/x.json" },
		"manifest drive":          func(p *Package) { p.Manifest = "C:/x.json" },
		"manifest backslash":      func(p *Package) { p.Manifest = `manifests\x.json` },
		"manifest device":         func(p *Package) { p.Manifest = "manifests/NUL" },
		"undo escapes":            func(p *Package) { p.Previous.Undo = "previous/../../x" },
		"undo empty":              func(p *Package) { p.Previous.Undo = "" },
		"previous manifest bad":   func(p *Package) { p.Previous.Manifest = "/x" },
		"previous version bad":    func(p *Package) { p.Previous.Version = "" },
		"rejected bad":            func(p *Package) { p.Rejected = []string{"x"} },
		"latest seen bad":         func(p *Package) { p.LatestSeen.Version = "" },
		"huge last error":         func(p *Package) { p.LastError = strings.Repeat("x", 5000) },
		"details without version": func(p *Package) { p.Version = "" },
	}
	for name, mutate := range cases {
		p := installed()
		mutate(p)
		s := New()
		s.Packages["photocraft"] = p
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if err := Save(filepath.Join(t.TempDir(), "state.json"), s); err == nil {
			t.Errorf("%s: Save wrote an invalid state", name)
		}
	}

	for _, name := range []string{"Photocraft", "con", "../x", ""} {
		s := New()
		s.Packages[name] = installed()
		if s.Validate() == nil {
			t.Errorf("package name %q accepted", name)
		}
	}
	s := New()
	s.Packages["x"] = nil
	if s.Validate() == nil {
		t.Error("a null package record was accepted")
	}
}

// A state.json edited by hand to point the undo log somewhere else is refused on Load.
func TestTamperedPathIsRefusedOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	data := `{"schema_version":1,"packages":{"photocraft":{"version":"0.6.0","dir":` + jsonString(absDir("P")) +
		`,"installed_at":"2026-10-10T09:00:00Z","previous":{"version":"0.5.0","undo":"../../../Users/x/undo.jsonl"}}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); !errors.Is(err, jsonfile.ErrCorrupt) || !strings.Contains(err.Error(), "undo") {
		t.Fatalf("Load = %v, want ErrCorrupt naming the undo path", err)
	}
}

func TestPathStaysUnderTheStateDirectory(t *testing.T) {
	dir := absDir("state")
	if got := Path(dir, "manifests/photocraft/0.6.0.json"); got != filepath.Join(dir, "manifests", "photocraft", "0.6.0.json") {
		t.Errorf("Path = %q", got)
	}
}

func TestManifestRoundTripIsSorted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifests", "photocraft", "0.6.0.json")
	m := &Manifest{
		Files: []ManifestFile{
			{Path: "sub/b.dll", Size: 2, SHA256: hash},
			{Path: "photocraft.exe", Size: 89481080, SHA256: hash, Exec: true},
		},
		Dirs: []string{"sub/deeper", "sub"},
	}
	if err := SaveManifest(path, m); err != nil {
		t.Fatal(err)
	}
	got, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Files[0].Path != "photocraft.exe" || got.Dirs[0] != "sub" || !got.Files[0].Exec {
		t.Errorf("manifest not sorted or not round-tripped: %+v", got)
	}
}

func TestEmptyManifestIsWrittenAsEmptyLists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.json")
	if err := SaveManifest(path, &Manifest{}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"files": []`) || !strings.Contains(string(raw), `"dirs": []`) {
		t.Errorf("empty manifest written as:\n%s", raw)
	}
}

func TestMissingManifestIsAnError(t *testing.T) {
	if _, err := LoadManifest(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Fatal("LoadManifest of a missing file succeeded")
	}
}

func TestManifestValidation(t *testing.T) {
	ok := ManifestFile{Path: "a.exe", SHA256: hash}
	cases := map[string]Manifest{
		"dot-dot":             {Files: []ManifestFile{{Path: "../a", SHA256: hash}}},
		"absolute":            {Files: []ManifestFile{{Path: "/a", SHA256: hash}}},
		"backslash":           {Files: []ManifestFile{{Path: `sub\a`, SHA256: hash}}},
		"empty component":     {Files: []ManifestFile{{Path: "sub//a", SHA256: hash}}},
		"dot component":       {Files: []ManifestFile{{Path: "./a", SHA256: hash}}},
		"trailing slash":      {Dirs: []string{"sub/"}},
		"duplicate file":      {Files: []ManifestFile{ok, ok}},
		"file and dir":        {Files: []ManifestFile{ok}, Dirs: []string{"a.exe"}},
		"file as parent":      {Files: []ManifestFile{ok, {Path: "a.exe/x", SHA256: hash}}},
		"file as grandparent": {Files: []ManifestFile{ok, {Path: "a.exe/x/y", SHA256: hash}}},
		"negative size":       {Files: []ManifestFile{{Path: "a", Size: -1, SHA256: hash}}},
		"bad hash":            {Files: []ManifestFile{{Path: "a", SHA256: "x"}}},
	}
	for name, m := range cases {
		if err := m.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good := Manifest{Files: []ManifestFile{ok, {Path: "sub/x/y.dll", SHA256: hash}}, Dirs: []string{"sub", "sub/x"}}
	if err := good.Validate(); err != nil {
		t.Errorf("valid manifest rejected: %v", err)
	}
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}
