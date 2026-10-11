//go:build !windows && !linux

package proc

// list finds nothing on systems GremLoader does not yet support for this check.
func list() ([]Process, error) { return nil, nil }
