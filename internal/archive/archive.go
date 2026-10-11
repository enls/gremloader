// Package archive extracts zip, tar and tar.gz files into a directory. It is the only code in
// GremLoader that unpacks anything, and everything in an archive is treated as hostile input
// (ADR 0010).
//
// The policy is applied to every entry before the first byte is written, and the same
// rules apply on every OS so that a package behaves the same everywhere: names with backslashes,
// colons, control characters, "..", absolute paths, Windows device names (also with an
// extension) and trailing dots or spaces are rejected, as are duplicates, names that differ only
// in case, and file/directory conflicts. Symbolic and hard links, device nodes, FIFOs, sockets,
// sparse and encrypted entries are rejected rather than skipped. Permission bits are reduced to 0644 or 0755.
// Declared sizes are never trusted: the bytes actually written are counted against per-file and
// total limits. Output goes only through an [os.Root] on the destination, so even a rule that
// was missed cannot write outside it.
//
// An archive is accepted whole or not at all: any error leaves the destination empty.
package archive

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Limits bound what an archive may contain. Zero fields mean the default.
type Limits struct {
	MaxTotalBytes int64 // bytes written, all files together (default 4 GiB)
	MaxFileBytes  int64 // bytes of any one file (default 2 GiB)
	MaxEntries    int   // entries of every kind (default 20,000)
	MaxPathLen    int   // bytes of a relative path (default 200)
	MaxNameLen    int   // bytes of one path component (default 255)
	MaxDepth      int   // components in a path (default 32)
}

