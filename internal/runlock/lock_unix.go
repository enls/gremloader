//go:build unix

package runlock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// flock locks the open file description, so two opens of run.lock conflict even inside one
// process, and the kernel releases the lock when the last descriptor closes or the process dies.
func tryLock(f *os.File) (busy bool, err error) {
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) //nolint:gosec // G115: a file descriptor fits in int
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

func unlock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN) //nolint:gosec // G115: a file descriptor fits in int
}
