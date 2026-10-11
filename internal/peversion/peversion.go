// Package peversion reads the product version of a Windows executable, which is how an existing
// install is adopted without trusting a version the user types (version_from_exe).
//
// The value is the one Explorer and PowerShell show as ProductVersion
// ((Get-Item app.exe).VersionInfo.ProductVersion, what CraftWerk read): the ProductVersion string
// of the version resource, and only when there is none the four numbers of the fixed version
// block. Real Craft apps report "0.6.0" and 0.6.0.0 respectively. The caller normalises the
// result with ver.ParseLoose, which accepts the four-part form when the fourth part is 0.
package peversion

import "errors"

// ErrNoVersion means the file has no version resource, or one that names no product version.
var ErrNoVersion = errors.New("the file has no product version")
