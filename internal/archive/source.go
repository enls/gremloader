package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// Format is an archive format.
type Format int

// The supported formats. xz, zstd, bzip2 and 7z are not (no dependency, E.7).
const (
	FormatUnknown Format = iota
	FormatZip
	FormatTar
	FormatTarGz
)

func (f Format) String() string {
	switch f {
	case FormatZip:
		return "zip"
	case FormatTar:
		return "tar"
	case FormatTarGz:
		return "tar.gz"
	}
	return "unknown"
}

// ErrUnsupportedFormat means the file is not a zip, tar or tar.gz archive.
var ErrUnsupportedFormat = errors.New("not a zip, tar or tar.gz archive")

// Detect reads the file's first bytes and says what kind of archive it is. The name's extension
// is not consulted: it is chosen by whoever published the release.
func Detect(path string) (Format, error) {
	f, err := os.Open(path) //nolint:gosec // path is the verified download, chosen by the engine
	if err != nil {
		return FormatUnknown, err
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return FormatUnknown, err
	}
	return detect(head[:n])
}

func detect(head []byte) (Format, error) {
	switch {
	case len(head) == 0:
		return FormatUnknown, fmt.Errorf("%w: the file is empty", ErrUnsupportedFormat)
	case len(head) == 512 && bytes.Equal(head, make([]byte, 512)):
		// A tar with no entries is only its end-of-archive marker: zero blocks, no "ustar" to see.
		return FormatTar, nil
	case bytes.HasPrefix(head, []byte("PK\x03\x04")), bytes.HasPrefix(head, []byte("PK\x05\x06")):
		return FormatZip, nil
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		return FormatTarGz, nil
	case len(head) >= 262 && bytes.Equal(head[257:262], []byte("ustar")):
		return FormatTar, nil
	}
	for _, k := range []struct {
		magic []byte
		name  string
	}{
		{[]byte{0xfd, '7', 'z', 'X', 'Z', 0x00}, "xz"},
		{[]byte{0x28, 0xb5, 0x2f, 0xfd}, "zstd"},
		{[]byte("BZh"), "bzip2"},
		{[]byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c}, "7z"},
	} {
		if bytes.HasPrefix(head, k.magic) {
			return FormatUnknown, fmt.Errorf("%w: %s archives are not supported", ErrUnsupportedFormat, k.name)
		}
	}
	return FormatUnknown, ErrUnsupportedFormat
}

type entryType int

const (
	typeRegular entryType = iota
	typeDir
	typeSymlink
	typeHardlink
	typeDevice
	typeFifo
	typeSocket
	typeSparse
	typeEncrypted
	typeOther
	typeSkip // metadata that is not an entry (a pax global header)
)

// rawEntry is one archive member as the archive describes it, before any policy is applied.
type rawEntry struct {
	Name   string
	Type   entryType
	Size   int64       // declared; never trusted
	Mode   fs.FileMode // permission bits as stored; 0 when the archive has none
	Detail string      // what typeOther was
	// open returns the entry's data. For a tar it is valid only during the walk callback.
	open func() (io.ReadCloser, error)
}

// walker yields an archive's entries in order.
type walker interface {
	walk(ctx context.Context, fn func(rawEntry) error) error
	Close() error
}

// hardCap bounds what is read from a (decompressed) stream no matter what the entries say: the
// declared total, the headers and block padding of the permitted number of entries, and slack.
func hardCap(l Limits) int64 {
	return l.MaxTotalBytes + int64(l.MaxEntries)*2048 + 1<<20
}