// DefaultLimits are the Phase 1 limits (plan E.3.5).
func DefaultLimits() Limits {
	return Limits{
		MaxTotalBytes: 4 << 30,
		MaxFileBytes:  2 << 30,
		MaxEntries:    20000,
		MaxPathLen:    200,
		MaxNameLen:    255,
		MaxDepth:      32,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxTotalBytes > 0 {
		d.MaxTotalBytes = l.MaxTotalBytes
	}
	if l.MaxFileBytes > 0 {
		d.MaxFileBytes = l.MaxFileBytes
	}
	if l.MaxEntries > 0 {
		d.MaxEntries = l.MaxEntries
	}
	if l.MaxPathLen > 0 {
		d.MaxPathLen = l.MaxPathLen
	}
	if l.MaxNameLen > 0 {
		d.MaxNameLen = l.MaxNameLen
	}
	if l.MaxDepth > 0 {
		d.MaxDepth = l.MaxDepth
	}
	return d
}

// Strip says how many leading path components to remove from every entry.
type Strip struct {
	// Auto removes one component when every entry lives under the same single top-level folder
	// (how release archives are packed), and none otherwise. N is ignored.
	Auto bool
	// N is a fixed number of components. An entry left with no path is a directory that is
	// dropped; a file left with none is an error.
	N int
}

// StripAuto is the default for release archives.
var StripAuto = Strip{Auto: true}

// Options configures Extract.
type Options struct {
	Limits Limits
	Strip  Strip

	// create opens an output file; tests replace it to inject write failures.
	create func(root *os.Root, name string, perm fs.FileMode) (io.WriteCloser, error)
}

// File is a regular file that was extracted.
type File struct {
	Path   string // slash-separated, relative to the destination, after strip
	Size   int64
	Mode   fs.FileMode // 0644 or 0755
	SHA256 string      // lower-case hex of the bytes written
}

// Result describes what Extract wrote.
type Result struct {
	Format   Format
	Files    []File   // in archive order
	Dirs     []string // every directory in the tree, explicit or implied, sorted
	Stripped string   // the top-level folder removed by StripAuto or N, "" if none
	Bytes    int64    // total bytes written
}

// ErrCorrupt means the archive could not be read to the end.
var ErrCorrupt = errors.New("archive is corrupt or truncated")

// Extract unpacks the archive at src into dst. dst is created if absent and must otherwise be
// an empty directory. On any error dst is left empty.
func Extract(ctx context.Context, src, dst string, opt Options) (*Result, error) {
	lim := opt.Limits.withDefaults()
	format, err := Detect(src)
	if err != nil {
		return nil, err
	}

	// Pass 1: check every entry and decide the tree. Nothing is written.
	plan, err := buildPlan(ctx, src, format, lim, opt.Strip)
	if err != nil {
		return nil, err
	}

	// Pass 2: write it, counting real bytes.
	if err := prepareDestination(dst); err != nil {
		return nil, err
	}
	res, err := write(ctx, src, format, dst, lim, plan, opt)
	if err != nil {
		cleanDestination(dst)
		return nil, err
	}
	return res, nil
}

// planned is one entry that will be written.
type planned struct {
	name string // the archive's spelling, to notice a changed archive in pass 2
	path string // final slash-separated path
	dir  bool
	size int64 // declared
	mode fs.FileMode
	skip bool // consumed by strip, or archive metadata
}

type plan struct {
	entries  []planned
	dirs     []string
	stripped string
}

type seen struct {
	name  string
	parts []string
	dir   bool
	size  int64
	mode  fs.FileMode
	skip  bool
}

func buildPlan(ctx context.Context, src string, format Format, l Limits, strip Strip) (*plan, error) {
	w, err := openWalker(src, format, l)
	if err != nil {
		return nil, err
	}
	defer func() { _ = w.Close() }()

	var all []seen
	var declared int64
	err = w.walk(ctx, func(e rawEntry) error {
		if len(all) >= l.MaxEntries {
			return reject("", RuleTooManyEntries, "more than %d entries", l.MaxEntries)
		}
		s, err := classify(e, l)
		if err != nil {
			return err
		}
		if !s.dir && !s.skip {
			if s.size > l.MaxFileBytes {
				return reject(e.Name, RuleFileTooLarge, "declares %d bytes, limit %d", s.size, l.MaxFileBytes)
			}
			declared += s.size
			if declared > l.MaxTotalBytes {
				return reject(e.Name, RuleTotalTooLarge, "the archive declares more than %d bytes in all", l.MaxTotalBytes)
			}
		}
		all = append(all, s)
		return nil
	})
	if err != nil {
		return nil, wrapWalk(err)
	}

	n := stripCount(all, strip)
	p := &plan{entries: make([]planned, len(all))}
	t := newTree()
	for i, s := range all {
		pe := planned{name: s.name, dir: s.dir, size: s.size, mode: s.mode, skip: s.skip}
		if s.skip {
			p.entries[i] = pe
			continue
		}
		parts := s.parts
		if len(parts) <= n {
			if !s.dir {
				return nil, reject(s.name, RuleNothingAfterStrip, "stripping %d leading components leaves no file name", n)
			}
			pe.skip = true // the folder that was stripped away, or the archive root
			p.entries[i] = pe
			continue
		}
		parts = parts[n:]
		pe.path = strings.Join(parts, "/")
		var re *RejectError
		if s.dir {
			re = t.addDir(s.name, parts)
		} else {
			re = t.addFile(s.name, parts)
		}
		if re != nil {
			return nil, re
		}
		p.entries[i] = pe
	}
	if n > 0 {
		for _, s := range all {
			if len(s.parts) >= n {
				p.stripped = strings.Join(s.parts[:n], "/")
				break
			}
		}
	}
	if !slices.ContainsFunc(p.entries, func(e planned) bool { return !e.skip && !e.dir }) {
		// Installing this would swap an empty tree over a working install.
		return nil, reject("", RuleEmpty, "the archive contains no files")
	}
	for _, d := range t.dirs {
		p.dirs = append(p.dirs, d)
	}
	sort.Strings(p.dirs)
	return p, nil
}

// classify applies the entry-type and name rules to one entry.
func classify(e rawEntry, l Limits) (seen, error) {
	if e.Type == typeSkip {
		return seen{name: e.Name, skip: true}, nil
	}
	switch e.Type {
	case typeSymlink:
		return seen{}, reject(e.Name, RuleSymlink, "symbolic links are never extracted")
	case typeHardlink:
		return seen{}, reject(e.Name, RuleHardlink, "hard links are never extracted")
	case typeDevice:
		return seen{}, reject(e.Name, RuleDevice, "device nodes are never extracted")
	case typeFifo:
		return seen{}, reject(e.Name, RuleFifo, "named pipes are never extracted")
	case typeSocket:
		return seen{}, reject(e.Name, RuleSocket, "sockets are never extracted")
	case typeSparse:
		return seen{}, reject(e.Name, RuleSparse, "sparse files are not supported")
	case typeEncrypted:
		return seen{}, reject(e.Name, RuleEncrypted, "encrypted entries are not supported")
	case typeOther:
		return seen{}, reject(e.Name, RuleUnsupported, "%s", e.Detail)
	}
	parts, dirName, re := splitName(e.Name, l)
	if re != nil {
		return seen{}, re
	}
	isDir := e.Type == typeDir || dirName
	if isDir && e.Size > 0 {
		return seen{}, reject(e.Name, RuleDirWithData, "a directory entry carries %d bytes of data", e.Size)
	}
	if !isDir && len(parts) == 0 {
		return seen{}, reject(e.Name, RuleEmptyName, "a file with no name")
	}
	mode := fs.FileMode(0o644)
	if isDir || e.Mode&0o111 != 0 {
		mode = 0o755
	}
	return seen{name: e.Name, parts: parts, dir: isDir, size: e.Size, mode: mode}, nil
}

// stripCount is the number of leading components to remove.
func stripCount(all []seen, s Strip) int {
	if !s.Auto {
		return max(s.N, 0)
	}
	top := ""
	count := 0
	for _, e := range all {
		if e.skip || len(e.parts) == 0 {
			continue
		}
		count++
		if top == "" {
			top = e.parts[0]
		}
		if e.parts[0] != top {
			return 0
		}
		if len(e.parts) == 1 && !e.dir {
			return 0 // a loose file at the top: there is no wrapping folder
		}
	}
	if count == 0 {
		return 0
	}
	return 1
}

func wrapWalk(err error) error {
	var re *RejectError
	switch {
	case errors.As(err, &re), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, ErrCorrupt):
		return err
	}
	return fmt.Errorf("%w: %w", ErrCorrupt, err)
}

