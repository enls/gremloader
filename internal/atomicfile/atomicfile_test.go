package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestWriteCreatesAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Write(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != "two" {
		t.Errorf("contents = %q, want two", got)
	}
	if left := temps(t, path); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestFailedRenameKeepsTheOldFileAndRemovesTheTemp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Write(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("injected rename failure")
	restore := swap(&rename, func(string, string) error { return boom })
	defer restore()
	defer swap(&sleep, func(time.Duration) {})()

	if err := Write(path, []byte("new"), 0o644); !errors.Is(err, boom) {
		t.Fatalf("Write = %v, want the rename failure", err)
	}
	if got := read(t, path); got != "old" {
		t.Errorf("contents = %q, want old", got)
	}
	if left := temps(t, path); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

// A crash between creating the temporary file and renaming it (simulated: a half-written temp
// beside the target) must not affect what is read, and the leftover is cleaned up once stale.
func TestTornWriteLeavesTheOldFileAndIsCleanedUpLater(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := Write(path, []byte(`{"complete":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	torn := filepath.Join(dir, ".state.json.123456.tmp")
	if err := os.WriteFile(torn, []byte(`{"compl`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != `{"complete":true}` {
		t.Fatalf("contents = %q", got)
	}

	// Young: another writer may still own it.
	if err := Write(path, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(torn); err != nil {
		t.Fatalf("a young temp file was removed: %v", err)
	}

	old := time.Now().Add(-StaleAfter - time.Minute)
	if err := os.Chtimes(torn, old, old); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("v3"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(torn); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale temp file still there (err %v)", err)
	}
	if got := read(t, path); got != "v3" {
		t.Errorf("contents = %q, want v3", got)
	}
}

func TestStaleCleanupLeavesOtherFilesAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	others := []string{"gremloader.lock", ".gremloader.lock.1.tmp", "state.json.bak", ".state.json.tmp"}
	old := time.Now().Add(-StaleAfter - time.Hour)
	for _, o := range others {
		p := filepath.Join(dir, o)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := Write(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, o := range others {
		if _, err := os.Stat(filepath.Join(dir, o)); err != nil {
			t.Errorf("%s was removed: %v", o, err)
		}
	}
}

// Go opens files without FILE_SHARE_DELETE, so on Windows a rename over a file that another
// process is reading fails until the reader closes it. Write retries for a short while.
func TestReplaceWaitsForAShortReaderOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Linux lets a rename replace an open file")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Write(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = f.Close()
	}()
	if err := Write(path, []byte("new"), 0o644); err != nil {
		t.Fatalf("Write with a short-lived reader: %v", err)
	}
	if got := read(t, path); got != "new" {
		t.Errorf("contents = %q, want new", got)
	}
}

func TestWriteIntoAMissingDirectoryFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "state.json")
	if err := Write(path, []byte("x"), 0o644); err == nil {
		t.Fatal("Write into a missing directory succeeded")
	}
}

func TestPermissionsOfANewFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Write(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func temps(t *testing.T, path string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func swap[T any](p *T, v T) func() {
	old := *p
	*p = v
	return func() { *p = old }
}
