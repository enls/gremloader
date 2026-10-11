package engine

import (
	"errors"
	"fmt"

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

// readOnly returns a clearer error than err when err is Windows refusing to replace or delete
// target because it has the read-only attribute, and nil otherwise.
func readOnly(target string, err error) error {
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil
	}
	p, perr := windows.UTF16PtrFromString(target)
	if perr != nil {
		return nil
	}
	if a, aerr := windows.GetFileAttributes(p); aerr != nil || a&windows.FILE_ATTRIBUTE_READONLY == 0 {
		return nil
	}
	return &readOnlyError{path: target, err: err}
}

type readOnlyError struct {
	path string
	err  error
}

func (e *readOnlyError) Error() string {
	// The path is in the command; callers that name the file already say it before this.
	return fmt.Sprintf(`it is read-only; clear that (attrib -r "%s") and run again`, e.path)
}

func (e *readOnlyError) Unwrap() error { return e.err }

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
