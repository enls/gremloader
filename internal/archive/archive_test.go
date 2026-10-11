package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/enls/gremloader/internal/testutil/archives"
)

// ---- helpers ----

type kind int

const (
	zipFmt kind = iota
	tarFmt
	tgzFmt
)

func (k kind) String() string { return [...]string{"zip", "tar", "tar.gz"}[k] }

// build makes the same entries in the given format.
func build(k kind, es ...archives.Entry) []byte {
	switch k {
	case zipFmt:
		return archives.Zip(es...)
	case tarFmt:
		return archives.Tar(es...)
	}
	return archives.TarGz(es...)
}

// run extracts data into a fresh destination inside a fresh parent. It returns the result, the
// destination, the parent (to check that nothing escaped) and the error.
func run(t *testing.T, data []byte, opt Options) (*Result, string, string, error) {
	t.Helper()
	parent := t.TempDir()
	src := filepath.Join(parent, "in.bin")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(parent, "out", "tree")
	res, err := Extract(context.Background(), src, dst, opt)
	return res, dst, parent, err
}

func mustExtract(t *testing.T, data []byte, opt Options) (*Result, string) {
	t.Helper()
	res, dst, _, err := run(t, data, opt)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return res, dst
}

// mustReject checks that the archive was refused for the rule, naming the entry, and that the
// destination was left empty and nothing appeared beside it.
func mustReject(t *testing.T, data []byte, opt Options, rule Rule, entry string) {
	t.Helper()
	// Only these are discovered while reading an entry's data; every other rule is decided from the
	// headers, before a file is created.
	switch rule {
	case RuleCompressionKind, RuleCorrupt, RuleSizeMismatch:
	default:
		if opt.create == nil {
			opt.create = neverCreate(t)
		}
	}
	res, dst, parent, err := run(t, data, opt)
	if err == nil {
		t.Fatalf("accepted; want rule %s (files %v)", rule, res.Files)
	}
	var re *RejectError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v (%T), want *RejectError", err, err)
	}
	if re.Rule != rule {
		t.Errorf("rule = %s (%v), want %s", re.Rule, err, rule)
	}
	if entry != "" && re.Entry != entry {
		t.Errorf("entry = %q, want %q", re.Entry, entry)
	}
	assertEmpty(t, dst)
	assertNothingOutside(t, parent)
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		t.Errorf("left behind in %s: %s", dir, e.Name())
	}
}

// assertNothingOutside checks the parent holds only what run put there.
func assertNothingOutside(t *testing.T, parent string) {
	t.Helper()
	ents, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != "in.bin" && e.Name() != "out" {
			t.Errorf("something appeared next to the destination: %s", e.Name())
		}
	}
	if ents, err := os.ReadDir(filepath.Join(parent, "out")); err == nil {
		for _, e := range ents {
			if e.Name() != "tree" {
				t.Errorf("something appeared next to the destination: out/%s", e.Name())
			}
		}
	}
}

func paths(res *Result) []string {
	var out []string
	for _, f := range res.Files {
		out = append(out, f.Path)
	}
	return out
}