func prepareDestination(dst string) error {
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return err
	}
	ents, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	if len(ents) != 0 {
		return fmt.Errorf("extract destination %s is not empty", dst)
	}
	return nil
}

// cleanDestination empties dst after a failure. Best effort: the engine discards the staging
// area anyway, this keeps a half-written tree from being mistaken for a result.
func cleanDestination(dst string) {
	root, err := os.OpenRoot(dst)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	ents, err := os.ReadDir(dst)
	if err != nil {
		return
	}
	for _, e := range ents {
		_ = root.RemoveAll(e.Name())
	}
}

func write(ctx context.Context, src string, format Format, dst string, l Limits, p *plan, opt Options) (*Result, error) {
	root, err := os.OpenRoot(dst)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	create := opt.create
	if create == nil {
		create = func(r *os.Root, name string, perm fs.FileMode) (io.WriteCloser, error) {
			return r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		}
	}

	w, err := openWalker(src, format, l)
	if err != nil {
		return nil, err
	}
	defer func() { _ = w.Close() }()

	for _, d := range p.dirs {
		if err := root.MkdirAll(filepath.FromSlash(d), 0o755); err != nil {
			return nil, fmt.Errorf("creating directory %s: %w", d, err)
		}
	}

	res := &Result{Format: format, Dirs: p.dirs, Stripped: p.stripped}
	i := 0
	err = w.walk(ctx, func(e rawEntry) error {
		if i >= len(p.entries) || p.entries[i].name != e.Name {
			return reject(e.Name, RuleCorrupt, "the archive changed while it was being extracted")
		}
		pe := p.entries[i]
		i++
		if pe.skip || pe.dir {
			return nil
		}
		f, err := writeFile(ctx, root, create, e, pe, l, res.Bytes)
		if err != nil {
			return err
		}
		res.Bytes += f.Size
		res.Files = append(res.Files, f)
		return nil
	})
	if err != nil {
		return nil, wrapWalk(err)
	}
	return res, nil
}

