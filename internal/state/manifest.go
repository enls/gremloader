package state

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/enls/gremloader/internal/jsonfile"
)

// Manifest lists what one installed version put on disk: every file with its size and SHA-256,
// and the directories GremLoader created (so remove can delete them again once they are empty).
// Paths are relative to the install directory and "/"-separated.
type Manifest struct {
	SchemaVersion int            `json:"schema_version"`
	Files         []ManifestFile `json:"files"`
	Dirs          []string       `json:"dirs"`
}

// ManifestFile is one installed file.
type ManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Exec   bool   `json:"exec,omitempty"`
}

// ManifestRel is where a version's manifest is stored, relative to the state directory.
// pkg must already be a valid package name and version a parsed version.
func ManifestRel(pkg, version string) string {
	return "manifests/" + pkg + "/" + version + ".json"
}

// LoadManifest reads one manifest. Unlike state.json, a missing manifest is an error: whoever
// asks for one found its name in state.json.
func LoadManifest(path string) (*Manifest, error) {
	var m Manifest
	found, err := jsonfile.Read(path, SchemaVersion, &m)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("manifest %s is missing", path)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w: %w", path, jsonfile.ErrCorrupt, err)
	}
	return &m, nil
}

// SaveManifest validates m, sorts it, and writes it atomically.
func SaveManifest(path string, m *Manifest) error {
	if err := m.Validate(); err != nil {
		return fmt.Errorf("refusing to write %s: %w", path, err)
	}
	m.SchemaVersion = SchemaVersion
	slices.SortFunc(m.Files, func(a, b ManifestFile) int { return cmp.Compare(a.Path, b.Path) })
	slices.Sort(m.Dirs)
	if m.Files == nil {
		m.Files = []ManifestFile{}
	}
	if m.Dirs == nil {
		m.Dirs = []string{}
	}
	return jsonfile.Write(path, m)
}

// Validate checks that every path is a clean relative path, nothing is listed twice, and no
// path is both a file and a directory.
func (m *Manifest) Validate() error {
	var errs []error
	seen := map[string]string{}
	add := func(p, kind string) {
		if !validRel(p) {
			errs = append(errs, fmt.Errorf("%s %q is not a clean relative path", kind, p))
			return
		}
		if prev, dup := seen[p]; dup {
			errs = append(errs, fmt.Errorf("%q is listed twice (as %s and %s)", p, prev, kind))
			return
		}
		seen[p] = kind
	}
	for _, f := range m.Files {
		add(f.Path, "file")
		if f.Size < 0 {
			errs = append(errs, fmt.Errorf("file %q: negative size", f.Path))
		}
		if !sha256Hex.MatchString(f.SHA256) {
			errs = append(errs, fmt.Errorf("file %q: sha256 is not 64 lower-case hex digits", f.Path))
		}
	}
	for _, d := range m.Dirs {
		add(d, "dir")
	}
	// A file may not sit where a listed path needs a directory.
	for p := range seen {
		for i := strings.IndexByte(p, '/'); i >= 0; i = nextSlash(p, i) {
			if seen[p[:i]] == "file" {
				errs = append(errs, fmt.Errorf("%q is a file but %q is inside it", p[:i], p))
			}
		}
	}
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errors.Join(errs...)
}

func nextSlash(s string, i int) int {
	j := strings.IndexByte(s[i+1:], '/')
	if j < 0 {
		return -1
	}
	return i + 1 + j
}
