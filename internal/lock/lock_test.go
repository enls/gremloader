package lock

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/jsonfile"
	"github.com/enls/gremloader/internal/platform"
)

var hash = strings.Repeat("0f", 32)

func sample() *File {
	f := New("gremloader 0.1.0", time.Date(2026, 10, 11, 9, 0, 0, 123456789, time.FixedZone("CEST", 2*3600)))
	pub := time.Date(2026, 10, 10, 2, 51, 42, 0, time.UTC)
	f.Packages["photocraft"] = &Package{
		Repo: "storytold/photocraft", Tag: "v0.6.0", Version: "0.6.0", PublishedAt: &pub,
		Platforms: map[string]*Asset{
			"windows/amd64": {Asset: "photocraft-0.6.0-windows-x64-portable.zip",
				URL:    "https://github.com/storytold/photocraft/releases/download/v0.6.0/photocraft-0.6.0-windows-x64-portable.zip",
				SHA256: hash, Size: 75465463},
			"linux/amd64": {Asset: "photocraft-0.6.0-linux-x86_64.tar.gz",
				URL:    "https://github.com/storytold/photocraft/releases/download/v0.6.0/photocraft-0.6.0-linux-x86_64.tar.gz",
				SHA256: hash, Size: 70000000},
		},
	}
	return f
}

func TestRoundTripAndStableOutput(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.lock"), filepath.Join(dir, "b.lock")
	if err := Save(a, sample()); err != nil {
		t.Fatal(err)
	}
	if err := Save(b, sample()); err != nil {
		t.Fatal(err)
	}
	ra, _ := os.ReadFile(a)
	rb, _ := os.ReadFile(b)
	if !bytes.Equal(ra, rb) {
		t.Error("the same lock written twice differs")
	}
	if !strings.Contains(string(ra), `"generated_at": "2026-10-11T07:00:00Z"`) {
		t.Errorf("generated_at not UTC whole seconds:\n%s", ra)
	}
	if strings.Index(string(ra), `"linux/amd64"`) > strings.Index(string(ra), `"windows/amd64"`) {
		t.Error("platforms not in sorted order")
	}

	f, found, err := Load(a)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	_, asset, err := f.Get("photocraft", platform.Platform{OS: "windows", Arch: "amd64"})
	if err != nil || asset.Size != 75465463 {
		t.Fatalf("Get = %+v, %v", asset, err)
	}
}

func TestMissingLock(t *testing.T) {
	f, found, err := Load(filepath.Join(t.TempDir(), "gremloader.lock"))
	if f != nil || found || err != nil {
		t.Errorf("Load of a missing lock = %v, %v, %v", f, found, err)
	}
}

func TestGetNamesWhatIsMissing(t *testing.T) {
	f := sample()
	if _, _, err := f.Get("wordcraft", platform.Current()); !errors.Is(err, ErrNotLocked) {
		t.Errorf("Get of an unlocked package = %v", err)
	}
	_, _, err := f.Get("photocraft", platform.Platform{OS: "windows", Arch: "arm64"})
	if !errors.Is(err, ErrNotLocked) || !strings.Contains(err.Error(), "linux/amd64, windows/amd64") {
		t.Errorf("Get of an unlocked platform = %v", err)
	}
}

func TestValidation(t *testing.T) {
	win := "windows/amd64"
	cases := map[string]func(f *File){
		"bad name":     func(f *File) { f.Packages["Photo Craft"] = f.Packages["photocraft"] },
		"nil package":  func(f *File) { f.Packages["x"] = nil },
		"bad repo":     func(f *File) { f.Packages["photocraft"].Repo = "photocraft" },
		"bad tag":      func(f *File) { f.Packages["photocraft"].Tag = "v1/../2" },
		"bad version":  func(f *File) { f.Packages["photocraft"].Version = "v0.6.0" },
		"no platforms": func(f *File) { f.Packages["photocraft"].Platforms = nil },
		"bad platform": func(f *File) {
			f.Packages["photocraft"].Platforms["windows/x64"] = f.Packages["photocraft"].Platforms[win]
		},
		"nil asset":    func(f *File) { f.Packages["photocraft"].Platforms[win] = nil },
		"asset path":   func(f *File) { f.Packages["photocraft"].Platforms[win].Asset = "../x.zip" },
		"short hash":   func(f *File) { f.Packages["photocraft"].Platforms[win].SHA256 = "00" },
		"zero size":    func(f *File) { f.Packages["photocraft"].Platforms[win].Size = 0 },
		"ftp url":      func(f *File) { f.Packages["photocraft"].Platforms[win].URL = "ftp://github.com/x.zip" },
		"relative url": func(f *File) { f.Packages["photocraft"].Platforms[win].URL = "/x.zip" },
		"credentials":  func(f *File) { f.Packages["photocraft"].Platforms[win].URL = "https://u:p@github.com/x.zip" },
		"signed cdn link": func(f *File) {
			f.Packages["photocraft"].Platforms[win].URL = "https://release-assets.githubusercontent.com/x?sig=abc"
		},
		"fragment": func(f *File) { f.Packages["photocraft"].Platforms[win].URL = "https://github.com/x.zip#y" },
	}
	for name, mutate := range cases {
		f := sample()
		mutate(f)
		if err := f.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if err := Save(filepath.Join(t.TempDir(), "gremloader.lock"), f); err == nil {
			t.Errorf("%s: Save wrote an invalid lock", name)
		}
	}
}

