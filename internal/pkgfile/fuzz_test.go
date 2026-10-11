package pkgfile

import (
	"testing"

	"github.com/enls/gremloader/internal/platform"
)

// FuzzParse: any package file and local file either load or fail with problems; nothing panics,
// and whatever loads can be explained and expanded.
func FuzzParse(f *testing.F) {
	f.Add([]byte(craftFile), []byte(""))
	f.Add([]byte(mergeFile), []byte("schema_version = 1\n[package.a]\npin = \"1\"\n[settings]\nchannel = \"prerelease\"\n"))
	f.Add([]byte("schema_version = 1\n[package.x]\nrepo = \"o/x\"\nasset = \"{{{name}}}\"\ndir = \"/x\"\nverify.require = []\n"), []byte(""))
	f.Fuzz(func(t *testing.T, main, local []byte) {
		if len(local) == 0 {
			local = nil
		}
		c, err := parse("f.toml", main, "l.toml", local, Options{Vars: map[string]string{"root": "/r"}})
		if err != nil {
			if err.Error() == "" {
				t.Fatal("empty error message")
			}
			return
		}
		for _, p := range c.Packages {
			_ = p.Explain()
			for _, os := range platform.OSes {
				for _, arch := range platform.Arches {
					v := Values{Version: "1.0.0", Tag: "v1.0.0", Platform: platform.Platform{OS: os, Arch: arch}}
					_, _ = p.AssetPattern(v)
					_, _, _ = p.ChecksumsName(v, "a.zip")
					_, _ = p.Hooks(PostInstall, v)
				}
				_, _ = p.Dir(os, "/home/x")
				_, _, _ = p.VersionFromExe(os)
			}
			_ = p.Platforms()
		}
	})
}