func readFile(t *testing.T, dst, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- real shapes ----

func TestRealWindowsZipShape(t *testing.T) {
	data := archives.CraftWindowsZip("soundcraft", "0.4.0")
	res, dst := mustExtract(t, data, Options{Strip: StripAuto})

	if res.Format != FormatZip || res.Stripped != "soundcraft-0.4.0-windows-x64-portable" {
		t.Errorf("format %v, stripped %q", res.Format, res.Stripped)
	}
	want := []string{"ATTRIBUTION.md", "LICENSE-APACHE", "LICENSE-MIT", "NOTICE", "README.md", "soundcraft-cli.exe", "soundcraft.exe"}
	if got := paths(res); !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if len(res.Dirs) != 0 {
		t.Errorf("dirs = %v, want none (the real zip has no directories)", res.Dirs)
	}
	var total int64
	for _, f := range res.Files {
		b := readFile(t, dst, f.Path)
		sum := sha256.Sum256([]byte(b))
		if f.SHA256 != hex.EncodeToString(sum[:]) || f.Size != int64(len(b)) {
			t.Errorf("%s: recorded %s/%d, on disk %x/%d", f.Path, f.SHA256, f.Size, sum, len(b))
		}
		if f.Mode != 0o644 {
			t.Errorf("%s: mode %v, want 0644", f.Path, f.Mode)
		}
		total += f.Size
	}
	if res.Bytes != total {
		t.Errorf("Bytes = %d, want %d", res.Bytes, total)
	}
}

func TestRealLinuxTarballShape(t *testing.T) {
	for _, k := range []kind{tgzFmt} {
		t.Run(k.String(), func(t *testing.T) {
			data := archives.CraftLinuxTarGz("soundcraft", "0.4.0", "x64")
			res, dst := mustExtract(t, data, Options{Strip: StripAuto})

			if res.Format != FormatTarGz || res.Stripped != "soundcraft-0.4.0-linux-x64" {
				t.Errorf("format %v, stripped %q", res.Format, res.Stripped)
			}
			wantDirs := []string{
				"bin", "share", "share/applications", "share/doc", "share/doc/soundcraft",
				"share/icons", "share/icons/hicolor", "share/icons/hicolor/64x64", "share/icons/hicolor/64x64/apps",
			}
			if !slices.Equal(res.Dirs, wantDirs) {
				t.Errorf("dirs = %v\nwant   %v", res.Dirs, wantDirs)
			}
			modes := map[string]fs.FileMode{}
			for _, f := range res.Files {
				modes[f.Path] = f.Mode
			}
			if modes["bin/soundcraft"] != 0o755 || modes["bin/soundcraft-cli"] != 0o755 {
				t.Errorf("executables lost their mode: %v", modes)
			}
			if modes["share/doc/soundcraft/NOTICE"] != 0o644 {
				t.Errorf("a document is %v, want 0644", modes["share/doc/soundcraft/NOTICE"])
			}
			if len(res.Files) != 8 {
				t.Errorf("%d files: %v", len(res.Files), paths(res))
			}
			if got := readFile(t, dst, "share/doc/soundcraft/NOTICE"); got != "notice\n" {
				t.Errorf("NOTICE = %q", got)
			}
			if st, err := os.Stat(filepath.Join(dst, "share", "icons", "hicolor", "64x64", "apps")); err != nil || !st.IsDir() {
				t.Errorf("the deepest directory is missing: %v", err)
			}
		})
	}
}

// ---- strip ----

func TestStrip(t *testing.T) {
	es := func(names ...string) []archives.Entry {
		var out []archives.Entry
		for _, n := range names {
			out = append(out, archives.F(n, "x"))
		}
		return out
	}
	cases := []struct {
		name     string
		strip    Strip
		entries  []archives.Entry
		want     []string
		stripped string
	}{
		{"auto: one folder", StripAuto, es("top/a", "top/b/c"), []string{"a", "b/c"}, "top"},
		{"auto: folder entry too", StripAuto, append([]archives.Entry{archives.D("top")}, es("top/a")...), []string{"a"}, "top"},
		{"auto: two folders", StripAuto, es("one/a", "two/b"), []string{"one/a", "two/b"}, ""},
		{"auto: loose file beside the folder", StripAuto, es("README", "top/a"), []string{"README", "top/a"}, ""},
		{"auto: only loose files", StripAuto, es("a", "b"), []string{"a", "b"}, ""},
		{"auto: a single loose file", StripAuto, es("a"), []string{"a"}, ""},
		{"none", Strip{}, es("top/a"), []string{"top/a"}, ""},
		{"N=1", Strip{N: 1}, es("one/a", "two/b"), []string{"a", "b"}, "one"},
		{"N=2", Strip{N: 2}, es("t/u/a", "t/u/v/b"), []string{"a", "v/b"}, "t/u"},
	}
	for _, c := range cases {
		for _, k := range []kind{zipFmt, tgzFmt} {
			t.Run(c.name+"/"+k.String(), func(t *testing.T) {
				res, _ := mustExtract(t, build(k, c.entries...), Options{Strip: c.strip})
				got := paths(res)
				slices.Sort(got)
				want := slices.Clone(c.want)
				slices.Sort(want)
				if !slices.Equal(got, want) {
					t.Errorf("files = %v, want %v", got, want)
				}
				if c.stripped != "" && res.Stripped != c.stripped {
					t.Errorf("Stripped = %q, want %q", res.Stripped, c.stripped)
				}
			})
		}
	}
}

func TestStripThatLeavesNoNameOrCollidesIsRejected(t *testing.T) {
	// A file with nothing left after stripping.
	mustReject(t, build(zipFmt, archives.F("a", "x")), Options{Strip: Strip{N: 1}}, RuleNothingAfterStrip, "a")
	// Two different folders stripped down to the same name.
	mustReject(t, build(zipFmt, archives.F("one/x", "1"), archives.F("two/x", "2")), Options{Strip: Strip{N: 1}}, RuleDuplicate, "two/x")
}

// ---- the malicious corpus ----

func TestRejectedNames(t *testing.T) {
	cases := []struct {
		name  string
		entry string
		rule  Rule
	}{
		{"zip slip", "../evil.txt", RuleDotDot},
		{"zip slip, nested", "a/../../evil.txt", RuleDotDot},
		{"zip slip, trailing", "a/..", RuleDotDot},
		{"absolute", "/etc/cron.d/evil", RuleAbsolute},
		{"drive letter, slash", "C:/evil.txt", RuleColon},
		{"drive letter, backslash", `C:\evil.txt`, RuleBackslash},
		{"backslash", `a\b.txt`, RuleBackslash},
		{"backslash traversal", `..\..\evil.txt`, RuleBackslash},
		{"alternate data stream", "readme.txt:evil", RuleColon},
		{"reserved name", "NUL", RuleReservedName},
		{"reserved name with extension", "NUL.txt", RuleReservedName},
		{"reserved name, lower case, in a folder", "dir/aux.log", RuleReservedName},
		{"reserved name, com port", "dir/Com1.dat", RuleReservedName},
		{"reserved name, space before the dot", "con .txt", RuleReservedName},
		{"trailing dot", "readme.", RuleTrailingDotSpace},
		{"trailing space", "readme ", RuleTrailingDotSpace},
		{"folder with a trailing dot", "dir./a.txt", RuleTrailingDotSpace},
		{"control character", "a\x01b.txt", RuleControlChar},
		{"newline", "a\nb.txt", RuleControlChar},
		{"escape sequence", "\x1b[2Jevil", RuleControlChar},
		{"windows-invalid character", "a<b.txt", RuleWindowsChar},
		{"question mark", "what?.txt", RuleWindowsChar},
		{"empty component", "a//b.txt", RuleEmptyComponent},
		{"invalid UTF-8", "bad\xff.txt", RuleInvalidUTF8},
	}
	for _, c := range cases {
		for _, k := range []kind{zipFmt, tgzFmt} {
			t.Run(c.name+"/"+k.String(), func(t *testing.T) {
				data := build(k, archives.F("fine.txt", "ok"), archives.F(c.entry, "bad"))
				mustReject(t, data, Options{}, c.rule, c.entry)
			})
		}
	}
}

func TestNameLimits(t *testing.T) {
	l := Limits{MaxPathLen: 40, MaxNameLen: 20, MaxDepth: 4}
	cases := []struct {
		name  string
		entry string
		rule  Rule
	}{
		{"too deep", "a/b/c/d/e.txt", RuleTooDeep},
		{"path too long", "aaaaaaaaaaaaaaaa/bbbbbbbbbbbbbbbb/cccccccccccccccc", RulePathTooLong},
		{"component too long", strings.Repeat("x", 21), RuleComponentTooLong},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mustReject(t, build(zipFmt, archives.F(c.entry, "x")), Options{Limits: l}, c.rule, c.entry)
		})
	}
	// At the limits is fine.
	ok := build(zipFmt, archives.F("a/b/c/d", "x"), archives.F(strings.Repeat("y", 20), "x"))
	_, _ = mustExtract(t, ok, Options{Limits: l})
}

