package archive

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Rule names the policy rule an entry broke. The values are stable: the plan output, the event
// log and the tests refer to them.
type Rule string

// The rules. Each rejection names the entry and exactly one of these.
const (
	// Names.
	RuleEmptyName         Rule = "empty-name"
	RuleInvalidUTF8       Rule = "invalid-utf8"
	RuleControlChar       Rule = "control-char"
	RuleBackslash         Rule = "backslash"
	RuleColon             Rule = "colon"
	RuleAbsolute          Rule = "absolute"
	RuleDotDot            Rule = "dot-dot"
	RuleEmptyComponent    Rule = "empty-component"
	RuleWindowsChar       Rule = "windows-char"
	RuleReservedName      Rule = "reserved-name"
	RuleTrailingDotSpace  Rule = "trailing-dot-or-space"
	RuleComponentTooLong  Rule = "component-too-long"
	RulePathTooLong       Rule = "path-too-long"
	RuleTooDeep           Rule = "too-deep"
	RuleNothingAfterStrip Rule = "nothing-after-strip"

	// The tree.
	RuleDuplicate       Rule = "duplicate"
	RuleCaseCollision   Rule = "case-collision"
	RuleFileDirConflict Rule = "file-dir-conflict"

	// Entry types.
	RuleSymlink     Rule = "symlink"
	RuleHardlink    Rule = "hardlink"
	RuleDevice      Rule = "device"
	RuleFifo        Rule = "fifo"
	RuleSocket      Rule = "socket"
	RuleSparse      Rule = "sparse"
	RuleEncrypted   Rule = "encrypted"
	RuleUnsupported Rule = "unsupported-type"
	RuleDirWithData Rule = "directory-with-data"

	// Sizes and counts.
	RuleTooManyEntries Rule = "too-many-entries"
	RuleFileTooLarge   Rule = "file-too-large"
	RuleTotalTooLarge  Rule = "total-too-large"
	RuleStreamTooLarge Rule = "stream-too-large"
	RuleSizeMismatch   Rule = "size-mismatch"

	// Damage.
	RuleCorrupt         Rule = "corrupt"
	RuleCompressionKind Rule = "unsupported-compression"
)

// RejectError means the archive was refused because of one of its entries. Nothing is left in
// the destination.
type RejectError struct {
	Entry  string // the entry's name as the archive spells it ("" for a whole-archive rule)
	Rule   Rule
	Detail string
}

func (e *RejectError) Error() string {
	if e.Entry == "" {
		return fmt.Sprintf("archive rejected (%s): %s", e.Rule, e.Detail)
	}
	return fmt.Sprintf("archive entry %s rejected (%s): %s", display(e.Entry), e.Rule, e.Detail)
}

// display quotes a name from the network for a message: control characters become escapes and a
// very long name is shortened.
func display(s string) string {
	const max = 80
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "?") + "..."
	}
	return fmt.Sprintf("%q", s)
}

func reject(entry string, rule Rule, format string, args ...any) *RejectError {
	return &RejectError{Entry: entry, Rule: rule, Detail: fmt.Sprintf(format, args...)}
}

