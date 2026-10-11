//go:build !windows

package peversion

import (
	"errors"
	"fmt"
)

// ProductVersion is only implemented on Windows: the version resource is a Windows convention,
// and adopting an install by its executable's version is a Windows feature in Phase 1.
func ProductVersion(path string) (string, error) {
	return "", fmt.Errorf("reading the version of %s: %w", path, errors.ErrUnsupported)
}
