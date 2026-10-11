// Package proc finds running programs that were started from an install directory, so an update
// never replaces the files of an app that is open.
//
// A process is matched by the path of its executable, not by its name: the same app running from
// another folder (a second copy, a portable USB stick) is not this install and does not block it.
//
// Limits, stated plainly: the executable path of another user's process cannot be read without
// elevation, so such a process is not noticed. A program that runs from elsewhere but has a DLL
// of this install loaded is not noticed either. On systems other than Windows and Linux nothing is
// ever reported.
package proc

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Process is one running program.
type Process struct {
	PID  int
	Path string // the executable, absolute
}

// Under returns the running processes whose executable lies in dir or below it. The calling
// process is never included (self-update replaces the running binary on purpose). Processes whose
// path cannot be read are skipped.
func Under(dir string) ([]Process, error) {
	roots := candidates(dir)
	all, err := list()
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var out []Process
	for _, p := range all {
		if p.PID == self || p.Path == "" {
			continue
		}
		for _, r := range roots {
			if inside(r, p.Path, runtime.GOOS == "windows") {
				out = append(out, p)
				break
			}
		}
	}
	return out, nil
}

// candidates are the spellings of dir an executable path may use: as given (made absolute) and
// with links resolved, since the system reports the real location of a program started through a
// junction or symlink.
func candidates(dir string) []string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = filepath.Clean(dir)
	}
	out := []string{abs}
	if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
		out = append(out, real)
	}
	return out
}

// inside reports whether file lies in dir or below it. Windows paths compare without regard to
// case, as NTFS does.
func inside(dir, file string, foldCase bool) bool {
	dir = strings.TrimRight(filepath.Clean(dir), `\/`)
	file = filepath.Clean(file)
	if len(file) <= len(dir) {
		return false
	}
	head, sep := file[:len(dir)], file[len(dir)]
	if sep != '/' && sep != filepath.Separator {
		return false // "app2\x.exe" is not inside "app"
	}
	if foldCase {
		return strings.EqualFold(head, dir)
	}
	return head == dir
}
