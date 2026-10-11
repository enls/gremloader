package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // test
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// On one volume a backup is a hard link: no bytes are copied.
func TestBackupIsAHardLinkOnOneVolume(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "a"), filepath.Join(d, "b")
	write(t, src, "x")
	if err := linkOrCopy(src, dst); err != nil {
		t.Fatal(err)
	}
	a, _ := os.Stat(src)
	b, _ := os.Stat(dst)
	if !os.SameFile(a, b) {
		t.Error("the backup is a copy, not a link")
	}
}

func TestCopyKeepsBytesAndRefusesToOverwrite(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "a"), filepath.Join(d, "b")
	write(t, src, "bytes")
	if err := copyFile(src, dst); err != nil || read(t, dst) != "bytes" {
		t.Fatalf("copy: %v", err)
	}
	if err := copyFile(src, dst); err == nil {
		t.Error("copied over an existing file")
	}
}

// Across volumes a rename fails; the file then goes to a temporary name beside the target and is
// renamed over it, so the target is still replaced in one step (P3-20).
func TestPlaceAcrossVolumes(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "staged"), filepath.Join(d, "app", "app.exe")
	write(t, src, "new")
	write(t, dst, "old")
	calls := 0
	osRename = func(from, to string) error {
		calls++
		if calls == 1 {
			return errCrossDevice
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { osRename = os.Rename })
	if err := placeFile(src, dst, true); err != nil {
		t.Fatal(err)
	}
	if read(t, dst) != "new" || calls != 2 {
		t.Errorf("content %q after %d renames", read(t, dst), calls)
	}
	if _, err := os.Stat(src); err == nil {
		t.Error("the staged file was left behind")
	}
	ents, _ := os.ReadDir(filepath.Dir(dst))
	if len(ents) != 1 {
		t.Errorf("left in the target folder: %v", ents)
	}
}

// Without move the source must survive (a saved file in previous/ that a rollback places).
func TestPlaceWithoutMoveKeepsTheSource(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "saved"), filepath.Join(d, "app.exe")
	write(t, src, "v1")
	write(t, dst, "v2")
	if err := placeFile(src, dst, false); err != nil {
		t.Fatal(err)
	}
	if read(t, dst) != "v1" || read(t, src) != "v1" {
		t.Error("wrong content or the source was consumed")
	}
}

// Another program holding the file for a moment is ridden out; anything else fails at once.
func TestSharingViolationsAreRetried(t *testing.T) {
	if errSharing == nil {
		t.Skip("no sharing violations on this system")
	}
	var slept []time.Duration
	retrySleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { retrySleep = time.Sleep })

	n := 0
	err := retryShared(func() error {
		if n++; n < 3 {
			return errSharing
		}
		return nil
	})
	if err != nil || n != 3 || len(slept) != 2 {
		t.Errorf("err=%v calls=%d sleeps=%v", err, n, slept)
	}

	n, slept = 0, nil
	if err := retryShared(func() error { n++; return errSharing }); !errors.Is(err, errSharing) || n != 5 {
		t.Errorf("a lasting violation: err=%v calls=%d", err, n)
	}
	n = 0
	other := errors.New("disk on fire")
	if err := retryShared(func() error { n++; return other }); !errors.Is(err, other) || n != 1 {
		t.Errorf("another error was retried: calls=%d", n)
	}
}

// restore after an undo that already ran: a missing backup is fine only if the file there is
// the saved one.
func TestRestoreIsIdempotentButNotGullible(t *testing.T) {
	d := t.TempDir()
	dst := filepath.Join(d, "app.exe")
	write(t, dst, "saved")
	sum, _, _ := hashFile(dst)
	if err := restore(filepath.Join(d, "gone"), dst, sum); err != nil {
		t.Errorf("already restored: %v", err)
	}
	write(t, dst, "something else")
	if err := restore(filepath.Join(d, "gone"), dst, sum); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("err = %v", err)
	}
}