func TestTreeConflicts(t *testing.T) {
	cases := []struct {
		name    string
		entries []archives.Entry
		rule    Rule
		entry   string
	}{
		{"duplicate file", []archives.Entry{archives.F("a", "1"), archives.F("a", "2")}, RuleDuplicate, "a"},
		{"duplicate directory", []archives.Entry{archives.D("d"), archives.D("d")}, RuleDuplicate, "d/"},
		{"case collision", []archives.Entry{archives.F("Readme.txt", "1"), archives.F("README.TXT", "2")}, RuleCaseCollision, "README.TXT"},
		{"case collision in a folder name", []archives.Entry{archives.F("Dir/a", "1"), archives.F("dir/b", "2")}, RuleCaseCollision, "dir/b"},
		{"file then directory of the same name", []archives.Entry{archives.F("a", "1"), archives.D("a")}, RuleFileDirConflict, "a/"},
		{"directory then file of the same name", []archives.Entry{archives.D("a"), archives.F("a", "1")}, RuleFileDirConflict, "a"},
		{"file then a path below it", []archives.Entry{archives.F("a", "1"), archives.F("a/b", "2")}, RuleFileDirConflict, "a/b"},
		{"path below, then a file of that name", []archives.Entry{archives.F("a/b", "2"), archives.F("a", "1")}, RuleFileDirConflict, "a"},
	}
	for _, c := range cases {
		for _, k := range []kind{zipFmt, tgzFmt} {
			t.Run(c.name+"/"+k.String(), func(t *testing.T) {
				mustReject(t, build(k, c.entries...), Options{}, c.rule, c.entry)
			})
		}
	}
}

func TestLinksAreRejectedNotSkipped(t *testing.T) {
	for _, k := range []kind{zipFmt, tarFmt, tgzFmt} {
		t.Run("symlink/"+k.String(), func(t *testing.T) {
			mustReject(t, build(k, archives.F("a", "x"), archives.L("link", "/etc/passwd")), Options{}, RuleSymlink, "link")
		})
	}
	for _, k := range []kind{tarFmt, tgzFmt} {
		t.Run("hardlink/"+k.String(), func(t *testing.T) {
			mustReject(t, build(k, archives.F("a", "x"), archives.H("link", "a")), Options{}, RuleHardlink, "link")
		})
	}
	// A symlink whose target looks harmless is still a symlink.
	mustReject(t, build(tarFmt, archives.L("here", "there")), Options{}, RuleSymlink, "here")
}

func TestTooManyEntries(t *testing.T) {
	var es []archives.Entry
	for i := range 11 {
		es = append(es, archives.F(fmt.Sprintf("f%02d", i), "x"))
	}
	for _, k := range []kind{zipFmt, tgzFmt} {
		t.Run(k.String(), func(t *testing.T) {
			mustReject(t, build(k, es...), Options{Limits: Limits{MaxEntries: 10}}, RuleTooManyEntries, "")
			_, _ = mustExtract(t, build(k, es[:10]...), Options{Limits: Limits{MaxEntries: 10}})
		})
	}
}

