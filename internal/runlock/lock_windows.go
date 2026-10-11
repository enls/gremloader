//go:build windows

package runlock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// LockFileEx locks a byte range, and a locked range cannot be read by anyone else. The range is
// therefore one byte far beyond anything the file will ever hold (Windows allows locking past the
// end of a file), so the run that is turned away can still read who holds the lock.
var lockRange = windows.Overlapped{Offset: 0, OffsetHigh: 0x7fffffff}

func tryLock(f *os.File) (busy bool, err error) {
	ol := lockRange
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return true, nil
	}
	return false, err
}

func unlock(f *os.File) error {
	ol := lockRange
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}
