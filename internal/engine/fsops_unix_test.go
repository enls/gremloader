//go:build !windows

package engine

import (
	"os"
	"syscall"
)

// The error the system gives for a rename across volumes. Unix has no sharing violations.
var (
	errCrossDevice error = &os.LinkError{Op: "rename", Err: syscall.EXDEV}
	errSharing     error
)
