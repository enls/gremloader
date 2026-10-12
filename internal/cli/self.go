package cli

import (
	_ "embed"

	"github.com/enls/gremloader/internal/pkgfile"
)

// selfName is the package name of GremLoader itself (self-update, ADR 0015).
const selfName = "gremloader"

//go:embed self.toml
var selfFile []byte

// selfConfig is the built-in package file of self-update: GremLoader installed in dir, the folder
// of the running binary, from channel ("stable" or "prerelease").
func selfConfig(dir, channel string) (*pkgfile.Config, error) {
	cfg, err := pkgfile.Parse("self.toml (built in)", selfFile, pkgfile.Options{Vars: map[string]string{"root": dir}})
	if err != nil {
		return nil, err
	}
	cfg.Packages[selfName].Channel = channel
	return cfg, nil
}
