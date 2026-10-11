package pkgfile

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"

	"github.com/enls/gremloader/internal/atomicfile"
	"github.com/enls/gremloader/internal/paths"
)

// Local is gremloader.local.toml. Its shape is the whitelist: strict decoding rejects any key
// that is not a field here, with its line, so a hook, URL or path can never be overridden
// locally.
type Local struct {
	SchemaVersion int64                    `toml:"schema_version"`
	Settings      *LocalSettings           `toml:"settings,omitempty"`
	Package       map[string]*LocalPackage `toml:"package,omitempty"`
}

// LocalSettings are the machine-wide overrides.
type LocalSettings struct {
	Channel      *string `toml:"channel,omitempty"`
	CooldownDays *int64  `toml:"cooldown_days,omitempty"`
}

// LocalPackage are one package's overrides.
type LocalPackage struct {
	Pin          *string `toml:"pin,omitempty"`
	Hold         *bool   `toml:"hold,omitempty"`
	Channel      *string `toml:"channel,omitempty"`
	CooldownDays *int64  `toml:"cooldown_days,omitempty"`
	Enabled      *bool   `toml:"enabled,omitempty"`
}

func (s *LocalSettings) raw() *rawLayer {
	if s == nil {
		return nil
	}
	return &rawLayer{Channel: s.Channel, CooldownDays: s.CooldownDays}
}

func (p *LocalPackage) raw() *rawLayer {
	if p == nil {
		return nil
	}
	return &rawLayer{Pin: p.Pin, Hold: p.Hold, Channel: p.Channel, CooldownDays: p.CooldownDays, Enabled: p.Enabled}
}

const localHeader = "# Written by GremLoader (pin, hold, unpin, unhold, ...). Machine-local: do not commit.\n" +
	"# Settings you write by hand belong in the package file next to it.\n\n"

// LoadLocal reads the local override file that belongs to packageFile. A missing file is an
// empty one.
func LoadLocal(packageFile string) (*Local, error) {
	path := paths.LocalOverride(packageFile)
	data, err := readCapped(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Local{SchemaVersion: SchemaVersion}, nil
	}
	if err != nil {
		return nil, err
	}
	var l Local
	if ps := decode(path, data, &l, SchemaVersion, notAllowedLocally); ps != nil {
		return nil, &Error{Problems: ps}
	}
	if err := l.validate(path); err != nil {
		return nil, err
	}
	return &l, nil
}

// SaveLocal validates l and writes it beside packageFile, atomically. Comments in an existing
// file are not kept (the file is machine-written; ADR 0004).
func SaveLocal(packageFile string, l *Local) error {
	path := paths.LocalOverride(packageFile)
	l.SchemaVersion = SchemaVersion
	if err := l.validate(path); err != nil {
		return fmt.Errorf("refusing to write %s: %w", path, err)
	}
	var buf bytes.Buffer
	buf.WriteString(localHeader)
	enc := toml.NewEncoder(&buf)
	enc.SetIndentTables(false)
	if err := enc.Encode(l); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return atomicfile.Write(path, buf.Bytes(), 0o644)
}

// validate applies the package file's value rules to the override values.
func (l *Local) validate(path string) error {
	b := &builder{userVars: map[string]bool{}}
	s := &src{b: b, name: filepath.Base(path), pos: positions{}}
	s.layer(l.Settings.raw(), []string{"settings"}, "[settings]", kindSettings)
	for n, p := range l.Package {
		if !paths.ValidPackageName(n) {
			s.problem([]string{"package", n}, "package name %q is not valid", n)
			continue
		}
		s.layer(p.raw(), []string{"package", n}, "[package."+n+"]", kindPackage)
	}
	if len(b.problems) > 0 {
		return &Error{Problems: b.problems}
	}
	return nil
}
