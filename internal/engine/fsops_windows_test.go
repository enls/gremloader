package engine

import (
	"os"

	"golang.org/x/sys/windows"
)

// The errors the system gives for a rename across volumes and for a file another program holds.
var (
	errCrossDevice error = &os.LinkError{Op: "rename", Err: windows.ERROR_NOT_SAME_DEVICE}
	errSharing     error = &os.LinkError{Op: "rename", Err: windows.ERROR_SHARING_VIOLATION}
)
