// Package journal is the undo log of one change to one package's install directory.
//
// Every change to an install (an update, a rollback, a remove) is recorded here *as it happens*,
// one JSON line per step, each flushed to disk before the next file operation. If the process
// dies at any point, the journal says exactly what was done, so the change can be undone (or, once
// every file is in place, finished). A journal that reaches "commit" becomes the package's
// rollback record.
//
// A journal is a directory:
//
//	undo.jsonl      the records, append-only
//	b/0001 ...      backups of files that were replaced or removed
//	manifest.json   the new tree's manifest (written by the engine, for finishing forward)
//
// Write-ahead rule (the engine's side of the contract): "created" and "mkdir" are recorded before
// the file or directory is created, so undo removes it if it exists; "backup" and "removed" are
// recorded after the backup is complete and flushed, so undo can trust that the backup exists.
package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"github.com/enls/gremloader/internal/paths"
)

// Record operations.
const (
	OpBegin   = "begin"   // first record: what is being done
	OpStep    = "step"    // a phase finished (Name)
	OpBackup  = "backup"  // Path existed and was saved to Backup before being replaced
	OpRemoved = "removed" // Path was saved to Backup, then deleted
	OpCreated = "created" // Path is about to be created (did not exist)
	OpMkdir   = "mkdir"   // directory Path is about to be created (did not exist)
	OpRmdir   = "rmdir"   // empty directory Path was removed
	OpPlaced  = "placed"  // the new Path is in place
	OpCommit  = "commit"  // the change is complete; nothing is to be undone
)

// File names inside a journal directory.
const (
	LogName      = "undo.jsonl"
	BackupDir    = "b"
	ManifestName = "manifest.json"
)

// Record is one line of the log. Which fields are set depends on Op.
type Record struct {
	Op string `json:"op"`

	// begin
	ID     string    `json:"id,omitempty"`
	Pkg    string    `json:"pkg,omitempty"`
	Action string    `json:"action,omitempty"` // install, update, reinstall, downgrade, rollback, remove
	From   string    `json:"from,omitempty"`   // version before ("" for an install)
	To     string    `json:"to,omitempty"`     // version after ("" for a remove)
	Dir    string    `json:"dir,omitempty"`    // the install directory, absolute
	TS     time.Time `json:"ts,omitzero"`
	// Result is what the engine writes to state.json when the change completes, opaque here. It is
	// in the begin record so that an interrupted change can be finished without the network.
	Result json.RawMessage `json:"result,omitempty"`

	// step
	Name string `json:"name,omitempty"`

	// file operations; Path is "/"-separated and relative to Dir
	Path   string `json:"path,omitempty"`
	Backup string `json:"backup,omitempty"` // "b/0001", relative to the journal directory
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"` // of the backup, checked before it is restored by a rollback
}

// ErrCorrupt means a journal cannot be trusted: a damaged line other than the last, a record
// that breaks the rules, or no begin record. It needs a person, not an automatic repair.
var ErrCorrupt = errors.New("journal is damaged")

// maxSize bounds what Read accepts. A record is a few hundred bytes; an install of the maximum
// 20,000 files writes about four records per file.
const maxSize = 64 << 20

// Journal is an open journal being written.
type Journal struct {
	dir     string
	f       *os.File
	n       int // records written
	backups int
}

// Create starts a journal in dir, which must not exist yet, with begin as its first record.
func Create(dir string, begin Record) (*Journal, error) {
	if begin.Op != OpBegin {
		return nil, fmt.Errorf("journal: first record must be %q, not %q", OpBegin, begin.Op)
	}
	if err := validate(begin, 0); err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	if err := os.Mkdir(filepath.Join(dir, BackupDir), 0o700); err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, LogName), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600) //nolint:gosec // G304: a directory under the state dir
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	j := &Journal{dir: dir, f: f}
	if err := j.Append(begin); err != nil {
		_ = f.Close()
		return nil, err
	}
	syncDir(filepath.Dir(dir))
	syncDir(dir)
	return j, nil
}

// Resume opens an existing journal to add records (finishing an interrupted change forward). A
// torn last line is cut off first so the next record does not run into it.
func Resume(dir string) (*Journal, *Log, error) {
	l, err := Read(dir)
	if err != nil {
		return nil, nil, err
	}
	name := filepath.Join(dir, LogName)
	f, err := os.OpenFile(name, os.O_WRONLY, 0o600) //nolint:gosec // G304: a journal under the state dir
	if err != nil {
		return nil, nil, fmt.Errorf("journal: %w", err)
	}
	if err := f.Truncate(l.validBytes); err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("journal: %w", err)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("journal: %w", err)
	}
	if l.unterminated {
		if _, err := f.Write([]byte("\n")); err != nil {
			_ = f.Close()
			return nil, nil, fmt.Errorf("journal: %w", err)
		}
	}
	j := &Journal{dir: dir, f: f, n: len(l.Records)}
	for _, r := range l.Records {
		if r.Backup != "" {
			j.backups++
		}
	}
	return j, l, nil
}

// Dir is the journal directory.
func (j *Journal) Dir() string { return j.dir }

// Append writes r and flushes it to disk before returning.
func (j *Journal) Append(r Record) error {
	if err := validate(r, j.n); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	b = append(b, '\n')
	if _, err := j.f.Write(b); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	if err := j.f.Sync(); err != nil {
		return fmt.Errorf("journal: flush: %w", err)
	}
	j.n++
	return nil
}

