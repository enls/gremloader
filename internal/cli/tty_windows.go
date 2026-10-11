package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// isTerminal reports whether f is a console (a prompt can be answered there).
func isTerminal(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}
