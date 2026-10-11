//go:build !windows

package engine

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// isSharingViolation: Unix lets a file be replaced while it is open, so nothing is retried.
func isSharingViolation(error) bool { return false }

func isCrossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }

// readOnly: a read-only file does not stop a rename over it or its removal on Unix.
func readOnly(string, error) error { return nil }

// freeBytes is the space available to an unprivileged user on the file system holding path.
func freeBytes(path string) (uint64, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return 0, err
	}
	return s.Bavail * uint64(s.Bsize), nil //nolint:gosec // G115: Bsize is a small positive block size
}
