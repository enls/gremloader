package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The errors the system gives for a rename across volumes and for a file another program holds.
var (
	errCrossDevice error = &os.LinkError{Op: "rename", Err: windows.ERROR_NOT_SAME_DEVICE}
	errSharing     error = &os.LinkError{Op: "rename", Err: windows.ERROR_SHARING_VIOLATION}
)

// Decision 7 of the M5 bug check: access denied on a read-only target fails at once, saying how
// to clear the attribute; on any other target it is still retried (a scanner holding the file).
func TestReadOnlyTargetIsNotRetried(t *testing.T) {
	var slept []time.Duration
	retrySleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { retrySleep = time.Sleep })
	target := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	denied := &os.LinkError{Op: "rename", Err: windows.ERROR_ACCESS_DENIED}

	n := 0
	if err := retryShared(target, func() error { n++; return denied }); !errors.Is(err, denied.Err) || n != 5 {
		t.Errorf("access denied on a writable file: err=%v calls=%d", err, n)
	}
	if err := os.Chmod(target, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o644) })
	n, slept = 0, nil
	err := retryShared(target, func() error { n++; return denied })
	var ro *readOnlyError
	if !errors.As(err, &ro) || !errors.Is(err, denied.Err) || n != 1 || len(slept) != 0 {
		t.Errorf("access denied on a read-only file: err=%v calls=%d sleeps=%v", err, n, slept)
	}
	n = 0
	if err := retryShared(target, func() error { n++; return errSharing }); !errors.Is(err, errSharing) || n != 5 {
		t.Errorf("a sharing violation on a read-only file: err=%v calls=%d", err, n)
	}
}
