package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/enls/gremloader/internal/journal"
)

// opKind is one kind of change to an install directory.
type opKind int

const (
	opMkdir  opKind = iota // create a directory (nothing if it exists)
	opPlace                // put a file at path, backing up what was there
	opRemove               // delete a file, backing it up first
	opRmdir                // delete a directory if it is empty
)

// op is one change. Paths are "/"-separated and relative to the install directory.
type op struct {
	kind opKind
	path string
	src  string // opPlace: the file to put there (absolute)
	move bool   // opPlace: src may be consumed (staging) rather than left intact (previous/)
}

// executor applies ops to dir, journaling each one by the write-ahead rule of package journal, so
// that undo (or repair) can always restore the directory exactly.
type executor struct {
	e   *Engine
	j   *journal.Journal
	dir string
}

func (x *executor) append(r journal.Record) error {
	if err := x.e.at("append"); err != nil {
		return err
	}
	return x.j.Append(r)
}

func (x *executor) abs(rel string) string { return filepath.Join(x.dir, filepath.FromSlash(rel)) }

// run applies ops in order. It stops at the first error, leaving the journal to undo what was done.
// Cancellation is noticed between operations, never inside one (P3-12).
func (x *executor) run(ctx context.Context, ops []op) error {
	for _, o := range ops {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := x.e.at("op"); err != nil {
			return err
		}
		var err error
		switch o.kind {
		case opMkdir:
			err = x.mkdir(o.path)
		case opPlace:
			err = x.place(o)
		case opRemove:
			err = x.remove(o.path)
		case opRmdir:
			err = x.rmdir(o.path)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (x *executor) mkdir(rel string) error {
	p := x.abs(rel)
	fi, err := os.Lstat(p)
	switch {
	case err == nil && fi.IsDir():
		return nil
	case err == nil:
		return fmt.Errorf("%s: a file is in the way of a folder the new version needs", p)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := x.append(journal.Record{Op: journal.OpMkdir, Path: rel}); err != nil {
		return err
	}
	if err := x.e.at("fs"); err != nil {
		return err
	}
	return os.Mkdir(p, 0o755) //nolint:gosec // G301: a folder of an install, readable like the rest of it
}

// backup saves the file at rel into the journal and records it under op (backup or removed).
func (x *executor) backup(rel string, recOp string) error {
	b, babs := x.j.NextBackup()
	if err := x.e.at("fs"); err != nil {
		return err
	}
	if err := linkOrCopy(x.abs(rel), babs); err != nil {
		return fmt.Errorf("backing up %s: %w", x.abs(rel), err)
	}
	sum, size, err := hashFile(babs)
	if err != nil {
		return fmt.Errorf("backing up %s: %w", x.abs(rel), err)
	}
	return x.append(journal.Record{Op: recOp, Path: rel, Backup: b, Size: size, SHA256: sum})
}

func (x *executor) place(o op) error {
	p := x.abs(o.path)
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := x.append(journal.Record{Op: journal.OpCreated, Path: o.path}); err != nil {
			return err
		}
	case err != nil:
		return err
	case !fi.Mode().IsRegular():
		return fmt.Errorf("%s: a folder or link is in the way of a file the new version needs", p)
	default:
		if err := x.backup(o.path, journal.OpBackup); err != nil {
			return err
		}
	}
	if err := x.e.at("fs"); err != nil {
		return err
	}
	// No "placed" record follows: undo does not need one (a created path is removed whether or
	// not it arrived, a backed-up one restored), and finishing forward goes by the "state" step.
	// Leaving it out halves the flushes per file.
	if err := placeFile(o.src, p, o.move); err != nil {
		return fmt.Errorf("placing %s: %w", p, err)
	}
	return nil
}

func (x *executor) remove(rel string) error {
	p := x.abs(rel)
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case !fi.Mode().IsRegular():
		return nil // not ours to delete; whatever it is, it is left alone
	}
	if err := x.backup(rel, journal.OpRemoved); err != nil {
		return err
	}
	if err := x.e.at("fs"); err != nil {
		return err
	}
	if err := remove(p); err != nil {
		return fmt.Errorf("removing %s: %w", p, err)
	}
	return nil
}

func (x *executor) rmdir(rel string) error {
	p := x.abs(rel)
	if !isEmptyDir(p) {
		return nil // something the user put there keeps it
	}
	if err := x.append(journal.Record{Op: journal.OpRmdir, Path: rel}); err != nil {
		return err
	}
	if err := x.e.at("fs"); err != nil {
		return err
	}
	if err := remove(p); err != nil {
		return fmt.Errorf("removing folder %s: %w", p, err)
	}
	return nil
}

// undo reverses a journal: every record in reverse order. It is idempotent, so an undo that was
// itself interrupted can simply be run again: a backup already moved back is recognised by its hash
// at the destination. Every record is attempted even after a failure, so as much as possible is
// restored; the errors are returned together and the journal is kept for repair.
func (e *Engine) undo(l *journal.Log) error {
	dir := l.Begin().Dir
	abs := func(rel string) string { return filepath.Join(dir, filepath.FromSlash(rel)) }
	var errs []error
	for _, r := range slices.Backward(l.Records) {
		if err := e.at("undo"); err != nil {
			if errors.Is(err, errCrash) {
				return err
			}
			errs = append(errs, err)
			continue
		}
		switch r.Op {
		case journal.OpCreated:
			if err := remove(abs(r.Path)); err != nil {
				errs = append(errs, fmt.Errorf("removing %s: %w", abs(r.Path), err))
			}
		case journal.OpBackup, journal.OpRemoved:
			if err := restore(l.BackupPath(r), abs(r.Path), r.SHA256); err != nil {
				errs = append(errs, err)
			}
		case journal.OpMkdir:
			if p := abs(r.Path); isEmptyDir(p) {
				if err := remove(p); err != nil {
					errs = append(errs, fmt.Errorf("removing folder %s: %w", p, err))
				}
			}
		case journal.OpRmdir:
			if err := os.MkdirAll(abs(r.Path), 0o755); err != nil { //nolint:gosec // G301: as above
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// restore puts a backup back at dst. If the backup is gone, an earlier undo already moved it, and
// dst must then hold exactly the saved bytes.
func restore(backup, dst, sum string) error {
	if _, err := os.Lstat(backup); errors.Is(err, fs.ErrNotExist) {
		if got, _, herr := hashFile(dst); herr == nil && got == sum {
			return nil
		}
		return fmt.Errorf("the backup of %s is missing and the file there is not the saved one", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // G301: as above
		return err
	}
	if err := rename(backup, dst); err != nil {
		return fmt.Errorf("restoring %s: %w", dst, err)
	}
	return nil
}

// manifestOps is the change from the old tree to the new one: folders first (parents before
// children), then every new file, then the old files the new version no longer has, then the old
// folders that became empty (deepest first). Without an old manifest (an adopted install) nothing
// is removed: what is there and was not replaced is not known to be ours.
func manifestOps(newFiles, newDirs []string, staged string, oldFiles, oldDirs []string) []op {
	var ops []op
	dirs := slices.Clone(newDirs)
	slices.SortFunc(dirs, func(a, b string) int { return depthThenName(a, b) })
	for _, d := range dirs {
		ops = append(ops, op{kind: opMkdir, path: d})
	}
	files := slices.Sorted(slices.Values(newFiles))
	for _, f := range files {
		ops = append(ops, op{kind: opPlace, path: f, src: filepath.Join(staged, filepath.FromSlash(f)), move: true})
	}
	keep := map[string]bool{}
	for _, f := range newFiles {
		keep[f] = true
	}
	for _, d := range newDirs {
		keep[d] = true
	}
	for _, f := range slices.Sorted(slices.Values(oldFiles)) {
		if !keep[f] {
			ops = append(ops, op{kind: opRemove, path: f})
		}
	}
	gone := slices.DeleteFunc(slices.Clone(oldDirs), func(d string) bool { return keep[d] })
	slices.SortFunc(gone, func(a, b string) int { return -depthThenName(a, b) })
	for _, d := range gone {
		ops = append(ops, op{kind: opRmdir, path: d})
	}
	return ops
}

func depthThenName(a, b string) int {
	if da, db := strings.Count(a, "/"), strings.Count(b, "/"); da != db {
		return da - db
	}
	return strings.Compare(a, b)
}
