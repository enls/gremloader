package journal

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// FuzzRead: Read is the parser undo trusts. Whatever the bytes, it does not panic, and a journal
// it accepts is one undo may act on. It fuzzes parse, which is Read without the file, for speed.
func FuzzRead(f *testing.F) {
	j, seedDir := create(f)
	for _, r := range []Record{
		{Op: OpStep, Name: "download"},
		{Op: OpCreated, Path: "a.txt"},
		{Op: OpBackup, Path: "b/c.dll", Backup: "b/0001", Size: 3, SHA256: h},
		{Op: OpStep, Name: "state"},
	} {
		if err := j.Append(r); err != nil {
			f.Fatal(err)
		}
	}
	_ = j.Close()
	good, err := os.ReadFile(logFile(seedDir))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(good)
	f.Add(append(bytes.Clone(good), `{"op":"abort"}`+"\n"...))
	f.Add(append(bytes.Clone(good), `{"op":"commit"}`+"\n"...))
	f.Add(append(bytes.Clone(good), `{"op":"rem`...))
	f.Add(good[:len(good)-1])
	f.Add([]byte{})

	dir := filepath.Join(f.TempDir(), "journal", "app")
	f.Fuzz(func(t *testing.T, data []byte) {
		l, err := parse(dir, LogName, data)
		if err != nil {
			return
		}
		if len(l.Records) == 0 || l.Records[0].Op != OpBegin {
			t.Fatalf("accepted without a begin record: %+v", l.Records)
		}
		if l.Torn && len(data) > 0 && data[len(data)-1] == '\n' {
			t.Fatal("torn although the last line is complete")
		}
		aborted := false
		for i, r := range l.Records {
			if err := validate(r, i); err != nil {
				t.Fatalf("record %d accepted but invalid: %v", i, err)
			}
			if r.Op == OpCommit && (aborted || i != len(l.Records)-1) {
				t.Fatalf("commit at %d of %d (aborted %v)", i, len(l.Records), aborted)
			}
			aborted = aborted || r.Op == OpAbort
			if r.Backup != "" {
				rel, err := filepath.Rel(dir, l.BackupPath(r))
				if err != nil || !filepath.IsLocal(rel) {
					t.Fatalf("backup %q leaves the journal", r.Backup)
				}
			}
		}
	})
}
