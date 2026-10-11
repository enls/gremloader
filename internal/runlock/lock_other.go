//go:build !unix && !windows

package runlock

import (
	"errors"
	"os"
)

func tryLock(*os.File) (bool, error) {
	return false, errors.New("file locking is not supported on this operating system")
}

func unlock(*os.File) error { return nil }
