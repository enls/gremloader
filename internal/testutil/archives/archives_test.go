package archives

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"slices"
	"strings"
	"testing"
)

func zipNames(t *testing.T, b []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range zr.File {
		out = append(out, f.Name)
	}
	return out
}

func tarEntries(t *testing.T, b []byte) []*tar.Header {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var out []*tar.Header
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
}

func TestDeterministic(t *testing.T) {
	a := CraftWindowsZip("soundcraft", "0.4.0")
	b := CraftWindowsZip("soundcraft", "0.4.0")
	if !bytes.Equal(a, b) {
		t.Error("zip is not deterministic")
	}
	if !bytes.Equal(CraftLinuxTarGz("soundcraft", "0.4.0", "x86_64"), CraftLinuxTarGz("soundcraft", "0.4.0", "x86_64")) {
		t.Error("tar.gz is not deterministic")
	}
}

// The synthetic Windows zip must have the shape that was observed on the real one.
func TestCraftWindowsZipShape(t *testing.T) {
	names := zipNames(t, CraftWindowsZip("soundcraft", "0.4.0"))
	if len(names) != 7 {
		t.Fatalf("entries = %d, want 7: %v", len(names), names)
	}
	top := "soundcraft-0.4.0-windows-x64-portable/"
	for _, n := range names {
		if !strings.HasPrefix(n, top) || strings.HasSuffix(n, "/") || strings.ContainsAny(n, `\:`) {
			t.Errorf("unexpected entry %q", n)
		}
	}
	for _, want := range []string{"soundcraft.exe", "soundcraft-cli.exe", "README.md", "NOTICE"} {
		if !slices.Contains(names, top+want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestCraftLinuxTarGzShape(t *testing.T) {
	hs := tarEntries(t, CraftLinuxTarGz("soundcraft", "0.4.0", "x86_64"))
	top := "soundcraft-0.4.0-linux-x86_64/"
	var exec, dirs int
	for _, h := range hs {
		if !strings.HasPrefix(h.Name, top) {
			t.Errorf("entry outside the top folder: %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeSymlink, tar.TypeLink:
			t.Errorf("real Craft tarballs have no links, got %q", h.Name)
		case tar.TypeDir:
			dirs++
		case tar.TypeReg:
			if h.Mode&0o111 != 0 {
				exec++
			}
		}
	}
	if exec != 2 || dirs == 0 {
		t.Errorf("exec=%d dirs=%d", exec, dirs)
	}
}

func TestHostileNamesAreWritable(t *testing.T) {
	names := []string{"../evil", `..\evil`, "/abs/path", `C:\evil.txt`, "a/../../b", "nul.txt", "a:stream", "dup", "dup"}
	var es []Entry
	for _, n := range names {
		es = append(es, F(n, "x"))
	}
	if got := zipNames(t, Zip(es...)); !slices.Equal(got, names) {
		t.Errorf("zip names = %q, want %q", got, names)
	}
	var got []string
	for _, h := range tarEntries(t, TarGz(es...)) {
		got = append(got, h.Name)
	}
	if !slices.Equal(got, names) {
		t.Errorf("tar names = %q, want %q", got, names)
	}
}

func TestLinksAndModes(t *testing.T) {
	hs := tarEntries(t, TarGz(L("l", "../target"), H("h", "a"), X("x", "1"), F("f", "2"), D("d")))
	want := map[string]struct {
		flag byte
		link string
		mode int64
	}{
		"l":  {tar.TypeSymlink, "../target", 0o644},
		"h":  {tar.TypeLink, "a", 0o644},
		"x":  {tar.TypeReg, "", 0o755},
		"f":  {tar.TypeReg, "", 0o644},
		"d/": {tar.TypeDir, "", 0o755},
	}
	for _, h := range hs {
		w, ok := want[h.Name]
		if !ok {
			t.Errorf("unexpected %q", h.Name)
			continue
		}
		if h.Typeflag != w.flag || h.Linkname != w.link || h.Mode != w.mode {
			t.Errorf("%s: flag=%c link=%q mode=%o; want flag=%c link=%q mode=%o", h.Name, h.Typeflag, h.Linkname, h.Mode, w.flag, w.link, w.mode)
		}
	}

	zr, err := zip.NewReader(bytes.NewReader(Zip(L("l", "../t"), X("x", "1"))), int64(len(Zip(L("l", "../t"), X("x", "1")))))
	if err != nil {
		t.Fatal(err)
	}
	if zr.File[0].Mode()&fs.ModeSymlink == 0 {
		t.Error("zip symlink lost its symlink bit")
	}
	if zr.File[1].Mode().Perm() != 0o755 {
		t.Errorf("zip exec mode = %v", zr.File[1].Mode())
	}
}
