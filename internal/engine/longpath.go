package engine

import (
	"fmt"
	"path/filepath"
	"unicode/utf16"
)

// Windows' classic path limit (MAX_PATH) is 260 UTF-16 units including the terminating NUL, so
// 259 characters for a file; a folder must leave room for an 8.3 file name in it (CreateDirectory
// takes at most 248 including the NUL). GremLoader itself copes with longer paths (Go uses the
// long-path form), but many programs, installers and hook scripts do not.
const (
	maxWindowsFile = 259
	maxWindowsDir  = 247
)

// longPath returns the longest path under dir (files and folders, slash-separated and relative)
// that is over the Windows limit, with its length; ok is false when there is none, or when goos is
// not Windows (decision 3 of the M5 bug check: warn, never refuse).
func longPath(goos, dir string, files, dirs []string) (path string, n int, ok bool) {
	if goos != "windows" {
		return "", 0, false
	}
	base := utf16Len(dir) + 1
	check := func(rels []string, limit int) {
		for _, rel := range rels {
			if l := base + utf16Len(rel); l > limit && l > n {
				path, n, ok = filepath.Join(dir, filepath.FromSlash(rel)), l, true
			}
		}
	}
	check(files, maxWindowsFile)
	check(dirs, maxWindowsDir)
	return path, n, ok
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// noteLongPath adds the MAX_PATH warning to an item, if there is one to give, and returns it.
func (e *Engine) noteLongPath(it *Item, files, dirs []string) (string, bool) {
	p, n, ok := longPath(e.cfg.Platform.OS, it.Dir, files, dirs)
	if !ok {
		return "", false
	}
	msg := fmt.Sprintf("%s is %d characters long, over the classic Windows path limit (MAX_PATH) that many programs "+
		"and hook scripts still have; a shorter install folder avoids it", p, n)
	it.notef("%s", msg)
	return msg, true
}
