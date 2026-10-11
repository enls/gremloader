//go:build !windows && !linux

package cli

import "os"

// isTerminal is not implemented on this OS: never a terminal, so changes need --yes.
func isTerminal(*os.File) bool { return false }
