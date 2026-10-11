package runlock

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const helperEnv = "GREMLOADER_RUNLOCK_HELPER"

// TestMain doubles as the helper process of the two-process tests: with helperEnv set, the test
// binary takes the lock, says so on stdout and holds it until stdin closes.
func TestMain(m *testing.M) {
	if path := os.Getenv(helperEnv); path != "" {
		os.Exit(holdLock(path))
	}
	os.Exit(m.Run())
}

func holdLock(path string) int {
	l, err := Acquire(path, Holder{PID: os.Getpid(), Version: "helper", Since: time.Now()})
	if err != nil {
		fmt.Println("error:", err)
		return 1
	}
	fmt.Println("held")
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := l.Release(); err != nil {
		fmt.Println("error:", err)
		return 1
	}
	return 0
}

type helper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

func startHelper(t *testing.T, path string) *helper {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // G204: re-runs this test binary
	cmd.Env = append(os.Environ(), helperEnv+"="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "held" {
		t.Fatalf("helper did not take the lock: %q, %v", line, err)
	}
	return &helper{cmd: cmd, stdin: stdin}
}

func TestSecondProcessIsTurnedAwayAndToldWho(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	h := startHelper(t, path)

	_, err := Acquire(path, Holder{PID: os.Getpid()})
	var le *LockedError
	if !errors.As(err, &le) || !errors.Is(err, ErrLocked) {
		t.Fatalf("Acquire while another process holds it = %v, want *LockedError", err)
	}
	if le.Holder == nil || le.Holder.PID != h.cmd.Process.Pid || le.Holder.Version != "helper" {
		t.Errorf("Holder = %+v, want the helper (PID %d)", le.Holder, h.cmd.Process.Pid)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("PID %d", h.cmd.Process.Pid)) {
		t.Errorf("message does not name the holder: %v", err)
	}

	// A clean release by the holder frees it.
	_ = h.stdin.Close()
	if err := h.cmd.Wait(); err != nil {
		t.Fatalf("helper: %v", err)
	}
	l, err := Acquire(path, Holder{PID: os.Getpid()})
	if err != nil {
		t.Fatalf("Acquire after the holder released: %v", err)
	}
	_ = l.Release()
}

// The reason the lock is a kernel lock: a holder that dies without cleaning up (crash, kill,
// power button on the scheduler's run) must not leave the next run locked out.
func TestLockIsFreedWhenTheHolderIsKilled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	h := startHelper(t, path)
	if _, err := Acquire(path, Holder{PID: os.Getpid()}); !errors.Is(err, ErrLocked) {
		t.Fatalf("Acquire while held = %v, want ErrLocked", err)
	}
	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = h.cmd.Wait()

	// Windows documents that a dead process's locks are released "depending on available system
	// resources", i.e. not necessarily by the time Wait returns. Allow a few seconds.
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err := Acquire(path, Holder{PID: os.Getpid()})
		if err == nil {
			_ = l.Release()
			return
		}
		if !errors.Is(err, ErrLocked) || time.Now().After(deadline) {
			t.Fatalf("Acquire after the holder was killed: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestTwoAcquiresInOneProcessConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	l, err := Acquire(path, Holder{PID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path, Holder{PID: 2}); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Acquire = %v, want ErrLocked", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("second Release = %v, want nil", err)
	}
	l2, err := Acquire(path, Holder{PID: 2})
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	_ = l2.Release()
}

func TestAcquireCreatesTheStateDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "deeper", "run.lock")
	l, err := Acquire(path, Holder{PID: 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Release()
}

// A run that dies leaves its description behind; it must not make the next run think it is
// locked out (the lock, not the file, decides).
func TestStaleDescriptionDoesNotLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	if err := os.WriteFile(path, []byte(`{"pid":99999,"version":"old","since":"2020-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Acquire(path, Holder{PID: 1})
	if err != nil {
		t.Fatalf("Acquire over a stale description: %v", err)
	}
	_ = l.Release()
}

func TestUnreadableDescriptionStillReportsLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	l, err := Acquire(path, Holder{PID: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Acquire(path, Holder{PID: 2})
	var le *LockedError
	if !errors.As(err, &le) || le.Holder != nil {
		t.Fatalf("Acquire = %v (holder %+v), want *LockedError without a holder", err, le)
	}
}

func TestHolderTextIsMadePrintable(t *testing.T) {
	rlo := string(rune(0x202e)) // RIGHT-TO-LEFT OVERRIDE
	e := &LockedError{Path: "run.lock", Holder: &Holder{PID: 7, Version: "1.0\x1b[2J" + rlo + strings.Repeat("x", 200)}}
	msg := e.Error()
	if strings.ContainsAny(msg, "\x1b"+rlo) {
		t.Errorf("control characters reached the message: %q", msg)
	}
	if strings.Count(msg, "x") > 64 {
		t.Errorf("version not shortened: %q", msg)
	}
}
