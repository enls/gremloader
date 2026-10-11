// Package state is what GremLoader knows about this machine: which version of each package is
// installed where, how it was verified, what can be rolled back, and the last check's result
// (plan E.2.4). It lives in <state>/state.json; per-version file lists live in manifests.
//
// This package reads, validates and writes those files. It does not decide anything: the engine
// (M5) does, holding the run lock while it writes.
//
// Every path in state.json that the engine will act on (manifest, undo log) is relative to the
// state directory and is checked to stay inside it, so a hand-edited or damaged state.json cannot
// point a rollback at files elsewhere on the disk. The install directory is the exception, and is
// absolute by nature: it comes from the package file, which the user controls anyway.
package state

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/enls/gremloader/internal/jsonfile"
	"github.com/enls/gremloader/internal/paths"
	"github.com/enls/gremloader/internal/ver"
)

// SchemaVersion is the state.json and manifest format this build writes and understands.
const SchemaVersion = 1

// State is state.json.
type State struct {
	SchemaVersion int                 `json:"schema_version"`
	Packages      map[string]*Package `json:"packages"`
}

// Package is one package's record. Version == "" means "not installed": the record then holds
// only what a check learned (LatestSeen, LastCheck, LastError).
type Package struct {
	Version     string     `json:"version,omitempty"`
	Tag         string     `json:"tag,omitempty"`
	Dir         string     `json:"dir,omitempty"`
	Asset       string     `json:"asset,omitempty"`
	AssetSHA256 string     `json:"asset_sha256,omitempty"`
	InstalledAt *time.Time `json:"installed_at,omitempty"`
	Adopted     bool       `json:"adopted,omitempty"`
	SharedDir   bool       `json:"shared_dir,omitempty"` // installed into a folder other packages share (shared_dir)
	VerifiedBy  []string   `json:"verified_by,omitempty"`
	Attested    bool       `json:"attested,omitempty"`
	Manifest    string     `json:"manifest,omitempty"` // relative to the state dir, "/"-separated
	Previous    *Previous  `json:"previous,omitempty"`
	Rejected    []string   `json:"rejected,omitempty"`
	LatestSeen  *Seen      `json:"latest_seen,omitempty"`
	LastCheck   *time.Time `json:"last_check,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

// Previous is the rollback record: the version an update replaced and the undo log that restores
// it. Manifest is empty when that version's file list was unknown (it was adopted).
type Previous struct {
	Version  string `json:"version"`
	Undo     string `json:"undo"`
	Manifest string `json:"manifest,omitempty"`
}

// Seen is the newest version the last check found, so `list` needs no network.
type Seen struct {
	Version     string     `json:"version"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	At          time.Time  `json:"at"`
}

// New returns an empty state.
func New() *State {
	return &State{SchemaVersion: SchemaVersion, Packages: map[string]*Package{}}
}

// Load reads path. A missing file is an empty state (nothing installed yet); a damaged one is an
// error, never silently empty: "nothing is installed" would make the next run treat every install
// directory as someone else's.
func Load(path string) (*State, error) {
	s := New()
	found, err := jsonfile.Read(path, SchemaVersion, s)
	if err != nil {
		return nil, err
	}
	if !found {
		return s, nil
	}
	if s.Packages == nil {
		s.Packages = map[string]*Package{}
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w: %w", path, jsonfile.ErrCorrupt, err)
	}
	return s, nil
}

// Save validates s and replaces path with it atomically.
func Save(path string, s *State) error {
	if err := s.Validate(); err != nil {
		return fmt.Errorf("refusing to write %s: %w", path, err)
	}
	s.SchemaVersion = SchemaVersion
	return jsonfile.Write(path, s)
}

// Path turns a stored relative path (Manifest, Previous.Undo ...) into a path under stateDir.
func Path(stateDir, rel string) string {
	return filepath.Join(stateDir, filepath.FromSlash(rel))
}

var (
	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
	levelName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// Validate checks every field that later code will trust.
func (s *State) Validate() error {
	var errs []error
	for name, p := range s.Packages {
		if !paths.ValidPackageName(name) {
			errs = append(errs, fmt.Errorf("package name %q is not valid", name))
			continue
		}
		if p == nil {
			errs = append(errs, fmt.Errorf("package %s: empty record", name))
			continue
		}
		if err := p.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("package %s: %w", name, err))
		}
	}
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errors.Join(errs...)
}

// Validate checks one package record by the rules Save applies to all of them.
func (p *Package) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if p.Version == "" {
		if p.Tag != "" || p.Dir != "" || p.Asset != "" || p.AssetSHA256 != "" || p.InstalledAt != nil ||
			p.Adopted || p.SharedDir || len(p.VerifiedBy) > 0 || p.Attested || p.Manifest != "" || p.Previous != nil {
			bad("has installation details but no version")
		}
	} else {
		if _, err := ver.Parse(p.Version); err != nil {
			bad("version: %v", err)
		}
		if p.Dir == "" || !filepath.IsAbs(p.Dir) {
			bad("dir %q is not an absolute path", p.Dir)
		}
		if p.InstalledAt == nil {
			bad("installed_at is missing")
		}
	}
	if p.Tag != "" && !ver.ValidTag(p.Tag) {
		bad("tag %q is not valid", p.Tag)
	}
	if p.Asset != "" && !validFileName(p.Asset) {
		bad("asset %q is not a file name", p.Asset)
	}
	if p.AssetSHA256 != "" && !sha256Hex.MatchString(p.AssetSHA256) {
		bad("asset_sha256 is not 64 lower-case hex digits")
	}
	for _, l := range p.VerifiedBy {
		if !levelName.MatchString(l) {
			bad("verified_by %q is not a level name", l)
		}
	}
	if p.Manifest != "" && !validRel(p.Manifest) {
		bad("manifest %q must be a relative path inside the state directory", p.Manifest)
	}
	if pr := p.Previous; pr != nil {
		if _, err := ver.Parse(pr.Version); err != nil {
			bad("previous version: %v", err)
		}
		if !validRel(pr.Undo) {
			bad("previous undo %q must be a relative path inside the state directory", pr.Undo)
		}
		if pr.Manifest != "" && !validRel(pr.Manifest) {
			bad("previous manifest %q must be a relative path inside the state directory", pr.Manifest)
		}
	}
	for _, r := range p.Rejected {
		if _, err := ver.Parse(r); err != nil {
			bad("rejected: %v", err)
		}
	}
	if ls := p.LatestSeen; ls != nil {
		if _, err := ver.Parse(ls.Version); err != nil {
			bad("latest_seen: %v", err)
		}
	}
	if len(p.LastError) > 4096 {
		bad("last_error is longer than 4096 bytes")
	}
	return errors.Join(errs...)
}

func validRel(rel string) bool { return paths.ValidRel(rel) }

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
