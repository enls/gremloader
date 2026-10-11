package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/exitcode"
)

// Bug check of M5, suspicion 9: only a failure to reach the network is "offline" (exit 7). Local
// errors also have a Timeout method (fs.PathError, syscall.Errno) and must stay ordinary failures.
func TestOfflineIsOnlyTheNetwork(t *testing.T) {
	_, missing := os.Open(filepath.Join(t.TempDir(), "missing"))
	mkdirFile := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(mkdirFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	notDir := os.MkdirAll(filepath.Join(mkdirFile, "sub"), 0o700)

	// A port that was listening a moment ago and is now closed: connection refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	refused := get(http.DefaultClient, "http://"+addr+"/")
	_, dns := net.DefaultResolver.LookupHost(context.Background(), "gremloader-test.invalid")
	slow := &http.Client{Timeout: time.Nanosecond}
	timeout := get(slow, "http://"+addr+"/")

	for _, c := range []struct {
		name    string
		err     error
		offline bool
	}{
		{"file not found", missing, false},
		{"mkdir under a file", notDir, false},
		{"wrapped local error", fmt.Errorf("staging: %w", missing), false},
		{"connection refused", refused, true},
		{"unknown host", dns, true},
		{"timeout", timeout, true},
		{"wrapped network error", fmt.Errorf("listing releases: %w", refused), true},
		{"plain error", errors.New("boom"), false},
	} {
		if c.err == nil {
			t.Fatalf("%s: no error was produced", c.name)
		}
		if got := isOffline(c.err); got != c.offline {
			t.Errorf("%s: isOffline(%v) = %v, want %v", c.name, c.err, got, c.offline)
		}
	}
}

// A local failure in a one-package run (the common case) must exit 1, not 7.
func TestLocalFailureIsNotReportedAsOffline(t *testing.T) {
	r := atV1(t)
	e := r.engine()
	staging := filepath.Dir(e.cfg.Paths.Staging("app"))
	if err := os.RemoveAll(staging); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := r.plan(e, Request{})
	if err := e.Apply(context.Background(), p, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	it := item(t, p, "app")
	if it.Result != ResultFailed {
		t.Fatalf("item = %+v", it)
	}
	if got := p.ExitCode(false); got != exitcode.Failed {
		t.Errorf("exit = %d, want %d (%v)", got, exitcode.Failed, it.Err)
	}
	assertTree(t, r.dir(), v1Tree)
}

// get returns only the error of a request.
func get(c *http.Client, u string) error {
	resp, err := c.Get(u) //nolint:noctx // a test of the error only
	if resp != nil {
		_ = resp.Body.Close()
	}
	return err
}