// ---- tar-only entry types ----

func rawTar(t *testing.T, hdrs ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hdrs {
		if h.Typeflag == tar.TypeReg && h.Size == 0 {
			h.Size = int64(len("data"))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("WriteHeader(%q): %v", h.Name, err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte("data")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTarSpecialEntriesAreRejected(t *testing.T) {
	cases := []struct {
		name string
		h    *tar.Header
		rule Rule
	}{
		{"character device", &tar.Header{Name: "dev", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}, RuleDevice},
		{"block device", &tar.Header{Name: "dev", Typeflag: tar.TypeBlock, Devmajor: 8, Devminor: 0}, RuleDevice},
		{"fifo", &tar.Header{Name: "pipe", Typeflag: tar.TypeFifo}, RuleFifo},
		{"contiguous file", &tar.Header{Name: "c", Typeflag: tar.TypeCont, Size: 0}, RuleUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.h.Mode = 0o644
			data := rawTar(t, &tar.Header{Name: "ok", Typeflag: tar.TypeReg, Mode: 0o644}, c.h)
			mustReject(t, data, Options{}, c.rule, c.h.Name)
		})
	}
}

// archive/tar's writer drops GNU.sparse records, so the archive is made with a placeholder key of the
// same length and the key is patched in afterwards. The reader keeps the record in PAXRecords, as
// it does for a genuinely sparse file.
func TestPaxSparseEntryIsRejected(t *testing.T) {
	const key = "GNU.sparse.size"
	pad := bytes.Repeat([]byte("x"), len(key))
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range []*tar.Header{
		{Name: "ok", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644},
		{Name: "hole", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644, PAXRecords: map[string]string{string(pad): "1000000"}},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte("data"))
	}
	_ = tw.Close()
	data := bytes.Replace(buf.Bytes(), pad, []byte(key), 1)
	if !bytes.Contains(data, []byte(key)) {
		t.Fatal("the key was not patched in")
	}
	mustReject(t, data, Options{}, RuleSparse, "hole")
}

func TestPaxGlobalHeaderIsMetadataNotAnEntry(t *testing.T) {
	// git archive and many tar writers begin with one; it must not count as a file or a name.
	data := rawTar(t,
		&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "abc"}},
		&tar.Header{Name: "top/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "top/a.txt", Typeflag: tar.TypeReg, Mode: 0o644},
	)
	res, _ := mustExtract(t, data, Options{Strip: StripAuto})
	if got := paths(res); !slices.Equal(got, []string{"a.txt"}) {
		t.Errorf("files = %v", got)
	}
}

func TestDotSlashPrefixAndRootEntry(t *testing.T) {
	// "tar -C dir -cf x ." writes "./" and "./name".
	data := rawTar(t,
		&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "./bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "./bin/app", Typeflag: tar.TypeReg, Mode: 0o755},
		&tar.Header{Name: "./lib/x", Typeflag: tar.TypeReg, Mode: 0o644},
	)
	res, _ := mustExtract(t, data, Options{Strip: StripAuto})
	if got := paths(res); !slices.Equal(got, []string{"bin/app", "lib/x"}) || res.Stripped != "" {
		t.Errorf("files = %v, stripped %q", got, res.Stripped)
	}
}

func TestPermissionBitsAreNormalised(t *testing.T) {
	data := rawTar(t,
		&tar.Header{Name: "suid", Typeflag: tar.TypeReg, Mode: 0o4755},
		&tar.Header{Name: "sgid", Typeflag: tar.TypeReg, Mode: 0o2755},
		&tar.Header{Name: "sticky/", Typeflag: tar.TypeDir, Mode: 0o1777},
		&tar.Header{Name: "world", Typeflag: tar.TypeReg, Mode: 0o666},
		&tar.Header{Name: "private", Typeflag: tar.TypeReg, Mode: 0o600},
		&tar.Header{Name: "group-exec", Typeflag: tar.TypeReg, Mode: 0o710},
	)
	res, dst := mustExtract(t, data, Options{})
	want := map[string]fs.FileMode{"suid": 0o755, "sgid": 0o755, "world": 0o644, "private": 0o644, "group-exec": 0o755}
	for _, f := range res.Files {
		if f.Mode != want[f.Path] {
			t.Errorf("%s: mode %v, want %v", f.Path, f.Mode, want[f.Path])
		}
		if runtime.GOOS == "windows" {
			continue // the permission bits are not stored there
		}
		st, err := os.Stat(filepath.Join(dst, f.Path))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode() != want[f.Path] {
			t.Errorf("%s on disk: %v, want %v (no setuid, setgid or sticky)", f.Path, st.Mode(), want[f.Path])
		}
	}
}

// ---- zip-only entry types and lying headers ----

// rawZipEntry writes one entry with whatever the header says, using the bytes given as the
// (already compressed) data.
func rawZip(t *testing.T, files ...func(zw *zip.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		f(zw)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func deflated(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// entryWithHeader adds a deflated entry whose header declares declared bytes, whatever data is.
func entryWithHeader(t *testing.T, name string, declared uint64, data []byte) func(*zip.Writer) {
	return func(zw *zip.Writer) {
		comp := deflated(t, data)
		w, err := zw.CreateRaw(&zip.FileHeader{
			Name:               name,
			Method:             zip.Deflate,
			CRC32:              crc32.ChecksumIEEE(data),
			CompressedSize64:   uint64(len(comp)),
			UncompressedSize64: declared,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(comp); err != nil {
			t.Fatal(err)
		}
	}
}

func TestZipHeaderThatLiesAboutSizeIsStopped(t *testing.T) {
	big := make([]byte, 3<<20) // 3 MiB of zeros: a few KiB compressed
	data := rawZip(t,
		entryWithHeader(t, "ok.txt", 2, []byte("ok")),
		entryWithHeader(t, "lies.bin", 10, big), // declares 10 bytes
	)
	res, dst, parent, err := run(t, data, Options{Limits: Limits{MaxFileBytes: 1 << 20, MaxTotalBytes: 2 << 20}})
	if err == nil {
		t.Fatalf("accepted a header that declares 10 bytes for 3 MiB: %v", paths(res))
	}
	var re *RejectError
	if !errors.As(err, &re) || re.Entry != "lies.bin" {
		t.Errorf("err = %v, want a rejection naming lies.bin", err)
	}
	assertEmpty(t, dst)
	assertNothingOutside(t, parent)
}

// neverCreate fails the test if the extractor starts writing a file: a refusal that can be made
// from the headers must be made before anything is written.
func neverCreate(t *testing.T) func(*os.Root, string, fs.FileMode) (io.WriteCloser, error) {
	return func(_ *os.Root, name string, _ fs.FileMode) (io.WriteCloser, error) {
		t.Errorf("a file was created (%s) for an archive the headers already condemn", name)
		return nil, errors.New("must not be created")
	}
}

func TestHonestZipBombIsRefusedFromTheHeaders(t *testing.T) {
	big := make([]byte, 8<<20)
	data := rawZip(t, entryWithHeader(t, "bomb.bin", uint64(len(big)), big))
	mustReject(t, data, Options{Limits: Limits{MaxFileBytes: 1 << 20}, create: neverCreate(t)}, RuleFileTooLarge, "bomb.bin")

	// Many small files that add up.
	var files []func(*zip.Writer)
	chunk := make([]byte, 400<<10)
	for i := range 4 {
		files = append(files, entryWithHeader(t, fmt.Sprintf("f%d", i), uint64(len(chunk)), chunk))
	}
	opt := Options{Limits: Limits{MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 20}, create: neverCreate(t)}
	mustReject(t, rawZip(t, files...), opt, RuleTotalTooLarge, "")
}

func TestTarGzCountsRealBytes(t *testing.T) {
	// The tar header is authoritative about size, so the cap fires on the header; a gzip layer
	// that expands is bounded by the stream cap even when every entry is individually small.
	big := bytes.Repeat([]byte{0}, 3<<20)
	data := archives.TarGz(archives.Entry{Name: "big.bin", Data: big})
	mustReject(t, data, Options{Limits: Limits{MaxFileBytes: 1 << 20}}, RuleFileTooLarge, "big.bin")

	var es []archives.Entry
	for i := range 6 {
		es = append(es, archives.Entry{Name: fmt.Sprintf("f%d", i), Data: bytes.Repeat([]byte{0}, 300<<10)})
	}
	mustReject(t, archives.TarGz(es...), Options{Limits: Limits{MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 20}}, RuleTotalTooLarge, "")
}

func TestStreamCapStopsMetadataThatIsNotAnEntry(t *testing.T) {
	// Entries that carry no file (pax global headers) are skipped, so no per-file cap sees their
	// payload. The cap on the stream itself is what bounds a run of them.
	var hdrs []*tar.Header
	for range 6 {
		hdrs = append(hdrs, &tar.Header{
			Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader,
			PAXRecords: map[string]string{"comment": strings.Repeat("c", 500<<10)},
		})
	}
	l := Limits{MaxTotalBytes: 1 << 20, MaxEntries: 100}
	mustReject(t, rawTar(t, hdrs...), Options{Limits: l}, RuleStreamTooLarge, "")
}

func TestZipEncryptedEntryIsRejected(t *testing.T) {
	data := rawZip(t, func(zw *zip.Writer) {
		w, err := zw.CreateRaw(&zip.FileHeader{Name: "secret.txt", Method: zip.Store, Flags: 0x1, CompressedSize64: 4, UncompressedSize64: 4, CRC32: 0})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("xxxx"))
	})
	mustReject(t, data, Options{}, RuleEncrypted, "secret.txt")
}

func TestZipUnknownCompressionIsRejectedNamingTheEntry(t *testing.T) {
	data := rawZip(t, func(zw *zip.Writer) {
		w, err := zw.CreateRaw(&zip.FileHeader{Name: "a.txt", Method: 99, CompressedSize64: 4, UncompressedSize64: 4})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("xxxx"))
	})
	mustReject(t, data, Options{}, RuleCompressionKind, "a.txt")
}

func TestZipDirectoryCarryingDataIsRejected(t *testing.T) {
	data := rawZip(t, func(zw *zip.Writer) {
		w, err := zw.CreateRaw(&zip.FileHeader{Name: "d/", Method: zip.Store, CompressedSize64: 4, UncompressedSize64: 4})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("xxxx"))
	})
	mustReject(t, data, Options{}, RuleDirWithData, "d/")
}

// ---- failure behaviour ----

type failingWriter struct {
	w      io.WriteCloser
	left   int
	failed bool
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if len(p) > f.left {
		f.failed = true
		n, _ := f.w.Write(p[:f.left])
		f.left = 0
		return n, syscall.ENOSPC
	}
	f.left -= len(p)
	return f.w.Write(p)
}

func (f *failingWriter) Close() error { return f.w.Close() }

func TestDiskFullLeavesNothingBehind(t *testing.T) {
	data := archives.Zip(
		archives.F("top/first.txt", "fits"),
		archives.F("top/second.txt", strings.Repeat("y", 5000)),
		archives.F("top/third.txt", "never reached"),
	)
	var created []string
	opt := Options{Strip: StripAuto}
	n := 0
	opt.create = func(root *os.Root, name string, perm fs.FileMode) (io.WriteCloser, error) {
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			return nil, err
		}
		created = append(created, name)
		n++
		if n == 2 { // the second file hits the end of the disk part-way through
			return &failingWriter{w: f, left: 100}, nil
		}
		return f, nil
	}
	_, dst, parent, err := run(t, data, opt)
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("err = %v, want it to wrap ENOSPC", err)
	}
	if len(created) != 2 {
		t.Errorf("created %v; the first file should have been written, the third never tried", created)
	}
	assertEmpty(t, dst)
	assertNothingOutside(t, parent)
}

func TestDestinationMustBeEmpty(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "in.zip")
	if err := os.WriteFile(src, archives.Zip(archives.F("a", "x")), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(parent, "dst")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dst, "keep.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(context.Background(), src, dst, Options{}); err == nil {
		t.Fatal("extracted into a folder that already had files")
	}
	if b, err := os.ReadFile(keep); err != nil || string(b) != "mine" {
		t.Errorf("an existing file was touched: %q, %v", b, err)
	}
}