// NextBackup names the next backup file: its journal-relative name (for the record) and its
// absolute path (to write it).
func (j *Journal) NextBackup() (rel, abs string) {
	j.backups++
	rel = fmt.Sprintf("%s/%04d", BackupDir, j.backups)
	return rel, filepath.Join(j.dir, filepath.FromSlash(rel))
}

// Close closes the log file. The journal stays on disk.
func (j *Journal) Close() error { return j.f.Close() }

// Log is a journal as read back.
type Log struct {
	Dir     string
	Records []Record
	// Torn is set when the last line was incomplete (the process died while writing it) and was
	// ignored.
	Torn bool

	validBytes   int64 // the length of the intact records
	unterminated bool  // the last intact record has no newline after it
}

// Begin is the first record.
func (l *Log) Begin() Record { return l.Records[0] }

// Committed reports whether the change completed.
func (l *Log) Committed() bool {
	return l.Records[len(l.Records)-1].Op == OpCommit
}

// HasStep reports whether the step name was recorded.
func (l *Log) HasStep(name string) bool {
	for _, r := range l.Records {
		if r.Op == OpStep && r.Name == name {
			return true
		}
	}
	return false
}

// BackupPath is the absolute path of a record's backup file.
func (l *Log) BackupPath(r Record) string {
	return filepath.Join(l.Dir, filepath.FromSlash(r.Backup))
}

// Exists reports whether dir holds a journal.
func Exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, LogName))
	return err == nil
}

// Read reads the journal in dir. Every record is checked: a path that leaves the install
// directory, a backup outside the journal, or records out of order make the whole journal
// ErrCorrupt, because undo acts on what it says.
func Read(dir string) (*Log, error) {
	name := filepath.Join(dir, LogName)
	f, err := os.Open(name) //nolint:gosec // G304: the journal directory is under GremLoader's state dir
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	if len(data) > maxSize {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrCorrupt, name, maxSize)
	}

	l := &Log{Dir: dir}
	rest := data
	for line := 1; len(rest) > 0; line++ {
		raw, after, terminated := bytes.Cut(rest, []byte("\n"))
		var r Record
		if err := strictUnmarshal(raw, &r); err != nil {
			// Each record is written with its newline in one write, so only a record without one
			// can be a write the process did not live to finish. That record never happened.
			if !terminated {
				l.Torn = true
				break
			}
			return nil, fmt.Errorf("%w: %s line %d: %w", ErrCorrupt, name, line, err)
		}
		if err := validate(r, len(l.Records)); err != nil {
			return nil, fmt.Errorf("%w: %s line %d: %w", ErrCorrupt, name, line, err)
		}
		if len(l.Records) > 0 && l.Records[len(l.Records)-1].Op == OpCommit {
			return nil, fmt.Errorf("%w: %s line %d: record after commit", ErrCorrupt, name, line)
		}
		l.Records = append(l.Records, r)
		l.validBytes = int64(len(data) - len(after))
		l.unterminated = !terminated
		rest = after
	}
	if len(l.Records) == 0 {
		return nil, fmt.Errorf("%w: %s has no begin record", ErrCorrupt, name)
	}
	return l, nil
}

func strictUnmarshal(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

var (
	backupName = regexp.MustCompile(`^b/[0-9]{4,9}$`)
	sha256Hex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	versionish = regexp.MustCompile(`^[0-9A-Za-z.+-]{0,128}$`)
	actions    = map[string]bool{
		"install": true, "update": true, "reinstall": true, "downgrade": true,
		"rollback": true, "remove": true, "repair": true,
	}
)

// validate checks one record; index is its position in the log.
func validate(r Record, index int) error {
	if (index == 0) != (r.Op == OpBegin) {
		if index == 0 {
			return fmt.Errorf("first record is %q, want %q", r.Op, OpBegin)
		}
		return fmt.Errorf("a second %q record", OpBegin)
	}
	switch r.Op {
	case OpBegin:
		switch {
		case !paths.ValidPackageName(r.Pkg):
			return fmt.Errorf("begin: package name %q is not valid", r.Pkg)
		case !actions[r.Action]:
			return fmt.Errorf("begin: unknown action %q", r.Action)
		case !versionish.MatchString(r.From) || !versionish.MatchString(r.To):
			return errors.New("begin: from/to is not a version")
		case !filepath.IsAbs(r.Dir) || filepath.Clean(r.Dir) != r.Dir:
			return fmt.Errorf("begin: install directory %q is not a clean absolute path", r.Dir)
		}
	case OpStep:
		if r.Name == "" || len(r.Name) > 32 {
			return errors.New("step: missing or overlong name")
		}
	case OpBackup, OpRemoved:
		if !backupName.MatchString(r.Backup) {
			return fmt.Errorf("%s %q: backup name %q is not a journal backup", r.Op, r.Path, r.Backup)
		}
		if !sha256Hex.MatchString(r.SHA256) || r.Size < 0 {
			return fmt.Errorf("%s %q: backup hash or size missing", r.Op, r.Path)
		}
		fallthrough
	case OpCreated, OpMkdir, OpRmdir, OpPlaced:
		if !paths.ValidRel(r.Path) {
			return fmt.Errorf("%s: path %q is not a clean relative path", r.Op, r.Path)
		}
	case OpCommit:
	default:
		return fmt.Errorf("unknown op %q", r.Op)
	}
	return nil
}

// syncDir makes a newly created entry durable on Linux (see atomicfile); Windows cannot open a
// directory for flushing.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir) //nolint:gosec // G304: a directory GremLoader just created
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