func openWalker(src string, format Format, l Limits) (walker, error) {
	switch format {
	case FormatZip:
		zr, err := zip.OpenReader(src)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		return &zipWalker{zr: zr, lim: l}, nil
	case FormatTar, FormatTarGz:
		f, err := os.Open(src) //nolint:gosec // src is the verified download, chosen by the engine
		if err != nil {
			return nil, err
		}
		var r io.Reader = f
		if format == FormatTarGz {
			gz, err := gzip.NewReader(f)
			if err != nil {
				_ = f.Close()
				return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
			}
			r = gz
		}
		c := hardCap(l)
		return &tarWalker{f: f, tr: tar.NewReader(&capReader{r: r, left: c, limit: c})}, nil
	}
	return nil, ErrUnsupportedFormat
}

// ---- zip ----

type zipWalker struct {
	zr  *zip.ReadCloser
	lim Limits
}

func (w *zipWalker) Close() error { return w.zr.Close() }

func (w *zipWalker) walk(ctx context.Context, fn func(rawEntry) error) error {
	// The central directory is read in full by OpenReader, so the count is known before any
	// entry is looked at.
	if len(w.zr.File) > w.lim.MaxEntries {
		return reject("", RuleTooManyEntries, "%d entries, limit %d", len(w.zr.File), w.lim.MaxEntries)
	}
	for _, f := range w.zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := rawEntry{Name: f.Name, Size: clampSize(f.UncompressedSize64), Mode: f.Mode().Perm()}
		m := f.Mode()
		switch {
		case f.Flags&0x1 != 0:
			e.Type = typeEncrypted
		case m&fs.ModeSymlink != 0:
			e.Type = typeSymlink
		case m&(fs.ModeDevice|fs.ModeCharDevice) != 0:
			e.Type = typeDevice
		case m&fs.ModeNamedPipe != 0:
			e.Type = typeFifo
		case m&fs.ModeSocket != 0:
			e.Type = typeSocket
		case m&fs.ModeIrregular != 0:
			e.Type, e.Detail = typeOther, "irregular file mode"
		case m.IsDir() || strings.HasSuffix(f.Name, "/"):
			e.Type = typeDir
		default:
			e.Type = typeRegular
		}
		f := f
		e.open = func() (io.ReadCloser, error) { return f.Open() }
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

func clampSize(u uint64) int64 {
	if u > 1<<62 {
		return 1 << 62
	}
	return int64(u)
}

// ---- tar ----

type tarWalker struct {
	f  *os.File
	tr *tar.Reader
}

func (w *tarWalker) Close() error { return w.f.Close() }

func (w *tarWalker) walk(ctx context.Context, fn func(rawEntry) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := w.tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return streamError(err)
		}
		e := rawEntry{Name: h.Name, Size: h.Size, Mode: h.FileInfo().Mode().Perm()}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA is what old tar writes; reading it is the point
			e.Type = typeRegular
		case tar.TypeDir:
			e.Type = typeDir
		case tar.TypeSymlink:
			e.Type = typeSymlink
		case tar.TypeLink:
			e.Type = typeHardlink
		case tar.TypeChar, tar.TypeBlock:
			e.Type = typeDevice
		case tar.TypeFifo:
			e.Type = typeFifo
		case tar.TypeGNUSparse:
			e.Type = typeSparse
		case tar.TypeXGlobalHeader:
			e.Type = typeSkip
		default:
			e.Type, e.Detail = typeOther, fmt.Sprintf("tar type flag %q", string(h.Typeflag))
		}
		for k := range h.PAXRecords {
			if strings.HasPrefix(k, "GNU.sparse.") {
				e.Type = typeSparse
			}
		}
		e.open = func() (io.ReadCloser, error) { return io.NopCloser(w.tr), nil }
		if err := fn(e); err != nil {
			return err
		}
	}
}

// streamError classifies a failure to read the archive's stream.
func streamError(err error) error {
	var re *RejectError
	if errors.As(err, &re) {
		return re
	}
	return fmt.Errorf("%w: %w", ErrCorrupt, err)
}

// capReader fails when more than left bytes are read from it.
type capReader struct {
	r     io.Reader
	left  int64
	limit int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, reject("", RuleStreamTooLarge, "the archive expands beyond %d bytes of stream", c.limit)
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}
