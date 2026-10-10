// Package ver is the version type for *packages* (the software GremLoader installs). It is not
// internal/version, which is the build identity of GremLoader itself.
//
// Precedence follows Semantic Versioning 2.0.0 (https://semver.org). On top of the strict form
// there is a loose form for the strings the real world produces: a leading "v", missing minor or
// patch numbers, and Windows file versions with a trailing ".0" (0.6.0.0). Build metadata is
// carried but never takes part in precedence, as the specification requires.
//
// Tags that do not parse are reported to the caller; ver never guesses. See ADR 0008.
package ver

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed version.
type Version struct {
	Major, Minor, Patch uint64
	Pre                 []string // pre-release identifiers, e.g. ["rc", "5"]
	Build               string   // build metadata after "+", ignored by Compare
}

// ParseError says what was wrong with an input string.
type ParseError struct {
	Input  string
	Reason string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("version %q: %s", e.Input, e.Reason)
}

// Parse accepts strict SemVer 2.0.0: MAJOR.MINOR.PATCH[-PRERELEASE][+BUILD], with no prefix.
func Parse(s string) (Version, error) {
	return parse(s, s, false)
}

// ParseLoose accepts what Parse accepts plus: a leading "v" or "V"; one or two numeric parts
// (1, 1.2) which are padded with zeros; and a fourth numeric part that is exactly 0 (the .NET
// four-part file version 1.2.3.0), which is dropped. A non-zero fourth part is an error because
// dropping it would make two different versions compare equal.
func ParseLoose(s string) (Version, error) {
	t := strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	return parse(s, t, true)
}

// FromTag turns a release tag into a version: it removes prefix (which may be empty) and then
// parses loosely. The tag must also pass ValidTag.
func FromTag(tag, prefix string) (Version, error) {
	if !ValidTag(tag) {
		return Version{}, &ParseError{Input: tag, Reason: "not a valid tag name"}
	}
	if prefix != "" && !strings.HasPrefix(tag, prefix) {
		return Version{}, &ParseError{Input: tag, Reason: fmt.Sprintf("does not start with the tag prefix %q", prefix)}
	}
	v, err := parse(tag, strings.TrimPrefix(tag, prefix), true)
	if err != nil {
		return Version{}, err
	}
	return v, nil
}

// ValidTag reports whether s is safe to use as a release tag: it must start with a letter or
// digit and contain only letters, digits and ". _ + -", up to 128 bytes. Tags come from the
// network and end up in URLs and file names, so this is checked before anything else (it
// rejects "..", slashes, backslashes, colons, spaces and control characters).
func ValidTag(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c == '.' || c == '_' || c == '+' || c == '-'):
		default:
			return false
		}
	}
	return !strings.Contains(s, "..")
}

func parse(orig, s string, loose bool) (Version, error) {
	fail := func(reason string) (Version, error) { return Version{}, &ParseError{Input: orig, Reason: reason} }
	if s == "" {
		return fail("empty")
	}

	var v Version
	rest := s
	if i := strings.IndexByte(rest, '+'); i >= 0 {
		build := rest[i+1:]
		rest = rest[:i]
		if err := validIdentifiers(build, false); err != nil {
			return fail("build metadata: " + err.Error())
		}
		v.Build = build
	}
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		pre := rest[i+1:]
		rest = rest[:i]
		if err := validIdentifiers(pre, true); err != nil {
			return fail("pre-release: " + err.Error())
		}
		v.Pre = strings.Split(pre, ".")
	}

	parts := strings.Split(rest, ".")
	max := 3
	if loose {
		max = 4
	}
	if len(parts) > max || (!loose && len(parts) != 3) {
		if loose {
			return fail("too many numeric parts")
		}
		return fail("want MAJOR.MINOR.PATCH")
	}
	nums := make([]uint64, 4)
	for i, p := range parts {
		n, err := parseNumeric(p)
		if err != nil {
			return fail(fmt.Sprintf("part %d: %v", i+1, err))
		}
		nums[i] = n
	}
	if len(parts) == 4 && nums[3] != 0 {
		return fail("a fourth numeric part is only accepted when it is 0")
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}

func parseNumeric(p string) (uint64, error) {
	if p == "" {
		return 0, fmt.Errorf("empty")
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return 0, fmt.Errorf("%q is not a number", p)
		}
	}
	if len(p) > 1 && p[0] == '0' {
		return 0, fmt.Errorf("%q has a leading zero", p)
	}
	n, err := strconv.ParseUint(p, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is too large", p)
	}
	return n, nil
}

// validIdentifiers checks a dot-separated identifier list. Pre-release numeric identifiers may
// not have leading zeros; build identifiers may.
func validIdentifiers(s string, pre bool) error {
	if s == "" {
		return fmt.Errorf("empty")
	}
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return fmt.Errorf("empty identifier")
		}
		numeric := true
		for i := 0; i < len(id); i++ {
			c := id[i]
			switch {
			case c >= '0' && c <= '9':
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-':
				numeric = false
			default:
				return fmt.Errorf("invalid character %q", c)
			}
		}
		if pre && numeric && len(id) > 1 && id[0] == '0' {
			return fmt.Errorf("numeric identifier %q has a leading zero", id)
		}
	}
	return nil
}

// String is the canonical form: MAJOR.MINOR.PATCH[-PRE][+BUILD], never with a "v".
func (v Version) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		b.WriteByte('-')
		b.WriteString(strings.Join(v.Pre, "."))
	}
	if v.Build != "" {
		b.WriteByte('+')
		b.WriteString(v.Build)
	}
	return b.String()
}

// IsPrerelease reports whether v has pre-release identifiers.
func (v Version) IsPrerelease() bool { return len(v.Pre) > 0 }

// Compare returns -1, 0 or +1 by SemVer 2.0.0 precedence (build metadata ignored).
func (v Version) Compare(o Version) int {
	switch {
	case v.Major != o.Major:
		return cmpUint(v.Major, o.Major)
	case v.Minor != o.Minor:
		return cmpUint(v.Minor, o.Minor)
	case v.Patch != o.Patch:
		return cmpUint(v.Patch, o.Patch)
	}
	// A version without pre-release identifiers is greater than one with.
	switch {
	case len(v.Pre) == 0 && len(o.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1
	case len(o.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(o.Pre); i++ {
		if c := compareIdentifier(v.Pre[i], o.Pre[i]); c != 0 {
			return c
		}
	}
	return cmpInt(len(v.Pre), len(o.Pre))
}

// Compare is Version.Compare as a function, for sort.Slice and slices.SortFunc.
func Compare(a, b Version) int { return a.Compare(b) }

func compareIdentifier(a, b string) int {
	an, bn := isNumeric(a), isNumeric(b)
	switch {
	case an && bn:
		// Compared as numbers without parsing, so identifiers longer than 64 bits still work:
		// there are no leading zeros, so a longer string is a bigger number.
		if len(a) != len(b) {
			return cmpInt(len(a), len(b))
		}
		return strings.Compare(a, b)
	case an:
		return -1 // numeric identifiers sort before alphanumeric ones
	case bn:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func isNumeric(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
