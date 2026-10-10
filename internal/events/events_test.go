package events

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/enls/gremloader/internal/clock"
)

var t0 = time.Date(2026, 10, 11, 10, 0, 0, 0, time.FixedZone("CDT", -5*3600))

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestEmitWritesOneJSONObjectPerLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "events.jsonl") // folder is created
	clk := clock.NewFake(t0)
	l, err := Open(path, WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()

	if err := l.Emit(Event{Event: "apply.begin", Package: "photocraft", From: "0.5.0", To: "0.6.0"}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Second)
	if err := l.Emit(Event{Level: Warn, Event: "verify.fail", Package: "photocraft", Code: "checksum_mismatch", Msg: "boom"}); err != nil {
		t.Fatal(err)
	}

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	var first, second map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if first["ts"] != "2026-10-11T15:00:00Z" {
		t.Errorf("ts = %v, want UTC 15:00:00Z", first["ts"])
	}
	if first["level"] != "info" || first["event"] != "apply.begin" || first["from"] != "0.5.0" {
		t.Errorf("first = %v", first)
	}
	if _, has := first["msg"]; has {
		t.Error("empty fields must be omitted")
	}
	if second["ts"] != "2026-10-11T15:00:02Z" || second["level"] != "warn" || second["code"] != "checksum_mismatch" {
		t.Errorf("second = %v", second)
	}
}

func TestSecretsNeverReachTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	signed := "https://release-assets.githubusercontent.com/x/y?sig=TOPSECRET&jwt=eyJALSOSECRET"
	if err := l.Emit(Event{Event: "download.retry", URL: signed, Msg: `Get "` + signed + `": EOF`}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	for _, secret := range []string{"TOPSECRET", "ALSOSECRET", "sig=", "jwt="} {
		if bytes.Contains(b, []byte(secret)) {
			t.Errorf("log contains %q:\n%s", secret, b)
		}
	}
	if !bytes.Contains(b, []byte("release-assets.githubusercontent.com/x/y")) {
		t.Errorf("the redacted URL itself should remain:\n%s", b)
	}
}

func TestRotationKeepsNewestFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := Open(path, WithMaxSize(300), WithKeep(2))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	for i := 0; i < 30; i++ {
		if err := l.Emit(Event{Event: "tick", Msg: strings.Repeat("x", 40), Bytes: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("no .1: %v", err)
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Errorf("no .2: %v", err)
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error(".3 exists but keep=2")
	}
	// No file may exceed the threshold by more than one line.
	for _, p := range []string{path, path + ".1", path + ".2"} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() > 300+200 {
			t.Errorf("%s is %d bytes", filepath.Base(p), st.Size())
		}
	}
	// Newest event is in the live file.
	live, _, err := scanFile(path)
	if err != nil || len(live) == 0 || live[len(live)-1].Bytes != 29 {
		t.Errorf("live file tail = %+v, %v", live, err)
	}
}

func scanFile(path string) ([]Event, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	return Scan(f)
}

func TestReopenAppendsAndCountsExistingSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	for i := 0; i < 2; i++ {
		l, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Emit(Event{Event: "run"}); err != nil {
			t.Fatal(err)
		}
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(readLines(t, path)); n != 2 {
		t.Errorf("lines = %d, want 2", n)
	}
}

func TestConcurrentEmitKeepsLinesIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = l.Emit(Event{Event: "tick", Msg: strings.Repeat("y", 100)})
			}
		}()
	}
	wg.Wait()
	evs, skipped, err := scanFile(path)
	if err != nil || skipped != 0 || len(evs) != 800 {
		t.Errorf("events=%d skipped=%d err=%v, want 800/0/nil", len(evs), skipped, err)
	}
}

