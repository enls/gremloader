// Package events writes the activity log: one JSON object per line (JSON Lines), so "what did
// GremLoader do and when" is something you can grep, tail, load into jq or query from the CLI.
//
// Every message and URL is passed through internal/redact first, so signed download links and
// tokens never reach the file. The file rotates by size (5 MiB, keeping 3 by default) so it
// cannot grow without bound. Writing the log must never be the reason an install fails, so Emit
// reports errors to the caller, who is expected to note and ignore them.
package events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/enls/gremloader/internal/clock"
	"github.com/enls/gremloader/internal/redact"
)

// Limits. A line is bounded when it is written so that the log can always be read back: Scan holds
// one line at a time, up to maxLine, and skips anything longer.
const (
	maxLine  = 1 << 20
	maxField = 16 << 10 // longest Msg or URL written; the rest is cut and marked
)

// Levels.
const (
	Debug = "debug"
	Info  = "info"
	Warn  = "warn"
	Error = "error"
)

// Event is one log line. Zero-valued fields are omitted.
type Event struct {
	Time       time.Time `json:"ts"`
	Level      string    `json:"level"`
	Event      string    `json:"event"`
	Package    string    `json:"package,omitempty"`
	Version    string    `json:"version,omitempty"`
	From       string    `json:"from,omitempty"`
	To         string    `json:"to,omitempty"`
	Step       string    `json:"step,omitempty"`
	Result     string    `json:"result,omitempty"`
	URL        string    `json:"url,omitempty"`
	Bytes      int64     `json:"bytes,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Code       string    `json:"code,omitempty"`
	Msg        string    `json:"msg,omitempty"`
}

// Option configures a Logger.
type Option func(*Logger)

// WithClock sets the time source (default: the system clock).
func WithClock(c clock.Clock) Option { return func(l *Logger) { l.clock = c } }

// WithMaxSize sets the rotation threshold in bytes (default 5 MiB).
func WithMaxSize(n int64) Option { return func(l *Logger) { l.maxSize = n } }

// WithKeep sets how many rotated files are kept (default 3).
func WithKeep(n int) Option { return func(l *Logger) { l.keep = n } }

// Logger appends events to a file. It is safe for concurrent use.
type Logger struct {
	mu      sync.Mutex
	path    string
	f       *os.File
	size    int64
	maxSize int64
	keep    int
	clock   clock.Clock
}

// Open creates the folder if needed and opens (or creates) the log for appending.
func Open(path string, opts ...Option) (*Logger, error) {
	l := &Logger{path: path, maxSize: 5 << 20, keep: 3, clock: clock.System{}}
	for _, o := range opts {
		o(l)
	}
	if l.keep < 1 {
		l.keep = 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("event log folder: %w", err)
	}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Logger) open() error {
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("event log: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("event log: %w", err)
	}
	l.f, l.size = f, st.Size()
	if l.size > 0 && !endsWithNewline(l.path, l.size) {
		// A crash (or a full disk) left a partial last line. Close it off so the next event starts
		// on its own line instead of being glued to the fragment and lost with it.
		n, _ := f.Write([]byte{'\n'})
		l.size += int64(n)
	}
	return nil
}

// shorten cuts s to at most limit bytes (on a character boundary) and marks the cut.
func shorten(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const mark = "...[truncated]"
	cut := max(limit-len(mark), 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}

// endsWithNewline reports whether the last byte of the file at path is a newline. Any doubt (it
// cannot be read) counts as yes: a stray blank line is harmless, a failed open is not.
func endsWithNewline(path string, size int64) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer func() { _ = f.Close() }()
	var b [1]byte
	if _, err := f.ReadAt(b[:], size-1); err != nil {
		return true
	}
	return b[0] == '\n'
}

// Emit stamps (if the time is zero), redacts and appends one event.
func (l *Logger) Emit(e Event) error {
	if e.Time.IsZero() {
		e.Time = l.clock.Now()
	}
	e.Time = e.Time.UTC()
	if e.Level == "" {
		e.Level = Info
	}
	e.Msg = shorten(redact.Text(e.Msg), maxField)
	e.URL = shorten(redact.URL(e.URL), maxField)

	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return fmt.Errorf("event log is closed")
	}
	if l.size > 0 && l.size+int64(len(line)) > l.maxSize {
		l.rotate() // best effort: on failure we keep appending to the current file
	}
	n, err := l.f.Write(line)
	l.size += int64(n)
	return err
}

// rotate shifts path -> path.1 -> path.2 ... and drops the oldest. Called with l.mu held.
func (l *Logger) rotate() {
	if err := l.f.Close(); err != nil {
		return
	}
	l.f = nil
	// If anything below fails the log keeps working: we reopen whatever file is at l.path.
	defer func() {
		if l.f == nil {
			if err := l.open(); err != nil {
				l.f = nil
			}
		}
	}()
	_ = os.Remove(fmt.Sprintf("%s.%d", l.path, l.keep))
	for i := l.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1))
	}
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		return
	}
	l.size = 0
}

// Close flushes and closes the file.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Scan reads events from r, skipping lines that are not valid events (a torn last line after a
// crash, a hand edit). It returns the events and how many lines were skipped.
func Scan(r io.Reader) (events []Event, skipped int, err error) {
	br := bufio.NewReaderSize(r, maxLine)
	for {
		line, rerr := br.ReadSlice('\n')
		tooLong := false
		for rerr == bufio.ErrBufferFull { // a line we cannot hold: drop the rest of it
			tooLong = true
			_, rerr = br.ReadSlice('\n')
		}
		if tooLong {
			skipped++
		} else if line = bytes.TrimSpace(line); len(line) > 0 {
			var e Event
			if json.Unmarshal(line, &e) != nil || e.Event == "" {
				skipped++
			} else {
				events = append(events, e)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				rerr = nil
			}
			return events, skipped, rerr
		}
	}
}
