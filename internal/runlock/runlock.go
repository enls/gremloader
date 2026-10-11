// Package runlock makes sure only one GremLoader run changes things at a time.
//
// The lock is a kernel lock on <state>/run.lock (flock on Linux, LockFileEx on Windows), not the
// file's existence: the kernel drops it when the holder exits, however it exits, so a crashed or
// killed run never leaves a stale lock behind. The holder writes its PID, version and start time
// into the file, but only so that the run that is turned away can say who holds it; nothing
// decides anything from those contents.
//
// The lock never waits. A second run fails at once with *LockedError (exit code 3).
//
// run.lock must never be deleted while GremLoader may be running: on Linux a run that deletes and
// recreates it would lock a different file than the holder.
package runlock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// ErrLocked is what a *LockedError matches with errors.Is.
var ErrLocked = errors.New("another GremLoader run is in progress")

// Holder describes the run that holds the lock.
type Holder struct {
	PID     int       `json:"pid"`
	Version string    `json:"version"`
	Since   time.Time `json:"since"`
}

// LockedError is returned when another run holds the lock. Holder is nil when the file could not
// be read or did not describe its holder (the holder may be between locking and writing it).
type LockedError struct {
	Path   string
	Holder *Holder
}

func (e *LockedError) Error() string {
	var b strings.Builder
	b.WriteString(ErrLocked.Error())
	if h := e.Holder; h != nil {
		fmt.Fprintf(&b, " (PID %d", h.PID)
		if v := printable(h.Version); v != "" {
			fmt.Fprintf(&b, ", gremloader %s", v)
		}
		if !h.Since.IsZero() {
			fmt.Fprintf(&b, ", since %s", h.Since.Local().Format("2006-01-02 15:04:05"))
		}
		b.WriteString(")")
	}
	fmt.Fprintf(&b, "; try again when it has finished (lock file: %s)", e.Path)
	return b.String()
}

func (e *LockedError) Is(target error) bool { return target == ErrLocked }

// Lock is a held run lock.
type Lock struct {
	f *os.File
}

// Acquire takes the lock at path, creating the file (and its directory) if needed, and records h
// in it. It fails at once with a *LockedError when another run holds it.
func Acquire(path string, h Holder) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // G703: path is <state>/run.lock
		return nil, fmt.Errorf("run lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: path is <state>/run.lock
	if err != nil {
		return nil, fmt.Errorf("run lock: %w", err)
	}
	busy, err := tryLock(f)
	if err != nil || busy {
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("run lock %s: %w", path, err)
		}
		return nil, &LockedError{Path: path, Holder: readHolder(path)}
	}

	// The description is a courtesy: failing to write it must not fail the run.
	if data, err := json.Marshal(h); err == nil {
		if f.Truncate(0) == nil {
			if _, err := f.WriteAt(append(data, '\n'), 0); err == nil {
				_ = f.Sync()
			}
		}
	}
	return &Lock{f: f}, nil
}

// Release gives the lock up. It is safe to call more than once.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	_ = f.Truncate(0)
	errU := unlock(f)
	errC := f.Close()
	if errU != nil {
		return fmt.Errorf("run lock: unlock: %w", errU)
	}
	return errC
}

// readHolder reads the description another run left. Anything odd yields nil.
func readHolder(path string) *Holder {
	f, err := os.Open(path) //nolint:gosec // G304: path is <state>/run.lock
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	var h Holder
	if n == 0 || json.Unmarshal(buf[:n], &h) != nil || h.PID <= 0 {
		return nil
	}
	return &h
}

// printable keeps a string from the lock file from putting escape sequences on the terminal.
func printable(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return -1
		}
		return r
	}, s)
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
