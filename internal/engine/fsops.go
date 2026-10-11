package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Test seams: the pause between retries of a file operation, and the rename underneath.
var (
	retrySleep = time.Sleep
	osRename   = os.Rename
)

// retryShared runs op, retrying a few times when Windows reports that another program has the file
// open (an antivirus scanner, Explorer's preview, a search indexer) (P3-13). Five tries over
// about three seconds; anything else fails at once.
func retryShared(op func() error) error {
	delay := 200 * time.Millisecond
	var err error
	for attempt := range 5 {
		if attempt > 0 {
			retrySleep(delay)
			delay = min(delay*2, 2*time.Second)
		}
		if err = op(); err == nil || !isSharingViolation(err) {
			return err
		}
	}
	return err
}

func rename(from, to string) error { return retryShared(func() error { return osRename(from, to) }) }

func remove(path string) error {
	err := retryShared(func() error { return os.Remove(path) })
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// hashFile returns a file's SHA-256 and size.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a path inside an install or the state directory
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// linkOrCopy makes dst (which must not exist) hold the same bytes as src: a hard link when both are
// on one volume (no copying, and the later replace of src is a rename, so the linked bytes are never
// touched), otherwise a flushed copy.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

// copyFile copies src to a new file dst with src's permissions and flushes it to disk.
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src) //nolint:gosec // G304: as hashFile
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm()) //nolint:gosec // G304: as hashFile
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// placeFile puts the file src at dst, replacing what is there in one rename, so dst is never
// missing or half-written. With move, src may be consumed (a staged file); otherwise src is left
// as it is (a saved file in previous/ that must survive until the change commits). When src and
// dst are on different volumes, the bytes go to a temporary name beside dst first (P3-20).
func placeFile(src, dst string, move bool) error {
	if move {
		err := rename(src, dst)
		if err == nil || !isCrossDevice(err) {
			return err
		}
	}
	tmp, err := tempName(dst)
	if err != nil {
		return err
	}
	if err := linkOrCopy(src, tmp); err != nil {
		return err
	}
	if err := rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if move {
		_ = os.Remove(src)
	}
	return nil
}

// tempName is an unused name beside path, recognisable as GremLoader's.
func tempName(path string) (string, error) {
	dir, base := filepath.Split(path)
	for i := range 100 {
		p := filepath.Join(dir, fmt.Sprintf(".%s.gremloader-%d.tmp", base, i))
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			return p, nil
		}
	}
	return "", fmt.Errorf("no free temporary name beside %s", path)
}

// isEmptyDir reports whether path is a directory with nothing in it.
func isEmptyDir(path string) bool {
	f, err := os.Open(path) //nolint:gosec // G304: as hashFile
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.IsDir() {
		return false
	}
	_, err = f.Readdirnames(1)
	return errors.Is(err, io.EOF)
}
