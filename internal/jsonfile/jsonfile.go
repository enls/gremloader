// Package jsonfile reads and writes GremLoader's JSON files (state.json, manifests,
// gremloader.lock) with one set of rules:
//
//   - every file carries "schema_version"; a file from a newer GremLoader is refused, never
//     half-understood, and that check comes before anything else so the message says so rather
//     than complaining about the first new field;
//   - unknown fields are an error (an older binary that dropped them on rewrite would lose data);
//   - one JSON value per file, nothing after it; at most MaxBytes;
//   - writes are atomic (internal/atomicfile), indented, with a final newline, so the committed
//     lock file diffs cleanly.
package jsonfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/enls/gremloader/internal/atomicfile"
)

// MaxBytes caps a file that is read. The largest real one, a manifest of 20,000 files, is about
// 3 MiB.
const MaxBytes = 32 << 20

var (
	// ErrNewerSchema: the file was written by a newer GremLoader.
	ErrNewerSchema = errors.New("written by a newer version of GremLoader")
	// ErrCorrupt: the file is not valid for this version (syntax, unknown field, wrong type,
	// missing schema_version, too large).
	ErrCorrupt = errors.New("damaged or not a GremLoader file")
)

// Read decodes path into v, which must have a SchemaVersion-style field tagged
// `json:"schema_version"`. It reports found=false (and no error) when the file does not exist.
func Read(path string, current int, v any) (found bool, err error) {
	f, err := os.Open(path) //nolint:gosec // G304: GremLoader's own files, paths chosen by the caller
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return true, fmt.Errorf("%s: %w", path, err)
	}
	if len(data) > MaxBytes {
		return true, fmt.Errorf("%s: %w: larger than %d MiB", path, ErrCorrupt, MaxBytes>>20)
	}
	return true, Decode(path, data, current, v)
}

// Decode applies Read's rules to data; name is used in messages.
func Decode(name string, data []byte, current int, v any) error {
	var probe struct {
		SchemaVersion *json.RawMessage `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("%s: %w: %w", name, ErrCorrupt, err)
	}
	if probe.SchemaVersion == nil {
		return fmt.Errorf("%s: %w: no schema_version", name, ErrCorrupt)
	}
	var n int
	if err := json.Unmarshal(*probe.SchemaVersion, &n); err != nil || n < 1 {
		return fmt.Errorf("%s: %w: schema_version %s is not a positive whole number", name, ErrCorrupt, *probe.SchemaVersion)
	}
	if n > current {
		return fmt.Errorf("%s: %w (schema_version %d; this one understands %d): update GremLoader", name, ErrNewerSchema, n, current)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w: %w", name, ErrCorrupt, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w: data after the end of the JSON value", name, ErrCorrupt)
	}
	return nil
}

// Write encodes v and replaces path with it, creating the directory if needed.
func Write(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // G301/G703: GremLoader's own directories
		return err
	}
	return atomicfile.Write(path, append(data, '\n'), 0o600)
}