func writeFile(ctx context.Context, root *os.Root, create func(*os.Root, string, fs.FileMode) (io.WriteCloser, error),
	e rawEntry, pe planned, l Limits, written int64) (File, error) {
	rc, err := e.open()
	if err != nil {
		return File{}, entryReadError(e.Name, err)
	}
	defer func() { _ = rc.Close() }()

	name := filepath.FromSlash(pe.path)
	out, err := create(root, name, 0o600)
	if err != nil {
		return File{}, fmt.Errorf("creating %s: %w", pe.path, err)
	}
	h := sha256.New()
	n, copyErr := copyCounted(ctx, io.MultiWriter(out, h), rc, l.MaxFileBytes, l.MaxTotalBytes-written)
	closeErr := out.Close()

	switch {
	case copyErr != nil:
		return File{}, entryCopyError(e.Name, pe.path, copyErr, l)
	case closeErr != nil:
		return File{}, fmt.Errorf("writing %s: %w", pe.path, closeErr)
	case n != pe.size:
		return File{}, reject(e.Name, RuleSizeMismatch, "the header declares %d bytes but the entry holds %d", pe.size, n)
	}
	if err := root.Chmod(name, pe.mode); err != nil {
		return File{}, fmt.Errorf("setting the mode of %s: %w", pe.path, err)
	}
	return File{Path: pe.path, Size: n, Mode: pe.mode, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// errWriteFailed marks a failure of the destination, as opposed to the archive.
type errWriteFailed struct{ err error }

func (e *errWriteFailed) Error() string { return e.err.Error() }
func (e *errWriteFailed) Unwrap() error { return e.err }

var (
	errFileCap  = errors.New("file cap")
	errTotalCap = errors.New("total cap")
)

// copyCounted copies src to dst and fails as soon as more than fileCap bytes or totalCap bytes
// have been produced, whatever the headers said.
func copyCounted(ctx context.Context, dst io.Writer, src io.Reader, fileCap, totalCap int64) (int64, error) {
	buf := make([]byte, 64<<10)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		nr, rerr := src.Read(buf)
		if nr > 0 {
			n += int64(nr)
			if n > fileCap {
				return n, errFileCap
			}
			if n > totalCap {
				return n, errTotalCap
			}
			if _, werr := dst.Write(buf[:nr]); werr != nil {
				return n, &errWriteFailed{werr}
			}
		}
		if rerr == io.EOF {
			return n, nil
		}
		if rerr != nil {
			return n, rerr
		}
	}
}

func entryCopyError(entry, path string, err error, l Limits) error {
	var wf *errWriteFailed
	switch {
	case errors.As(err, &wf):
		return fmt.Errorf("writing %s: %w", path, wf.err)
	case errors.Is(err, errFileCap):
		return reject(entry, RuleFileTooLarge, "holds more than the limit of %d bytes", l.MaxFileBytes)
	case errors.Is(err, errTotalCap):
		return reject(entry, RuleTotalTooLarge, "the archive holds more than %d bytes in all", l.MaxTotalBytes)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return entryReadError(entry, err)
}

// entryReadError turns a failure to read one entry's data into a rejection naming it.
func entryReadError(entry string, err error) error {
	var re *RejectError
	if errors.As(err, &re) {
		return re
	}
	if errors.Is(err, zip.ErrAlgorithm) {
		return reject(entry, RuleCompressionKind, "%v", err)
	}
	return reject(entry, RuleCorrupt, "%v", err)
}