// splitName checks one entry name against the name rules and returns its components. A name
// that is only the archive root ("./" or ".") returns no components: the caller skips it. dir
// reports a trailing slash.
//
// The rules apply on every OS, so an archive means the same thing everywhere (P3-14): a name
// that is merely odd on Linux (a backslash, a colon) is a path separator or a data stream on
// Windows.
func splitName(raw string, l Limits) (parts []string, dir bool, err *RejectError) {
	switch {
	case raw == "":
		return nil, false, reject(raw, RuleEmptyName, "the entry has no name")
	case !utf8.ValidString(raw):
		return nil, false, reject(raw, RuleInvalidUTF8, "the name is not valid UTF-8")
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return nil, false, reject(raw, RuleControlChar, "the name contains a control character (U+%04X)", r)
		}
	}
	switch {
	case strings.Contains(raw, `\`):
		return nil, false, reject(raw, RuleBackslash, "backslash is a path separator on Windows and a letter elsewhere; the same bytes would be two different trees")
	case strings.Contains(raw, ":"):
		return nil, false, reject(raw, RuleColon, "a colon is a drive letter or an NTFS data stream on Windows")
	case strings.HasPrefix(raw, "/"):
		return nil, false, reject(raw, RuleAbsolute, "absolute path")
	}

	dir = strings.HasSuffix(raw, "/")
	for _, c := range strings.Split(strings.TrimSuffix(raw, "/"), "/") {
		switch c {
		case "":
			return nil, false, reject(raw, RuleEmptyComponent, "empty path component")
		case ".":
			continue // "./bin/app" is how tar writes a relative name; harmless
		case "..":
			return nil, false, reject(raw, RuleDotDot, "path leaves the destination")
		}
		if e := checkComponent(raw, c, l); e != nil {
			return nil, false, e
		}
		parts = append(parts, c)
	}
	if len(parts) > l.MaxDepth {
		return nil, false, reject(raw, RuleTooDeep, "%d levels deep, limit %d", len(parts), l.MaxDepth)
	}
	if n := len(strings.Join(parts, "/")); n > l.MaxPathLen {
		return nil, false, reject(raw, RulePathTooLong, "path is %d bytes, limit %d", n, l.MaxPathLen)
	}
	return parts, dir, nil
}

func checkComponent(entry, c string, l Limits) *RejectError {
	if len(c) > l.MaxNameLen {
		return reject(entry, RuleComponentTooLong, "a path component is %d bytes, limit %d", len(c), l.MaxNameLen)
	}
	if strings.ContainsAny(c, `<>"|?*`) {
		return reject(entry, RuleWindowsChar, "component %q has a character Windows does not allow in names", c)
	}
	if strings.HasSuffix(c, ".") || strings.HasSuffix(c, " ") {
		return reject(entry, RuleTrailingDotSpace, "component %q ends in a dot or space, which Windows silently drops", c)
	}
	// "NUL", "nul.txt" and "NUL .txt" are all the device, with any extension.
	stem, _, _ := strings.Cut(c, ".")
	if reservedDevice[strings.ToUpper(strings.TrimRight(stem, " "))] {
		return reject(entry, RuleReservedName, "component %q is a Windows device name", c)
	}
	return nil
}

var reservedDevice = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true,
	"COM0": true, "COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT0": true, "LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	"COM¹": true, "COM²": true, "COM³": true, "LPT¹": true, "LPT²": true, "LPT³": true,
}

// fold is the key two names must not share: file systems on Windows and macOS compare names
// without regard to case, and the rule applies on every OS so a package behaves the same
// everywhere. Upper then lower folds "ß" with "SS" as NTFS's table does.
func fold(s string) string { return strings.ToLower(strings.ToUpper(s)) }

// tree tracks what the extracted tree will contain, to find duplicates, case collisions and
// file/directory conflicts before anything is written.
type tree struct {
	files    map[string]string // folded path -> path as given
	dirs     map[string]string
	explicit map[string]bool // folded paths that were given as a directory entry
}

func newTree() *tree {
	return &tree{files: map[string]string{}, dirs: map[string]string{}, explicit: map[string]bool{}}
}

func (t *tree) addFile(entry string, parts []string) *RejectError {
	p := strings.Join(parts, "/")
	k := fold(p)
	if prev, ok := t.files[k]; ok {
		if prev == p {
			return reject(entry, RuleDuplicate, "the path appears more than once; a later entry would silently replace the earlier one")
		}
		return reject(entry, RuleCaseCollision, "differs from %q only in letter case; they are one file on Windows and macOS", prev)
	}
	if prev, ok := t.dirs[k]; ok {
		return reject(entry, RuleFileDirConflict, "is a file, but %q is a directory", prev)
	}
	if e := t.addParents(entry, parts); e != nil {
		return e
	}
	t.files[k] = p
	return nil
}

func (t *tree) addDir(entry string, parts []string) *RejectError {
	p := strings.Join(parts, "/")
	k := fold(p)
	if prev, ok := t.files[k]; ok {
		return reject(entry, RuleFileDirConflict, "is a directory, but %q is a file", prev)
	}
	if prev, ok := t.dirs[k]; ok {
		if prev != p {
			return reject(entry, RuleCaseCollision, "differs from %q only in letter case", prev)
		}
		if t.explicit[k] {
			return reject(entry, RuleDuplicate, "the directory is listed more than once")
		}
	}
	if e := t.addParents(entry, parts); e != nil {
		return e
	}
	t.dirs[k] = p
	t.explicit[k] = true
	return nil
}

// addParents registers the implied directories above parts.
func (t *tree) addParents(entry string, parts []string) *RejectError {
	for i := 1; i < len(parts); i++ {
		p := strings.Join(parts[:i], "/")
		k := fold(p)
		if prev, ok := t.files[k]; ok {
			return reject(entry, RuleFileDirConflict, "needs %q to be a directory, but it is a file", prev)
		}
		if prev, ok := t.dirs[k]; ok {
			if prev != p {
				return reject(entry, RuleCaseCollision, "directory %q differs from %q only in letter case", p, prev)
			}
			continue
		}
		t.dirs[k] = p
	}
	return nil
}
