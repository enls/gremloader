package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/pkgfile"
	"github.com/enls/gremloader/internal/platform"
)

// The built-in package names exactly the archives the release script builds, and requires our own
// release workflow's attestation.
func TestSelfPackage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	cfg, err := selfConfig(dir, "prerelease")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("warnings: %v", cfg.Warnings)
	}
	p := cfg.Packages[selfName]
	if p == nil || len(cfg.Packages) != 1 {
		t.Fatalf("packages: %v", cfg.Names())
	}
	if p.Repo != "enls/gremloader" || p.Channel != "prerelease" || !p.Strip.Auto {
		t.Errorf("repo %q, channel %q, strip %+v", p.Repo, p.Channel, p.Strip)
	}
	if !slices.Equal(p.VerifyRequire, []string{"checksum", "attestation"}) || p.SignerWorkflow != ".github/workflows/release.yml" {
		t.Errorf("verify: require %v, signer_workflow %q", p.VerifyRequire, p.SignerWorkflow)
	}

	// scripts/build-release.sh: gremloader-<version>-<goos>-<goarch>, .zip for windows, else .tar.gz.
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "build-release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`name="gremloader-${version}-${goos}-${goarch}"`, `out="$outdir/$name.zip"`, `out="$outdir/$name.tar.gz"`} {
		if !strings.Contains(string(script), want) {
			t.Errorf("build-release.sh no longer has %s: update self.toml and this test", want)
		}
	}
	for _, pl := range []string{"windows/amd64", "windows/arm64", "linux/amd64", "linux/arm64"} {
		plat, err := platform.Parse(pl)
		if err != nil {
			t.Fatal(err)
		}
		ext := ".tar.gz"
		if plat.OS == "windows" {
			ext = ".zip"
		}
		v := pkgfile.Values{Version: "0.0.2-rc.1", Tag: "v0.0.2-rc.1", Platform: plat}
		if got, err := p.AssetPattern(v); err != nil || got != "gremloader-0.0.2-rc.1-"+plat.OS+"-"+plat.Arch+ext {
			t.Errorf("%s: asset %q, %v", pl, got, err)
		}
		if got, ok, err := p.ChecksumsName(v, "x"); err != nil || !ok || got != "SHA256SUMS.txt" {
			t.Errorf("%s: checksums %q %v %v", pl, got, ok, err)
		}
		if got, err := p.Dir(plat.OS, ""); err != nil || got != dir {
			t.Errorf("%s: dir %q, %v", pl, got, err)
		}
		if !p.SharedDir(plat.OS) {
			t.Errorf("%s: not a shared folder", pl)
		}
	}
}