func TestCancelledContextStopsAndCleans(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "in.zip")
	if err := os.WriteFile(src, archives.Zip(archives.F("a", "x"), archives.F("b", "y")), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst := filepath.Join(parent, "dst")
	if _, err := Extract(ctx, src, dst, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	assertEmpty(t, dst)
}

func TestCorruptAndUnsupportedArchives(t *testing.T) {
	good := archives.Zip(archives.F("a", strings.Repeat("hello ", 100)))
	goodGz := archives.TarGz(archives.F("a", strings.Repeat("hello ", 100)))

	t.Run("truncated zip", func(t *testing.T) {
		_, dst, _, err := run(t, good[:len(good)/2], Options{})
		if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrUnsupportedFormat) {
			t.Errorf("err = %v", err)
		}
		assertEmpty(t, dst)
	})
	t.Run("truncated tar.gz", func(t *testing.T) {
		_, dst, _, err := run(t, goodGz[:len(goodGz)-20], Options{})
		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("err = %v, want ErrCorrupt", err)
		}
		assertEmpty(t, dst)
	})
	t.Run("not an archive", func(t *testing.T) {
		_, _, _, err := run(t, []byte("just some text, not an archive at all"), Options{})
		if !errors.Is(err, ErrUnsupportedFormat) {
			t.Errorf("err = %v, want ErrUnsupportedFormat", err)
		}
	})
	for name, magic := range map[string][]byte{
		"xz":    {0xfd, '7', 'z', 'X', 'Z', 0x00, 1, 2, 3},
		"zstd":  {0x28, 0xb5, 0x2f, 0xfd, 1, 2, 3},
		"bzip2": []byte("BZh91AY&SY"),
		"7z":    {'7', 'z', 0xbc, 0xaf, 0x27, 0x1c, 0, 4},
	} {
		t.Run(name+" is named in the error", func(t *testing.T) {
			_, _, _, err := run(t, magic, Options{})
			if !errors.Is(err, ErrUnsupportedFormat) || !strings.Contains(err.Error(), name) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestDetectIgnoresTheFileName(t *testing.T) {
	dir := t.TempDir()
	for name, want := range map[string]struct {
		data []byte
		f    Format
	}{
		"looks-like.tar.gz": {archives.Zip(archives.F("a", "x")), FormatZip},
		"looks-like.zip":    {archives.TarGz(archives.F("a", "x")), FormatTarGz},
		"plain.bin":         {archives.Tar(archives.F("a", "x")), FormatTar},
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, want.data, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := Detect(p)
		if err != nil || got != want.f {
			t.Errorf("%s: %v, %v; want %v", name, got, err, want.f)
		}
	}
}

// ---- the stdlib's own guard, as a second line of defence ----

// os.Root is what keeps a missed rule from writing outside the destination. This pins down what it
// does on the OS under test, which the plan listed as not yet confirmed for Windows (E.8).
func TestOSRootRefusesToLeaveItsDirectory(t *testing.T) {
	parent := t.TempDir()
	dst := filepath.Join(parent, "dst")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	names := []string{"../escape.txt", "a/../../escape.txt", "/abs/escape.txt"}
	if runtime.GOOS == "windows" {
		names = append(names, `..\escape.txt`, `C:\escape.txt`, `\\server\share\x`)
	}
	for _, n := range names {
		if f, err := root.OpenFile(n, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
			_ = f.Close()
			t.Errorf("root.OpenFile(%q) succeeded", n)
		}
	}
	ents, _ := os.ReadDir(parent)
	if len(ents) != 1 {
		t.Errorf("the parent holds %d entries, want only dst", len(ents))
	}
}

// ---- fuzzing ----

func fuzzOptions() Options {
	return Options{Limits: Limits{MaxTotalBytes: 1 << 20, MaxFileBytes: 1 << 19, MaxEntries: 200}, Strip: StripAuto}
}

// checkInvariants is what every accepted archive must satisfy, however it was crafted.
func checkInvariants(t *testing.T, res *Result, dst, parent string) {
	t.Helper()
	assertNothingOutside(t, parent)
	l := DefaultLimits()
	seen := map[string]bool{}
	for _, f := range res.Files {
		if _, _, re := splitName(f.Path, l); re != nil {
			t.Fatalf("an accepted path breaks the name rules: %v", re)
		}
		if f.Mode != 0o644 && f.Mode != 0o755 {
			t.Fatalf("%s has mode %v", f.Path, f.Mode)
		}
		k := fold(f.Path)
		if seen[k] {
			t.Fatalf("%s collides with an earlier path", f.Path)
		}
		seen[k] = true
		st, err := os.Lstat(filepath.Join(dst, filepath.FromSlash(f.Path)))
		if err != nil || !st.Mode().IsRegular() || st.Size() != f.Size {
			t.Fatalf("%s: %v %v", f.Path, st, err)
		}
	}
}

func FuzzExtractZip(f *testing.F) {
	f.Add(archives.CraftWindowsZip("app", "1.0.0"))
	f.Add(archives.Zip(archives.F("a/b.txt", "x"), archives.D("c"), archives.L("l", "a")))
	f.Add(archives.Zip(archives.F("../x", "x")))
	f.Add([]byte("PK\x03\x04"))
	f.Fuzz(func(t *testing.T, data []byte) {
		res, dst, parent, err := run(t, data, fuzzOptions())
		if err != nil {
			assertEmpty(t, dst)
			assertNothingOutside(t, parent)
			return
		}
		checkInvariants(t, res, dst, parent)
	})
}

func FuzzExtractTar(f *testing.F) {
	f.Add(archives.CraftLinuxTarGz("app", "1.0.0", "x64"))
	f.Add(archives.Tar(archives.F("a/b.txt", "x"), archives.D("c"), archives.L("l", "a"), archives.H("h", "a/b.txt")))
	f.Add(archives.Tar(archives.F("../x", "x")))
	f.Add([]byte("\x1f\x8b"))
	f.Fuzz(func(t *testing.T, data []byte) {
		res, dst, parent, err := run(t, data, fuzzOptions())
		if err != nil {
			assertEmpty(t, dst)
			assertNothingOutside(t, parent)
			return
		}
		checkInvariants(t, res, dst, parent)
	})
}

// A zip keeps unix file types in the high bits of the external attributes, as Info-ZIP writes
// them. Devices, pipes and sockets are rejected like their tar counterparts.
func TestZipUnixSpecialFilesAreRejected(t *testing.T) {
	cases := []struct {
		name string
		mode fs.FileMode
		rule Rule
	}{
		{"character device", fs.ModeDevice | fs.ModeCharDevice | 0o644, RuleDevice},
		{"block device", fs.ModeDevice | 0o644, RuleDevice},
		{"named pipe", fs.ModeNamedPipe | 0o644, RuleFifo},
		{"socket", fs.ModeSocket | 0o644, RuleSocket},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := rawZip(t, func(zw *zip.Writer) {
				h := &zip.FileHeader{Name: "special", Method: zip.Store}
				h.SetMode(c.mode)
				w, err := zw.CreateHeader(h)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = w.Write(nil)
			})
			mustReject(t, data, Options{}, c.rule, "special")
		})
	}
}

// An archive with no files must not "succeed": the engine would swap an empty tree over a
// working install. Directories alone do not count as content.
func TestArchiveWithoutFilesIsRejected(t *testing.T) {
	for _, k := range []kind{zipFmt, tarFmt, tgzFmt} {
		t.Run(k.String()+" with nothing", func(t *testing.T) {
			mustReject(t, build(k), Options{}, RuleEmpty, "")
		})
		t.Run(k.String()+" with only directories", func(t *testing.T) {
			mustReject(t, build(k, archives.D("app/"), archives.D("app/sub/")), Options{}, RuleEmpty, "")
		})
	}
	t.Run("an empty tar is named as one", func(t *testing.T) {
		_, _, _, err := run(t, archives.Tar(), Options{})
		if errors.Is(err, ErrUnsupportedFormat) {
			t.Errorf("err = %v, want the empty rule, not 'not an archive'", err)
		}
	})
	t.Run("a zero-byte file says so", func(t *testing.T) {
		_, _, _, err := run(t, nil, Options{})
		if !errors.Is(err, ErrUnsupportedFormat) || !strings.Contains(err.Error(), "empty") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestEmptyNameIsRejected(t *testing.T) {
	data := rawZip(t, func(zw *zip.Writer) {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: "", Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("x"))
	})
	mustReject(t, data, Options{}, RuleEmptyName, "")
}

// ---- the last lines of defence, tested directly ----
//
// archive/zip and archive/tar already refuse to hand back more bytes than a header declares, so
// through Extract the counting below is never the first to fire. It exists so that the limits do not
// depend on that, and these tests are what keep it from rotting.

type endless struct{ b byte }

func (e endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = e.b
	}
	return len(p), nil
}

func TestCopyCountedStopsAtEitherCapWhateverTheSourceHolds(t *testing.T) {
	ctx := context.Background()
	if n, err := copyCounted(ctx, io.Discard, endless{'x'}, 1000, 1<<30); !errors.Is(err, errFileCap) || n < 1000 || n > 1000+64<<10 {
		t.Errorf("file cap: n=%d err=%v", n, err)
	}
	if n, err := copyCounted(ctx, io.Discard, endless{'x'}, 1<<30, 5000); !errors.Is(err, errTotalCap) || n < 5000 || n > 5000+64<<10 {
		t.Errorf("total cap: n=%d err=%v", n, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := copyCounted(cctx, io.Discard, endless{'x'}, 1<<30, 1<<30); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestWriteFileChecksTheBytesItWroteAgainstTheHeader(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	create := func(r *os.Root, name string, perm fs.FileMode) (io.WriteCloser, error) {
		return r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	}
	l := DefaultLimits()
	entry := func(declared int64, data string) (rawEntry, planned) {
		return rawEntry{Name: "f", open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(data)), nil }},
			planned{name: "f", path: "f", size: declared, mode: 0o644}
	}

	e, p := entry(3, "hello") // declares 3, holds 5
	_, err = writeFile(context.Background(), root, create, e, p, l, 0)
	var re *RejectError
	if !errors.As(err, &re) || re.Rule != RuleSizeMismatch {
		t.Errorf("declared 3, held 5: %v", err)
	}

	e, p = entry(5, "hello")
	p.path = "g"
	small := l
	small.MaxFileBytes = 4
	if _, err = writeFile(context.Background(), root, create, e, p, small, 0); !errors.As(err, &re) || re.Rule != RuleFileTooLarge {
		t.Errorf("over the per-file cap: %v", err)
	}

	p.path = "h"
	if _, err = writeFile(context.Background(), root, create, e, p, l, l.MaxTotalBytes-2); !errors.As(err, &re) || re.Rule != RuleTotalTooLarge {
		t.Errorf("over the total cap: %v", err)
	}
}

func TestAnArchiveThatChangesBetweenThePassesIsNotWritten(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.zip")
	if err := os.WriteFile(src, archives.Zip(archives.F("a.txt", "one")), 0o600); err != nil {
		t.Fatal(err)
	}
	l := DefaultLimits()
	p, err := buildPlan(context.Background(), src, FormatZip, l, Strip{})
	if err != nil {
		t.Fatal(err)
	}
	// Somebody swaps the file after it was checked.
	if err := os.WriteFile(src, archives.Zip(archives.F("evil.txt", "two")), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = write(context.Background(), src, FormatZip, dst, l, p, Options{})
	var re *RejectError
	if !errors.As(err, &re) || re.Rule != RuleCorrupt || !strings.Contains(re.Detail, "changed") {
		t.Fatalf("err = %v, want a rejection saying the archive changed", err)
	}
}

// Two entries that were somehow planned onto one path (the tree check normally makes this
// impossible) must still not overwrite each other: files are created with O_EXCL.
func TestCreateRefusesToOverwriteAnEarlierFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.zip")
	if err := os.WriteFile(src, archives.Zip(archives.F("a", "1"), archives.F("b", "2")), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &plan{entries: []planned{
		{name: "a", path: "same", size: 1, mode: 0o644},
		{name: "b", path: "same", size: 1, mode: 0o644},
	}}
	dst := filepath.Join(dir, "out")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := write(context.Background(), src, FormatZip, dst, DefaultLimits(), p, Options{}); err == nil {
		t.Fatal("the second entry replaced the first")
	}
	if b := readFile(t, dst, "same"); b != "1" {
		t.Errorf("same = %q, want the first entry's data", b)
	}
}
