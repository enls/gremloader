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
	// wanted holds the paths this change put or found in place, by case-folded name. On a
	// case-insensitive file system an old "Readme.txt" may be the new "README.txt", and removing
	// the old name would delete the new file.
	wanted map[string][]string
}

func (x *executor) want(rel string) {
	if x.wanted == nil {
		x.wanted = map[string][]string{}
	}
	k := strings.ToLower(rel)
	x.wanted[k] = append(x.wanted[k], rel)
}

// isWanted reports whether the file or folder fi at rel is one this change put or found in place
// under a name that differs only in case.
func (x *executor) isWanted(rel string, fi fs.FileInfo) bool {
	for _, w := range x.wanted[strings.ToLower(rel)] {
		if w == rel {
			continue
		}
		if wi, err := os.Lstat(x.abs(w)); err == nil && os.SameFile(fi, wi) {
			return true
		}
	}
	return false
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
		x.want(rel)
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
	x.want(rel)
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
	x.want(o.path)
	if err := placeFile(o.src, p, o.move, func() error { return x.e.at("place") }); err != nil {
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
	case x.isWanted(rel, fi):
		return nil // the same file under its new name
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
	if fi, err := os.Lstat(p); err == nil && x.isWanted(rel, fi) {
		return nil // the same folder under its new name
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
	if err := removeTemps(dir, l); err != nil {
		errs = append(errs, err)
	}
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

// removeTemps deletes the temporary files placeFile leaves beside a file the journal touched when
// the process dies between filling one and renaming it into place. They are not journaled (that
// would cost a flush per file); each folder is read once.
func removeTemps(dir string, l *journal.Log) error {
	beside := map[string]map[string]bool{} // folder -> names of files the journal touched there
	for _, r := range l.Records {
		switch r.Op {
		case journal.OpCreated, journal.OpBackup, journal.OpRemoved:
			parent, base := filepath.Split(filepath.Join(dir, filepath.FromSlash(r.Path)))
			if beside[parent] == nil {
				beside[parent] = map[string]bool{}
			}
			beside[parent][base] = true
		}
	}
	var errs []error
	for parent, names := range beside {
		ents, err := os.ReadDir(parent)
		if err != nil {
			continue // a folder that is not there holds no temporary files
		}
		for _, e := range ents {
			if base, ok := tempOf(e.Name()); ok && names[base] && e.Type().IsRegular() {
				if err := remove(filepath.Join(parent, e.Name())); err != nil {
					errs = append(errs, err)
				}
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
//
// A path that changes kind between versions (a file that becomes a folder, or the reverse) is
// cleared out of the way before anything else: the old file, or the old folder's files and then
// the folder, deepest first. All of it is journaled, so undo and rollback put it back. Something
// in the way that the old version did not install is left alone, and the mkdir or place that
// meets it fails, naming it.
func manifestOps(newFiles, newDirs []string, staged string, oldFiles, oldDirs []string) []op {
	// A file is kept only if the new version has a file there, a folder only if it has a folder.
	keepFile, keepDir := map[string]bool{}, map[string]bool{}
	for _, f := range newFiles {
		keepFile[f] = true
	}
	for _, d := range newDirs {
		keepDir[d] = true
	}
	// Case-folded, as a case-insensitive file system sees them: the new folders (newDirs holds the
	// implied ones too, as archive.Result.Dirs does) and the new files.
	newDir, newFile := map[string]bool{}, map[string]bool{}
	for _, d := range newDirs {
		newDir[strings.ToLower(d)] = true
	}
	for _, f := range newFiles {
		newFile[strings.ToLower(f)] = true
	}
	// cleared: the old folders where a new file goes; under reports whether p is in one of them.
	var cleared []string
	for _, d := range oldDirs {
		if !keepDir[d] && newFile[strings.ToLower(d)] {
			cleared = append(cleared, strings.ToLower(d)+"/")
		}
	}
	under := func(p string) bool {
		lp := strings.ToLower(p)
		return slices.ContainsFunc(cleared, func(c string) bool { return strings.HasPrefix(lp+"/", c) })
	}

	var ops []op
	early := map[string]bool{}
	for _, f := range slices.Sorted(slices.Values(oldFiles)) {
		if !keepFile[f] && (newDir[strings.ToLower(f)] || under(f)) {
			ops = append(ops, op{kind: opRemove, path: f})
			early[f] = true
		}
	}
	var earlyDirs []string
	for _, d := range oldDirs {
		if !keepDir[d] && under(d) {
			earlyDirs = append(earlyDirs, d)
			early[d] = true
		}
	}
	slices.SortFunc(earlyDirs, func(a, b string) int { return -depthThenName(a, b) })
	for _, d := range earlyDirs {
		ops = append(ops, op{kind: opRmdir, path: d})
	}

	dirs := slices.Clone(newDirs)
	slices.SortFunc(dirs, func(a, b string) int { return depthThenName(a, b) })
	for _, d := range dirs {
		ops = append(ops, op{kind: opMkdir, path: d})
	}
	files := slices.Sorted(slices.Values(newFiles))
	for _, f := range files {
		ops = append(ops, op{kind: opPlace, path: f, src: filepath.Join(staged, filepath.FromSlash(f)), move: true})
	}
	for _, f := range slices.Sorted(slices.Values(oldFiles)) {
		if !keepFile[f] && !early[f] {
			ops = append(ops, op{kind: opRemove, path: f})
		}
	}
	gone := slices.DeleteFunc(slices.Clone(oldDirs), func(d string) bool { return keepDir[d] || early[d] })
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
