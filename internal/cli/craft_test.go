package cli

import (
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/pkgfile"
	"github.com/enls/gremloader/internal/platform"
)

// craftFile is the shipped package file for the twelve Craft apps (examples/craft).
var craftFile = filepath.Join("..", "..", "examples", "craft", "gremloader.toml")

var craftApps = []string{"cadcraft", "deckcraft", "designcraft", "effectcraft", "filmcraft", "gridcraft",
	"lightcraft", "pdfcraft", "photocraft", "soundcraft", "vectorcraft", "wordcraft"}

func loadCraft(t *testing.T, root string) *pkgfile.Config {
	t.Helper()
	var opts pkgfile.Options
	if root != "" {
		opts.Vars = map[string]string{"root": root}
	}
	cfg, err := pkgfile.Load(craftFile, opts)
	if err != nil {
		t.Fatalf("examples/craft/gremloader.toml: %v", err)
	}
	if len(cfg.Warnings) > 0 {
		t.Errorf("warnings: %q", cfg.Warnings)
	}
	return cfg
}

func TestCraftFileNamesTheTwelveApps(t *testing.T) {
	cfg := loadCraft(t, "")
	if got := cfg.Names(); !reflect.DeepEqual(got, craftApps) {
		t.Fatalf("packages = %v", got)
	}
	for _, n := range craftApps {
		p := cfg.Packages[n]
		if p.Repo != "storytold/"+n || !reflect.DeepEqual(p.VerifyRequire, []string{"checksum"}) || p.Channel != "stable" {
			t.Errorf("%s: repo %q verify %v channel %q", n, p.Repo, p.VerifyRequire, p.Channel)
		}
	}
}

// The asset names as released, read from photocraft v0.7.0 and pdfcraft v0.6.0 with
// `gh release view` on 2026-10-11. Each pattern is an exact name, so pdfcraft's second linux
// tarball (pdfcraft-cli-<v>-linux-*.tar.gz) can never be the one chosen.
func TestCraftFileAssetsAreTheReleasedNames(t *testing.T) {
	cfg := loadCraft(t, "")
	released := map[string]string{
		"windows/amd64": "photocraft-0.7.0-windows-x64-portable.zip",
		"windows/arm64": "photocraft-0.7.0-windows-arm64-portable.zip",
		"windows/386":   "photocraft-0.7.0-windows-x86-portable.zip",
		"linux/amd64":   "photocraft-0.7.0-linux-x86_64.tar.gz",
		"linux/arm64":   "photocraft-0.7.0-linux-aarch64.tar.gz",
	}
	for _, n := range craftApps {
		p := cfg.Packages[n]
		if got := len(p.Platforms()); got != len(released) {
			t.Errorf("%s: %d platforms, want %d", n, got, len(released))
		}
		for plat, want := range released {
			pl, err := platform.Parse(plat)
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.AssetPattern(pkgfile.Values{Version: "0.7.0", Tag: "v0.7.0", Platform: pl})
			want = strings.Replace(want, "photocraft", n, 1)
			if err != nil || got != want {
				t.Errorf("%s %s: asset %q, %v; want %q", n, plat, got, err, want)
			}
			if strings.ContainsAny(got, "*?[") {
				t.Errorf("%s %s: %q is a pattern, not a name", n, plat, got)
			}
			if s, ok, err := p.ChecksumsName(pkgfile.Values{Version: "0.7.0", Platform: pl}, got); err != nil || !ok || s != "SHA256SUMS.txt" {
				t.Errorf("%s: checksums %q %v %v", n, s, ok, err)
			}
		}
	}
}

func TestCraftFileFoldersAndHooks(t *testing.T) {
	root := "/srv/o'neil craft"
	home := "/home/o'neil"
	if runtime.GOOS == "windows" {
		root, home = `C:\Users\O'Neil Smith\CraftWerk`, `C:\Users\O'Neil Smith`
	}
	cfg := loadCraft(t, root)
	p := cfg.Packages["photocraft"]

	// Each OS's folder; only one of them is an absolute path on this machine.
	if runtime.GOOS == "windows" {
		if d, err := p.Dir("windows", home); err != nil || d != root+`\Photocraft` {
			t.Errorf("windows dir = %q, %v", d, err)
		}
	} else if d, err := p.Dir("linux", home); err != nil || d != home+"/.local" {
		t.Errorf("linux dir = %q, %v", d, err)
	}
	if !p.SharedDir("linux") || p.SharedDir("windows") {
		t.Errorf("shared_dir linux %v windows %v, want true false", p.SharedDir("linux"), p.SharedDir("windows"))
	}
	if exe, ok, err := p.VersionFromExe("windows"); err != nil || !ok || exe != "photocraft.exe" {
		t.Errorf("version_from_exe = %q %v %v", exe, ok, err)
	}
	if _, ok, _ := p.VersionFromExe("linux"); ok {
		t.Error("linux has a version_from_exe")
	}

	win := pkgfile.Values{Version: "0.7.0", Tag: "v0.7.0", Platform: platform.Platform{OS: "windows", Arch: "amd64"}}
	register := [][]string{{"powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", root + `\_update\register-apps.ps1`, "-Only", "photocraft"}}
	for _, ev := range []string{pkgfile.PostInstall, pkgfile.PostUpdate, pkgfile.PostRemove} {
		if h, err := p.Hooks(ev, win); err != nil || !reflect.DeepEqual(h, register) {
			t.Errorf("windows %s = %q, %v", ev, h, err)
		}
	}
	if h, _ := p.Hooks(pkgfile.PreRemove, win); h != nil {
		t.Errorf("pre_remove = %q", h)
	}

	lin := win
	lin.Platform = platform.Platform{OS: "linux", Arch: "amd64"}
	for _, ev := range []string{pkgfile.PostInstall, pkgfile.PostUpdate, pkgfile.PostRemove} {
		h, err := p.Hooks(ev, lin)
		if err != nil || len(h) != 1 || len(h[0]) != 4 || h[0][0] != "sh" || h[0][1] != "-c" || h[0][3] != "craft-hook" {
			t.Fatalf("linux %s = %q, %v", ev, h, err)
		}
		script := h[0][2]
		// Never edits the shipped .desktop files (verify would then report them changed).
		if strings.Contains(script, "sed -i") || !strings.Contains(script, "update-desktop-database") {
			t.Errorf("linux %s script:\n%s", ev, script)
		}
		if ev != pkgfile.PostRemove && (!strings.Contains(script, "ldd") || !strings.Contains(script, ":$PATH:")) {
			t.Errorf("linux %s does not check libraries and PATH:\n%s", ev, script)
		}
	}
}
