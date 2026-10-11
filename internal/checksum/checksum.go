// Package checksum parses the checksum files that releases publish and hashes streams.
//
// Only SHA-256 is accepted. Lines carrying other digest lengths (MD5, SHA-1, SHA-512) are
// reported as warnings and skipped; they are never used as verification.
//
// Real files vary, so the parser reads every dialect seen in the wild (see
// testdata/fixtures): GNU text mode "<hash>  <name>", GNU binary mode "<hash> *<name>", BSD
// "SHA256 (<name>) = <hash>", and a bare "<hash>" in a single-file sidecar. It tolerates CRLF line
// endings, a UTF-8 or UTF-16 byte-order mark (Windows PowerShell writes UTF-16 by default),
// upper-case hashes, "./" prefixes, blank lines and "#" comments. A line it cannot understand is a
// warning, not an error, so one odd line cannot hide the entry you need; but a name listed twice
// with different hashes is an error, because someone is lying.
package checksum

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// Limits protect the parser from a hostile or broken "checksum file".
const (
	maxFileSize = 4 << 20 // 4 MiB
	maxLineSize = 64 << 10
)

var (
	// ErrNotListed means the file parsed but has no entry for the name.
	ErrNotListed = errors.New("not listed in the checksum file")
	// ErrConflict means the name is listed more than once with different hashes.
	ErrConflict = errors.New("listed with conflicting hashes")
	// ErrTooLarge means the file exceeds the size limit.
	ErrTooLarge = errors.New("checksum file is too large")
)

// Entry is one parsed line.
type Entry struct {
	Hash   string // lower-case hex, 64 characters
	Name   string // "" for a bare-hash sidecar line
	Binary bool   // GNU binary-mode marker "*"
	Line   int    // 1-based line number in the decoded file
}

// File is a parsed checksum file.
type File struct {
	Entries  []Entry
	Warnings []string // lines that were skipped, with their line numbers
}

// Parse reads a checksum file.
func Parse(r io.Reader) (*File, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxFileSize {
		return nil, ErrTooLarge
	}
	text := decodeText(raw)

	f := &File{}
	// The text is already in memory (and bounded), so split it rather than scan it: a scanner
	// gives up on the whole file at the first line past its buffer.
	for i, raw := range strings.Split(text, "\n") {
		n := i + 1
		if len(raw) > maxLineSize {
			f.Warnings = append(f.Warnings, fmt.Sprintf("line %d: longer than %d bytes; ignored", n, maxLineSize))
			continue
		}
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		e, warn := parseLine(line)
		if warn != "" {
			f.Warnings = append(f.Warnings, fmt.Sprintf("line %d: %s", n, warn))
			continue
		}
		e.Line = n
		f.Entries = append(f.Entries, e)
	}
	return f, nil
}

// decodeText strips a byte-order mark and converts UTF-16 to UTF-8.
func decodeText(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return string(b[3:])
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		return decodeUTF16(b[2:], false)
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		return decodeUTF16(b[2:], true)
	}
	return string(b)
}

func decodeUTF16(b []byte, bigEndian bool) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	return string(utf16.Decode(u))
}

func parseLine(line string) (Entry, string) {
	// BSD style: SHA256 (name) = hash
	if rest, ok := strings.CutPrefix(line, "SHA256 ("); ok {
		i := strings.LastIndex(rest, ") = ")
		if i < 0 {
			return Entry{}, "malformed BSD-style line"
		}
		return build(rest[i+4:], rest[:i], false)
	}
	if strings.HasPrefix(line, "\\") {
		return Entry{}, "escaped file names are not supported"
	}

	// GNU style: the hash is the first whitespace-delimited field.
	sp := strings.IndexAny(line, " \t")
	if sp < 0 {
		return build(line, "", false) // bare hash
	}
	hash, rest := line[:sp], line[sp:]
	rest = strings.TrimLeft(rest, " \t")
	binary := false
	if strings.HasPrefix(rest, "*") {
		binary = true
		rest = rest[1:]
	}
	if rest == "" {
		return Entry{}, "hash without a file name"
	}
	return build(hash, rest, binary)
}

func build(hash, name string, binary bool) (Entry, string) {
	switch len(hash) {
	case 64:
	case 32, 40, 96, 128:
		return Entry{}, fmt.Sprintf("a %d-character digest is not SHA-256; ignored", len(hash))
	default:
		return Entry{}, "not a SHA-256 hash"
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return Entry{}, "hash is not hexadecimal"
	}
	name = strings.TrimPrefix(name, "./")
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] == 0x7f {
			return Entry{}, "file name contains a control character"
		}
	}
	return Entry{Hash: strings.ToLower(hash), Name: name, Binary: binary}, ""
}

// Lookup returns the entry for name. It reports ErrNotListed when absent and ErrConflict when
// the name appears with two different hashes. Matching is exact and case-sensitive.
func (f *File) Lookup(name string) (Entry, error) {
	var found *Entry
	for i := range f.Entries {
		e := &f.Entries[i]
		if e.Name != name {
			continue
		}
		if found != nil && found.Hash != e.Hash {
			return Entry{}, fmt.Errorf("%q (lines %d and %d): %w", name, found.Line, e.Line, ErrConflict)
		}
		if found == nil {
			found = e
		}
	}
	if found == nil {
		return Entry{}, fmt.Errorf("%q: %w", name, ErrNotListed)
	}
	return *found, nil
}

// Single returns the only entry of a one-file sidecar (named or bare). It is an error if the
// file has no entries or more than one distinct hash.
func (f *File) Single() (Entry, error) {
	if len(f.Entries) == 0 {
		return Entry{}, ErrNotListed
	}
	first := f.Entries[0]
	for _, e := range f.Entries[1:] {
		if e.Hash != first.Hash {
			return Entry{}, fmt.Errorf("expected one hash, found several: %w", ErrConflict)
		}
	}
	return first, nil
}

// Sidecar returns the hash a per-asset checksum file ("app.zip.sha256") gives for asset. An entry
// naming asset is used if there is one. Otherwise the file's single hash is used even when its
// line names another file, with a warning: real sidecars are sometimes generated from a build
// output that was renamed before upload, and the hash still has to match the bytes received.
func (f *File) Sidecar(asset string) (e Entry, warning string, err error) {
	if e, err := f.Lookup(asset); err == nil || errors.Is(err, ErrConflict) {
		return e, "", err
	}
	e, err = f.Single()
	if err != nil {
		return Entry{}, "", err
	}
	if e.Name != "" {
		warning = fmt.Sprintf("the checksum file names %q, not %q; its hash is used anyway", e.Name, asset)
	}
	return e, warning, nil
}

// ParseDigest reads GitHub's asset digest form "sha256:<64 hex>".
func ParseDigest(s string) (string, error) {
	algo, h, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || !strings.EqualFold(algo, "sha256") {
		return "", fmt.Errorf("digest %q: only sha256 is supported", s)
	}
	if len(h) != 64 {
		return "", fmt.Errorf("digest %q: want 64 hex characters", s)
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", fmt.Errorf("digest %q: not hexadecimal", s)
	}
	return strings.ToLower(h), nil
}

// SHA256 hashes everything r yields and returns the lower-case hex digest and the byte count.
func SHA256(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Equal compares two hex digests in constant time, ignoring case.
func Equal(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