func TestScanSkipsTornAndForeignLines(t *testing.T) {
	in := `{"ts":"2026-10-11T15:00:00Z","level":"info","event":"a"}` + "\n" +
		"not json\n" +
		`{"ts":"2026-10-11T15:00:01Z","level":"info"}` + "\n" + // no event name
		"\n" +
		`{"ts":"2026-10-11T15:00:02Z","level":"info","event":"b"}` + "\n" +
		`{"ts":"2026-10-11T15:00:03Z","level":"inf` // torn
	evs, skipped, err := Scan(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || skipped != 3 {
		t.Errorf("events=%d skipped=%d, want 2/3", len(evs), skipped)
	}
}

func TestEmitAfterClose(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "e.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Emit(Event{Event: "late"}); err == nil {
		t.Error("Emit after Close must report an error")
	}
	if err := l.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// A crash can leave the last line without its newline. The next event must not be glued onto it:
// that would turn two events into one unreadable line and lose the first event after every crash.
func TestEmitAfterATornTailStartsANewLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	torn := `{"ts":"2026-10-11T15:00:00Z","level":"info","event":"first"}` + "\n" +
		`{"ts":"2026-10-11T15:00:01Z","level":"info","ev`
	if err := os.WriteFile(path, []byte(torn), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path, WithClock(clock.NewFake(t0)))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Emit(Event{Event: "after"}); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	evs, skipped, err := Scan(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Event != "first" || evs[1].Event != "after" || skipped != 1 {
		t.Errorf("events = %+v, skipped %d; want first and after, with the torn line skipped", evs, skipped)
	}
}

// One enormous message must not make the log unreadable: Emit bounds what it writes, and Scan
// skips a line it cannot hold instead of stopping there.
func TestOversizedMessageDoesNotHideLaterEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := Open(path, WithClock(clock.NewFake(t0)))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Emit(Event{Event: "big", Msg: strings.Repeat("x", 3<<20)}); err != nil {
		t.Fatal(err)
	}
	if err := l.Emit(Event{Event: "small"}); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	evs, _, err := Scan(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(evs) != 2 || evs[1].Event != "small" {
		t.Errorf("events = %d, want big (shortened) and small", len(evs))
	}

	// A log written elsewhere (or by an older build) can still contain such a line.
	huge := strings.Repeat("y", 2<<20)
	in := `{"ts":"2026-10-11T15:00:00Z","event":"a"}` + "\n" + huge + "\n" + `{"ts":"2026-10-11T15:00:00Z","event":"b"}` + "\n"
	evs, skipped, err := Scan(strings.NewReader(in))
	if err != nil || len(evs) != 2 || skipped != 1 {
		t.Errorf("Scan of an over-long line: %d events, skipped %d, err %v", len(evs), skipped, err)
	}
}

func TestShortenCutsOnACharacterBoundary(t *testing.T) {
	s := strings.Repeat("é", 100) // two bytes each
	got := shorten(s, 51)
	if len(got) > 51 || !utf8.ValidString(got) || !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("shorten = %q (%d bytes)", got, len(got))
	}
	if shorten("short", 51) != "short" {
		t.Error("a short string was changed")
	}
}

// A failed reopen after rotation must not disable the log for the rest of the process.
func TestEmitRecoversWhenTheFileWasNotReopened(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := Open(path, WithClock(clock.NewFake(t0)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	// What rotate leaves behind when its reopen fails (a scanner holding the file, say).
	_ = l.f.Close()
	l.f = nil

	if err := l.Emit(Event{Event: "later"}); err != nil {
		t.Fatalf("Emit after a failed reopen: %v", err)
	}
	if got := readLines(t, path); len(got) != 1 {
		t.Errorf("lines = %v", got)
	}
}

// Result is free text as well (callers put err.Error() there), so a signed link in it must be
// redacted like one in Msg.
func TestSecretsInResultNeverReachTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := Open(path, WithClock(clock.NewFake(t0)))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Emit(Event{Event: "download", Result: "failed: GET https://cdn.example/a?sig=SECRETSIG&jwt=SECRETJWT: EOF"}); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, "SECRET") || !strings.Contains(s, "https://cdn.example/a") {
		t.Errorf("log line = %s", s)
	}
}
