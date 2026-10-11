// Package atomicfile replaces a file so that a reader sees either the old contents or the new
// ones, never a mix, and a crash at any moment leaves one of the two on disk.
//
// The new contents go to a temporary file in the same directory, which is flushed to disk and
// then renamed over the target (a same-directory rename is atomic on NTFS and on the Linux file
// systems GremLoader targets). On Linux the directory is flushed as well, so the rename itself
// survives a power cut. A crash before the rename leaves a stray ".<name>.*.tmp" beside the
// target: it is ignored by readers and removed by a later Write once it is old enough that no
// other writer can still be using it.
//
// state.json, gremloader.lock and gremloader.local.toml are written through here.
package atomicfile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// StaleAfter is how old a leftover temporary file must be before Write removes it.
const StaleAfter = 10 * time.Minute

// Test seams.
var (
	rename = os.Rename
	now    = time.Now
	sleep  = time.Sleep
)

// Write replaces path with data. perm applies to a newly created file (the umask still applies on
// Linux); an existing file's permissions are not carried over.
func Write(path string, data []byte, perm fs.FileMode) (err error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()

	if err = tmp.Chmod(perm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("write %s: flush: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = replace(name, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	syncDir(dir)
	removeStale(dir, base)
	return nil
}

// replace renames tmp over path. On Windows a rename over a file that another process has open
// without delete sharing (Go's own os.Open does that, so a second gremloader reading state.json is
// enough) fails until that handle closes; reads are short, so a few short retries ride it out.
func replace(tmp, path string) error {
	err := rename(tmp, path)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	delay := 20 * time.Millisecond
	for range 7 { // about 2.5 s in all
		sleep(delay)
		delay *= 2
		if err = rename(tmp, path); err == nil {
			return nil
		}
	}
	return err
}

// syncDir makes a rename durable on Linux. Windows cannot open a directory for flushing and
// NTFS journals the rename itself, so there it is skipped. Failure is not reported: the data is
// already safe in the file, only the name may roll back after a power cut.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir) //nolint:gosec // G304: dir is the directory of a path the caller chose
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// removeStale deletes temporary files a crashed Write left beside base. Only old ones: a young
// one may belong to a writer that is still running.
func removeStale(dir, base string) {
	matches, err := filepath.Glob(filepath.Join(dir, "."+escapeGlob(base)+".*.tmp"))
	if err != nil {
		return
	}
	cutoff := now().Add(-StaleAfter)
	for _, m := range matches {
		st, err := os.Lstat(m)
		if err != nil || !st.Mode().IsRegular() || st.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(m)
	}
}

func escapeGlob(s string) string {
	if runtime.GOOS == "windows" {
		// filepath.Match has no escape character on Windows; a name with a meta character is
		// simply not cleaned up. GremLoader's own file names have none.
		if strings.ContainsAny(s, `*?[`) {
			return "\x00"
		}
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
