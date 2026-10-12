package fixtures

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sort"
	"strings"
	"testing"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// The captured zip must still match the hash the release published for it; if it does not, the
// fixture was altered (or line-ending normalized) and every test built on it is suspect.
func TestCraftWerkZipMatchesPublishedChecksum(t *testing.T) {
	sums := string(Read(t, "craftwerk-v0.2.0/SHA256SUMS.txt"))
	zipBytes := Read(t, "craftwerk-v0.2.0/CraftWerk-Manager.zip")

	var want string
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == "CraftWerk-Manager.zip" {
			want = fields[0]
		}
	}
	if want == "" {
		t.Fatalf("no CraftWerk-Manager.zip entry in SHA256SUMS.txt:\n%s", sums)
	}
	if got := sha256Hex(zipBytes); got != want {
		t.Errorf("CraftWerk-Manager.zip sha256 = %s, published %s", got, want)
	}
}

func TestPhotocraftChecksumFileShape(t *testing.T) {
	b := Read(t, "photocraft-v0.6.0/SHA256SUMS.txt")
	if bytes.Contains(b, []byte("\r")) {
		t.Error("fixture contains CR: line endings were altered")
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) < 10 {
		t.Fatalf("only %d lines", len(lines))
	}
	for _, l := range lines {
		// text-mode format: 64 hex chars, two spaces, name
		if len(l) < 67 || l[64:66] != "  " {
			t.Errorf("unexpected line shape: %q", l)
		}
	}
}

func zipNames(t *testing.T, b []byte) []string {
	t.Helper()
	r, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	return names
}

func tarFiles(t *testing.T, b []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		if h.Typeflag == tar.TypeReg {
			names = append(names, h.Name)
		}
	}
	sort.Strings(names)
	return names
}

// The derived tarball must hold exactly the files of the real zip.
func TestDerivedTarballMatchesZip(t *testing.T) {
	z := zipNames(t, Read(t, "craftwerk-v0.2.0/CraftWerk-Manager.zip"))
	g := tarFiles(t, Read(t, "craftwerk-v0.2.0/CraftWerk-Manager.tar.gz"))
	if strings.Join(z, "\n") != strings.Join(g, "\n") {
		t.Errorf("zip and tarball differ\nzip: %q\ntar: %q", z, g)
	}
	if len(z) != 8 {
		t.Errorf("expected 8 files, got %d", len(z))
	}
}
