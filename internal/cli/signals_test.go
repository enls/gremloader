package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/testutil/archives"
)

// syncBuffer is a bytes.Buffer safe for the watcher goroutine and the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestFirstInterruptCancelsSecondQuits(t *testing.T) {
	sig := make(chan os.Signal, 2)
	var stderr syncBuffer
	exited := make(chan int, 1)
	ctx, stop := interruptible(sig, &stderr, func(code int) { exited <- code })
	defer stop()

	sig <- os.Interrupt
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the first interrupt did not cancel the run")
	}
	if !strings.Contains(stderr.String(), "interrupt again to quit at once") {
		t.Errorf("stderr = %q", stderr.String())
	}
	select {
	case code := <-exited:
		t.Fatalf("quit after one interrupt (exit %d)", code)
	case <-time.After(100 * time.Millisecond):
	}

	sig <- os.Interrupt
	select {
	case code := <-exited:
		if code != exitcode.Failed || !strings.Contains(stderr.String(), "gremloader repair") {
			t.Errorf("exit %d, stderr %q", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second interrupt did not quit")
	}
}

func TestStopEndsTheWatchWithoutCancelling(t *testing.T) {
	sig := make(chan os.Signal, 1)
	ctx, stop := interruptible(sig, &syncBuffer{}, func(int) { t.Error("exit called") })
	if ctx.Err() != nil {
		t.Fatal("cancelled before anything happened")
	}
	stop()
	sig <- os.Interrupt // nobody listens any more: nothing happens
	time.Sleep(50 * time.Millisecond)
}

// An interrupted run changes nothing it has not finished: here, before the first package.
func TestInterruptedRunChangesNothing(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := r.deps()
	d.ctx = ctx
	var out, errb bytes.Buffer
	code := runWith([]string{"install", "app", "--yes", "--root", r.root}, IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb}, d)
	if code != exitcode.Failed || r.appExe() != "" {
		t.Errorf("exit %d, app %q\n%s%s", code, r.appExe(), out.String(), errb.String())
	}
	// The next, uninterrupted run is not blocked by anything the interrupted one left.
	if code, out, errOut := r.run("install", "app", "--yes"); code != exitcode.OK || r.appExe() != "1" {
		t.Errorf("next run: exit %d\n%s%s", code, out, errOut)
	}
}
