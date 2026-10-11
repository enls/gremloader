package engine

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isSharingViolation reports the errors Windows gives while another program holds a file open.
// Access denied is included because antivirus scanners and the search indexer produce it for a
// moment; a real permission problem still fails after the retries.
func isSharingViolation(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

func isCrossDevice(err error) bool { return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE) }

// freeBytes is the space available to this user on the volume holding path.
func freeBytes(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, nil, nil); err != nil {
		return 0, err
	}
	return free, nil
}