func TestDamagedAndNewerLocks(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct {
		data string
		want error
	}{
		"truncated": {`{"schema_version":1,"packages":{`, jsonfile.ErrCorrupt},
		"unknown":   {`{"schema_version":1,"packages":{},"extra":1}`, jsonfile.ErrCorrupt},
		"newer":     {`{"schema_version":2,"packages":{},"extra":1}`, jsonfile.ErrNewerSchema},
		"invalid":   {`{"schema_version":1,"packages":{"x":{"repo":"a/b","tag":"v1","version":"1.0.0","platforms":{}}}}`, jsonfile.ErrCorrupt},
	} {
		path := filepath.Join(dir, name+".lock")
		if err := os.WriteFile(path, []byte(c.data), 0o600); err != nil {
			t.Fatal(err)
		}
		if f, found, err := Load(path); !errors.Is(err, c.want) || f != nil || !found {
			t.Errorf("%s: Load = %v, %v, %v; want %v", name, f, found, err, c.want)
		}
	}
}

func TestOverride(t *testing.T) {
	win := platform.Platform{OS: "windows", Arch: "amd64"}
	other := strings.Repeat("ab", 32)
	accepted := Asset{Asset: "photocraft-0.6.0-windows-x64-portable.zip",
		URL:    "https://github.com/storytold/photocraft/releases/download/v0.6.0/photocraft-0.6.0-windows-x64-portable.zip",
		SHA256: other, Size: 75465464}

	f := sample()
	if !f.Override("photocraft", "0.6.0", win, accepted) {
		t.Fatal("not recorded")
	}
	got := f.Packages["photocraft"].Platforms["windows/amd64"]
	if got.SHA256 != other || got.Size != 75465464 || got.VerifiedBy != HashOverride {
		t.Errorf("entry = %+v", got)
	}
	if f.Packages["photocraft"].Platforms["linux/amd64"].VerifiedBy != "" {
		t.Error("another platform was marked")
	}
	path := filepath.Join(t.TempDir(), "gremloader.lock")
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Count(string(data), `"verified_by": "hash-override"`) != 1 {
		t.Errorf("lock file:\n%s", data)
	}
	if back, _, err := Load(path); err != nil || back.Packages["photocraft"].Platforms["windows/amd64"].VerifiedBy != HashOverride {
		t.Errorf("Load = %v", err)
	}

	for name, call := range map[string]func(f *File) bool{
		"other version": func(f *File) bool { return f.Override("photocraft", "0.6.1", win, accepted) },
		"other package": func(f *File) bool { return f.Override("wordcraft", "0.6.0", win, accepted) },
		"other platform": func(f *File) bool {
			return f.Override("photocraft", "0.6.0", platform.Platform{OS: "windows", Arch: "arm64"}, accepted)
		},
	} {
		f := sample()
		if call(f) || f.Packages["photocraft"].Platforms["windows/amd64"].SHA256 != hash {
			t.Errorf("%s: the lock was changed", name)
		}
	}
}

func TestOnlyHashOverrideIsAMarker(t *testing.T) {
	f := sample()
	f.Packages["photocraft"].Platforms["windows/amd64"].VerifiedBy = "checksum"
	if err := f.Validate(); err == nil {
		t.Error("verified_by = checksum accepted")
	}
}
