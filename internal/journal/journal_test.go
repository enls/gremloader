package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const h = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func begin(t testing.TB) Record {
	t.Helper()
	return Record{
		Op: OpBegin, ID: "r1", Pkg: "photocraft", Action: "update", From: "0.5.0", To: "0.6.0",
		Dir: filepath.Join(t.TempDir(), "Photocraft"), TS: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC),
		Result: json.RawMessage(`{"version":"0.6.0"}`),
	}
}

func create(t testing.TB) (*Journal, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "journal", "photocraft")
	j, err := Create(dir, begin(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j, dir
}

func appendAll(t *testing.T, j *Journal, rs ...Record) {
	t.Helper()
	for _, r := range rs {
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
}

func logFile(dir string) string { return filepath.Join(dir, LogName) }

func TestRoundTrip(t *testing.T) {
	j, dir := create(t)
	rel, abs := j.NextBackup()
	if rel != "b/0001" || abs != filepath.Join(dir, "b", "0001") {
		t.Errorf("NextBackup = %q, %q", rel, abs)
	}
	appendAll(t, j,
		Record{Op: OpStep, Name: "download"},
		Record{Op: OpBackup, Path: "app.exe", Backup: rel, Size: 3, SHA256: h},
		Record{Op: OpMkdir, Path: "sub"},
		Record{Op: OpCreated, Path: "sub/new.dll"},
		Record{Op: OpPlaced, Path: "sub/new.dll"},
		Record{Op: OpRemoved, Path: "old.dll", Backup: "b/0002", Size: 0, SHA256: h},
		Record{Op: OpRmdir, Path: "gone"},
		Record{Op: OpStep, Name: "state"},
		Record{Op: OpCommit},
	)
	l, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Records) != 10 || l.Torn || !l.Committed() || !l.HasStep("state") || l.HasStep("verify") {
		t.Fatalf("log = %+v", l)
	}
	b := l.Begin()
	if b.Pkg != "photocraft" || b.To != "0.6.0" || string(b.Result) != `{"version":"0.6.0"}` || b.TS.IsZero() {
		t.Errorf("begin = %+v", b)
	}
	if got := l.BackupPath(l.Records[2]); got != abs {
		t.Errorf("BackupPath = %q", got)
	}
	if fi, err := os.Stat(filepath.Join(dir, BackupDir)); err != nil || !fi.IsDir() {
		t.Errorf("backup directory missing: %v", err)
	}
	if !Exists(dir) || Exists(t.TempDir()) {
		t.Error("Exists")
	}
}

// An abort is what tells repair to undo a change that got as far as its state step.
func TestAbortIsRead(t *testing.T) {
	j, dir := create(t)
	appendAll(t, j, Record{Op: OpCreated, Path: "a.txt"}, Record{Op: OpStep, Name: "state"})
	l, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if l.Aborted() {
		t.Error("aborted before any abort was written")
	}
	appendAll(t, j, Record{Op: OpAbort})
	if l, err = Read(dir); err != nil {
		t.Fatal(err)
	}
	if !l.Aborted() || l.Committed() || !l.HasStep("state") {
		t.Errorf("log = %+v", l)
	}
}

func TestCreateRefusesAnExistingJournal(t *testing.T) {
	_, dir := create(t)
	if _, err := Create(dir, begin(t)); err == nil {
		t.Fatal("a second journal was started over the first")
	}
}

// The process can die in the middle of writing a line. That record never happened; everything
// before it stands, and a resumed journal continues on a clean line.
func TestTornLastLineIsIgnoredAndResumeContinuesCleanly(t *testing.T) {
	j, dir := create(t)
	appendAll(t, j, Record{Op: OpStep, Name: "download"})
	_ = j.Close()
	f, err := os.OpenFile(logFile(dir), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"op":"created","pa`)
	_ = f.Close()

	l, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Torn || len(l.Records) != 2 {
		t.Fatalf("torn=%v records=%d", l.Torn, len(l.Records))
	}

	j2, l2, err := Resume(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(l2.Records) != 2 {
		t.Fatalf("resume read %d records", len(l2.Records))
	}
	appendAll(t, j2, Record{Op: OpStep, Name: "state"}, Record{Op: OpCommit})
	_ = j2.Close()
	l3, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if l3.Torn || len(l3.Records) != 4 || !l3.Committed() {
		t.Fatalf("after resume: torn=%v records=%+v", l3.Torn, l3.Records)
	}
}

// A complete record that lost only its newline is kept, and the next record must not be glued
// onto it.
func TestCompleteRecordWithoutNewline(t *testing.T) {
	j, dir := create(t)
	appendAll(t, j, Record{Op: OpStep, Name: "download"})
	_ = j.Close()
	data, _ := os.ReadFile(logFile(dir))
	if err := os.WriteFile(logFile(dir), []byte(strings.TrimSuffix(string(data), "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	j2, l, err := Resume(dir)
	if err != nil {
		t.Fatal(err)
	}
	if l.Torn || len(l.Records) != 2 {
		t.Fatalf("torn=%v records=%d", l.Torn, len(l.Records))
	}
	appendAll(t, j2, Record{Op: OpCommit})
	_ = j2.Close()
	l2, err := Read(dir)
	if err != nil || len(l2.Records) != 3 || !l2.Committed() {
		t.Fatalf("records=%v err=%v", l2, err)
	}
}

// Damage anywhere but the last line is not a crash signature: undo must not act on it.
func TestDamageIsCorrupt(t *testing.T) {
	good := func(t *testing.T) (string, string) {
		j, dir := create(t)
		appendAll(t, j, Record{Op: OpStep, Name: "download"}, Record{Op: OpCreated, Path: "a.txt"})
		_ = j.Close()
		data, _ := os.ReadFile(logFile(dir))
		return dir, string(data)
	}
	for _, c := range []struct {
		name string
		edit func(string) string
	}{
		{"damaged middle line", func(s string) string { return strings.Replace(s, `{"op":"step"`, `{"op":"st`, 1) }},
		{"blank line in the middle", func(s string) string { return strings.Replace(s, "\n", "\n\n", 1) }},
		{"damaged last line with its newline", func(s string) string { return strings.Replace(s, `"a.txt"}`, `"a.tx`, 1) }},
		{"path leaves the directory", func(s string) string { return strings.Replace(s, `"a.txt"`, `"../a.txt"`, 1) }},
		{"absolute path", func(s string) string { return strings.Replace(s, `"a.txt"`, `"/etc/passwd"`, 1) }},
		{"backslash path", func(s string) string { return strings.Replace(s, `"a.txt"`, `"sub\\a.txt"`, 1) }},
		{"device name", func(s string) string { return strings.Replace(s, `"a.txt"`, `"nul.txt"`, 1) }},
		{"unknown op", func(s string) string { return strings.Replace(s, `"created"`, `"chmod"`, 1) }},
		{"unknown field", func(s string) string { return strings.Replace(s, `"path"`, `"mode":1,"path"`, 1) }},
		{"no begin", func(s string) string { _, rest, _ := strings.Cut(s, "\n"); return rest }},
		{"empty", func(string) string { return "" }},
		{"two begins", func(s string) string { first, _, _ := strings.Cut(s, "\n"); return s + first + "\n" }},
		{"record after commit", func(s string) string { return s + `{"op":"commit"}` + "\n" + `{"op":"created","path":"b"}` + "\n" }},
		{"commit after abort", func(s string) string { return s + `{"op":"abort"}` + "\n" + `{"op":"commit"}` + "\n" }},
		{"backup outside the journal", func(s string) string {
			return s + `{"op":"backup","path":"x","backup":"../../x","size":1,"sha256":"` + h + `"}` + "\n"
		}},
		{"backup without hash", func(s string) string { return s + `{"op":"backup","path":"x","backup":"b/0001","size":1}` + "\n" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, data := good(t)
			if err := os.WriteFile(logFile(dir), []byte(c.edit(data)), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(dir); !errors.Is(err, ErrCorrupt) {
				t.Errorf("err = %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestBeginIsChecked(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*Record)
	}{
		{"not a begin", func(r *Record) { r.Op = OpStep }},
		{"bad package", func(r *Record) { r.Pkg = "../x" }},
		{"bad action", func(r *Record) { r.Action = "explode" }},
		{"relative dir", func(r *Record) { r.Dir = "Photocraft" }},
		{"unclean dir", func(r *Record) { r.Dir += string(filepath.Separator) + ".." }},
		{"bad version", func(r *Record) { r.To = "1.0/../x" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := begin(t)
			c.edit(&b)
			if _, err := Create(filepath.Join(t.TempDir(), "j"), b); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestAppendChecksRecords(t *testing.T) {
	j, _ := create(t)
	for _, r := range []Record{
		{Op: OpBegin, Pkg: "x", Action: "update", Dir: t.TempDir()},
		{Op: OpCreated, Path: "../x"},
		{Op: OpBackup, Path: "x", Backup: "b/1"},
		{Op: "chmod", Path: "x"},
	} {
		if err := j.Append(r); err == nil {
			t.Errorf("Append(%+v) accepted", r)
		}
	}
}

// Backup numbering continues after a resume, so a resumed change never overwrites a backup.
func TestResumeContinuesBackupNumbering(t *testing.T) {
	j, dir := create(t)
	rel, _ := j.NextBackup()
	appendAll(t, j, Record{Op: OpBackup, Path: "a", Backup: rel, Size: 1, SHA256: h})
	_ = j.Close()
	j2, _, err := Resume(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j2.Close() }()
	if rel2, _ := j2.NextBackup(); rel2 != "b/0002" {
		t.Errorf("next backup after resume = %q, want b/0002", rel2)
	}
}

func TestOversizedJournalIsRefused(t *testing.T) {
	j, dir := create(t)
	_ = j.Close()
	if err := os.Truncate(logFile(dir), maxSize+1); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); !errors.Is(err, ErrCorrupt) {
		t.Errorf("err = %v, want ErrCorrupt", err)
	}
}

func TestBegun(t *testing.T) {
	j, dir := create(t)
	_ = j.Close()
	full, err := os.ReadFile(logFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		log  []byte // nil: no log file
		want bool
	}{
		{"no log", nil, false},
		{"empty log", []byte{}, false},
		{"torn begin", full[:len(full)/2], false},
		{"begin without its newline", full[:len(full)-1], false},
		{"complete begin", full, true},
		{"complete begin and a torn record", append(full, `{"op":"cre`...), true},
		{"a long first line", append(bytes.Repeat([]byte("x"), 10000), '\n'), true},
	} {
		d := filepath.Join(t.TempDir(), "j")
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if c.log != nil {
			if err := os.WriteFile(logFile(d), c.log, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if got := Begun(d); got != c.want {
			t.Errorf("%s: Begun = %v", c.name, got)
		}
	}
	if Begun(filepath.Join(t.TempDir(), "missing")) {
		t.Error("a missing directory has begun")
	}
}
