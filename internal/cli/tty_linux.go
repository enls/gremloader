package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// isTerminal reports whether f is a terminal (a prompt can be answered there).
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS) //nolint:gosec // G115: a file descriptor fits in an int
	return err == nil
}
