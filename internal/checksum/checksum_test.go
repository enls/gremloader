package checksum

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/enls/gremloader/internal/testutil/fixtures"
)

const (
	h1 = "b425f34eab8808245726150ae55e5faa6630178ad014f9d48e2230b491f6bd11"
	h2 = "103f6b25ba6096f5d3e3e747e4ede537644a4bee98d5a1a6e64c8ca6f285ac68"
)

func parse(t *testing.T, s string) *File {
	t.Helper()
	f, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func TestRealCraftWerkFixtureBinaryMode(t *testing.T) {
	f, err := Parse(bytes.NewReader(fixtures.Read(t, "craftwerk-v0.2.0/SHA256SUMS.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Warnings) != 0 {
		t.Errorf("warnings: %v", f.Warnings)
	}
	e, err := f.Lookup("CraftWerk-Manager.zip")
	if err != nil {
		t.Fatal(err)
	}
	if e.Hash != h1 || !e.Binary {
		t.Errorf("entry = %+v, want hash %s and Binary", e, h1)
	}
	if _, err := f.Lookup("craftwerk.sh"); err != nil {
		t.Errorf("craftwerk.sh: %v", err)
	}
}

func TestRealPhotocraftFixtureTextMode(t *testing.T) {
	f, err := Parse(bytes.NewReader(fixtures.Read(t, "photocraft-v0.6.0/SHA256SUMS.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Warnings) != 0 {
		t.Errorf("warnings: %v", f.Warnings)
	}
	if len(f.Entries) != 22 {
		t.Errorf("entries = %d, want 22 (23 assets minus the checksum file itself)", len(f.Entries))
	}
	e, err := f.Lookup("photocraft-0.6.0-windows-x64-portable.zip")
	if err != nil {
		t.Fatal(err)
	}
	if e.Binary {
		t.Error("text-mode line must not be marked Binary")
	}
	if len(e.Hash) != 64 {
		t.Errorf("hash = %q", e.Hash)
	}
	// Exact matching: the -cli- style neighbour and prefixes must not match.
	for _, n := range []string{"photocraft-0.6.0-windows-x64-portable", "PHOTOCRAFT-0.6.0-windows-x64-portable.zip", ""} {
		if _, err := f.Lookup(n); !errors.Is(err, ErrNotListed) {
			t.Errorf("Lookup(%q) error = %v, want ErrNotListed", n, err)
		}
	}
}

func TestDialects(t *testing.T) {
	cases := []struct {
		name, in string
		want     []Entry
	}{
		{"text mode", h1 + "  a.zip\n", []Entry{{Hash: h1, Name: "a.zip", Line: 1}}},
		{"binary mode", h1 + " *a.zip\n", []Entry{{Hash: h1, Name: "a.zip", Binary: true, Line: 1}}},
		{"BSD", "SHA256 (a.zip) = " + h1 + "\n", []Entry{{Hash: h1, Name: "a.zip", Line: 1}}},
		{"bare sidecar", h1 + "\n", []Entry{{Hash: h1, Line: 1}}},
		{"CRLF", h1 + "  a.zip\r\n" + h2 + "  b.zip\r\n", []Entry{{Hash: h1, Name: "a.zip", Line: 1}, {Hash: h2, Name: "b.zip", Line: 2}}},
		{"upper-case hash", strings.ToUpper(h1) + "  a.zip\n", []Entry{{Hash: h1, Name: "a.zip", Line: 1}}},
		{"UTF-8 BOM", "\xEF\xBB\xBF" + h1 + "  a.zip\n", []Entry{{Hash: h1, Name: "a.zip", Line: 1}}},
		{"dot-slash prefix", h1 + "  ./a.zip\n", []Entry{{Hash: h1, Name: "a.zip", Line: 1}}},
		{"no final newline", h1 + "  a.zip", []Entry{{Hash: h1, Name: "a.zip", Line: 1}}},
		{"comments and blanks", "# hello\n\n" + h1 + "  a.zip\n\n", []Entry{{Hash: h1, Name: "a.zip", Line: 3}}},
		{"name with spaces", h1 + "  CraftWerk Setup.cmd\n", []Entry{{Hash: h1, Name: "CraftWerk Setup.cmd", Line: 1}}},
		{"tab separated", h1 + "\ta.zip\n", []Entry{{Hash: h1, Name: "a.zip", Line: 1}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := parse(t, c.in)
			if len(f.Warnings) != 0 {
				t.Errorf("warnings: %v", f.Warnings)
			}
			if len(f.Entries) != len(c.want) {
				t.Fatalf("entries = %+v, want %+v", f.Entries, c.want)
			}
			for i := range c.want {
				if f.Entries[i] != c.want[i] {
					t.Errorf("entry %d = %+v, want %+v", i, f.Entries[i], c.want[i])
				}
			}
		})
	}
}

// Windows PowerShell 5.1's Out-File writes UTF-16 LE with a BOM by default.
func TestUTF16WithBOM(t *testing.T) {
	text := h1 + "  a.zip\r\n"
	u := utf16.Encode([]rune(text))
	le := []byte{0xFF, 0xFE}
	be := []byte{0xFE, 0xFF}
	for _, w := range u {
		le = append(le, byte(w), byte(w>>8))
		be = append(be, byte(w>>8), byte(w))
	}
	for name, b := range map[string][]byte{"LE": le, "BE": be} {
		f, err := Parse(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if e, err := f.Lookup("a.zip"); err != nil || e.Hash != h1 {
			t.Errorf("%s: %+v, %v", name, e, err)
		}
	}
}

func TestWarningsDoNotHideGoodLines(t *testing.T) {
	in := strings.Join([]string{
		"d41d8cd98f00b204e9800998ecf8427e  md5.zip", // MD5
		"da39a3ee5e6b4b0d3255bfef95601890afd80709  sha1.zip",
		"not a hash line",
		h1[:63] + "  short.zip",
		strings.Repeat("g", 64) + "  nothex.zip",
		h1,
		h1 + "  ok.zip",
		h2 + " ",
		`\` + h2 + "  escaped.zip",
	}, "\n")
	f := parse(t, in)
	if _, err := f.Lookup("ok.zip"); err != nil {
		t.Errorf("ok.zip: %v", err)
	}
	if len(f.Warnings) != 6 {
		t.Errorf("warnings = %d, want 6:\n%s", len(f.Warnings), strings.Join(f.Warnings, "\n"))
	}
	for _, w := range f.Warnings {
		if !strings.HasPrefix(w, "line ") {
			t.Errorf("warning without a line number: %q", w)
		}
	}
	for _, n := range []string{"md5.zip", "sha1.zip", "short.zip", "nothex.zip", "escaped.zip"} {
		if _, err := f.Lookup(n); !errors.Is(err, ErrNotListed) {
			t.Errorf("%s must not be listed, got %v", n, err)
		}
	}
}

func TestConflictingDuplicateIsAnError(t *testing.T) {
	f := parse(t, h1+"  a.zip\n"+h2+"  a.zip\n")
	if _, err := f.Lookup("a.zip"); !errors.Is(err, ErrConflict) {
		t.Errorf("error = %v, want ErrConflict", err)
	}
	// Identical duplicates are harmless.
	f = parse(t, h1+"  a.zip\n"+h1+" *a.zip\n")
	if e, err := f.Lookup("a.zip"); err != nil || e.Hash != h1 {
		t.Errorf("identical duplicate: %+v, %v", e, err)
	}
}

func TestSingle(t *testing.T) {
	if e, err := parse(t, h1+"\n").Single(); err != nil || e.Hash != h1 {
		t.Errorf("bare: %+v, %v", e, err)
	}
	if _, err := parse(t, "").Single(); !errors.Is(err, ErrNotListed) {
		t.Errorf("empty: %v", err)
	}
	if _, err := parse(t, h1+"  a\n"+h2+"  b\n").Single(); !errors.Is(err, ErrConflict) {
		t.Errorf("two hashes: %v", err)
	}
}

func TestTooLarge(t *testing.T) {
	_, err := Parse(strings.NewReader(strings.Repeat("a", maxFileSize+1)))
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("error = %v, want ErrTooLarge", err)
	}
}

func TestControlCharactersInNameAreRejected(t *testing.T) {
	f := parse(t, h1+"  a\x00b.zip\n")
	if len(f.Entries) != 0 || len(f.Warnings) != 1 {
		t.Errorf("entries=%v warnings=%v", f.Entries, f.Warnings)
	}
}

func TestParseDigest(t *testing.T) {
	good := "sha256:" + strings.ToUpper(h1)
	if got, err := ParseDigest(good); err != nil || got != h1 {
		t.Errorf("ParseDigest(%q) = %q, %v", good, got, err)
	}
	for _, bad := range []string{"", "sha256:", "sha1:" + h1, "sha256:" + h1[:10], h1, "sha256:" + strings.Repeat("z", 64)} {
		if _, err := ParseDigest(bad); err == nil {
			t.Errorf("ParseDigest(%q) succeeded", bad)
		}
	}
}

func TestSHA256AndEqual(t *testing.T) {
	// SHA-256("abc"), FIPS 180-4 test vector.
	const abc = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	got, n, err := SHA256(strings.NewReader("abc"))
	if err != nil || got != abc || n != 3 {
		t.Errorf("SHA256 = %q, %d, %v", got, n, err)
	}
	if !Equal(abc, strings.ToUpper(abc)) {
		t.Error("Equal must ignore case")
	}
	if Equal(abc, abc[:63]+"0") || Equal(abc, "") {
		t.Error("Equal accepted different digests")
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(h1 + "  a.zip\n"))
	f.Add([]byte(h1 + " *a.zip\r\n"))
	f.Add([]byte("SHA256 (a) = " + h1))
	f.Add([]byte("\xEF\xBB\xBF" + h1))
	f.Add([]byte{0xFF, 0xFE, 'a', 0})
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, data []byte) {
		file, err := Parse(bytes.NewReader(data))
		if err != nil {
			return
		}
		for _, e := range file.Entries {
			if len(e.Hash) != 64 || e.Hash != strings.ToLower(e.Hash) {
				t.Fatalf("bad hash in entry %+v", e)
			}
			if e.Line < 1 {
				t.Fatalf("bad line in entry %+v", e)
			}
			// Whatever parsed must be findable by its own name without panicking.
			if _, err := file.Lookup(e.Name); err != nil && !errors.Is(err, ErrConflict) {
				t.Fatalf("Lookup(%q) of a parsed entry failed: %v", e.Name, err)
			}
		}
	})
}
