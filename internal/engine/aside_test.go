package engine

import (
	"bufio"
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestSleepHelper is not a test: runningCopy starts copies of the test binary with this one
// selected, to stand in for a GremLoader that is running from an install folder.
func TestSleepHelper(t *testing.T) {
	if os.Getenv("GREMLOADER_SLEEP_HELPER") != "1" {
		t.Skip("helper process")
	}
	println("sleeping")
	time.Sleep(time.Hour)
}

// running is a started copy of the test binary.
type running struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (p *running) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// runningCopy puts a copy of the test binary at path (unless one is there already) and starts it,
// returning once it runs. It is killed when the test ends.
func runningCopy(t *testing.T, path string, copyFirst bool) *running {
	t.Helper()
	if copyFirst {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if err := copyOver(self, path); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(path, "-test.run=^TestSleepHelper$") //nolint:gosec // G204: a copy of our own test binary
	cmd.Env = append(os.Environ(), "GREMLOADER_SLEEP_HELPER=1")
	out, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &running{cmd: cmd, done: make(chan struct{})}
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(out).ReadString('\n')
		line <- s
	}()
	go func() { _ = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-p.done })
	select {
	case s := <-line:
		if !strings.HasPrefix(s, "sleeping") {
			t.Fatalf("helper said %q", s)
		}
	case <-time.After(2 * time.Minute): // a new executable's first start is scanned on Windows
		t.Fatal("helper did not start")
	}
	return p
}

func copyOver(src, dst string) error {
	b, err := os.ReadFile(src) //nolint:gosec // G304: the test binary
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o755) //nolint:gosec // G306: an executable
}

// withoutAside drops the names the running binary was moved aside to: they are not part of any
// version, and the next start of GremLoader removes them.
func withoutAside(m map[string]string, exe string) map[string]string {
	out := maps.Clone(m)
	for _, n := range AsideNames(exe) {
		delete(out, n)
	}
	return out
}

// The update of a package whose program is running from its folder, which is what a self-update
// is: the running check leaves our own process out. On Windows the program cannot be replaced, so
// the engine renames it aside first; on Linux the rename over it works as it is.
func TestReplacingTheRunningBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("starts processes")
	}
	r := atV1(t)
	exe := filepath.Join(r.dir(), "app.exe")
	p := runningCopy(t, exe, true)
	running := readString(t, exe)

	// Backups are copies, as when the state folder is on another volume. (A hard link to the running
	// file is enough for Windows to allow replacing it: checked 2026-10-11, Windows 11.)
	osLink = func(string, string) error { return errors.New("another volume") }
	t.Cleanup(func() { osLink = os.Link })

	if runtime.GOOS == "windows" {
		// Without Self: Windows refuses, the update fails and the old version stays.
		it := item(t, r.apply(r.engine(), Request{}), "app")
		if it.Result != ResultFailed || it.Err == nil || !strings.Contains(it.Err.Error(), "app.exe") {
			t.Fatalf("without Self: %s, %v", it.Result, it.Err)
		}
		if readString(t, exe) != running {
			t.Fatal("without Self: the running binary was changed")
		}
		r.assertClean()
	}

	r.self = exe
	it := item(t, r.apply(r.engine(), Request{}), "app")
	if it.Result != ResultUpdated {
		t.Fatalf("with Self: %s, %v", it.Result, it.Err)
	}
	if got := readString(t, exe); got != "app v2" {
		t.Errorf("app.exe holds %d bytes, not the new version", len(got))
	}
	if aside := exe + ".old"; asideRunning != fileExists(aside) {
		t.Errorf("%s exists: %v", aside, fileExists(aside))
	}
	if !p.alive() {
		t.Error("the running program died")
	}
	r.assertClean()
}

// A crash after the running binary was moved aside and before the new one is in place leaves no
// file at its path; repair puts the old one back, and it starts.
func TestCrashAfterMovingAsideIsRepaired(t *testing.T) {
	if testing.Short() {
		t.Skip("starts processes")
	}
	asideRunning = true
	t.Cleanup(func() { asideRunning = runtime.GOOS == "windows" })
	r := atV1(t)
	exe := filepath.Join(r.dir(), "app.exe")
	runningCopy(t, exe, true)
	running := readString(t, exe)

	r.self = exe
	e := r.engine()
	aside := false
	e.fault = func(point string) error {
		switch {
		case point == "aside":
			aside = true
		case aside && point == "fs":
			return errCrash
		}
		return nil
	}
	if err := e.Apply(context.Background(), r.plan(e, Request{}), ApplyOptions{}); !errors.Is(err, errCrash) {
		t.Fatalf("Apply returned %v", err)
	}
	if fileExists(exe) || !fileExists(exe+".old") {
		t.Fatalf("after the crash: app.exe there %v, app.exe.old there %v", fileExists(exe), fileExists(exe+".old"))
	}

	for _, ri := range r.engine().Repair(context.Background()) {
		if ri.Result != ResultRepaired {
			t.Errorf("repair: %+v", ri)
		}
	}
	if readString(t, exe) != running {
		t.Fatal("repair did not put the running binary back")
	}
	if v := r.state().Packages["app"].Version; v != "1.0.0" {
		t.Errorf("state says %s", v)
	}
	r.assertClean()
	runningCopy(t, exe, false)
}

// The crash sweep of TestCrashAtEveryPointIsRepaired, with app.exe as the running binary that is
// moved aside: every outcome is still exactly the old or exactly the new version.
func TestSelfCrashAtEveryPointIsRepaired(t *testing.T) {
	asideRunning = true
	t.Cleanup(func() { asideRunning = runtime.GOOS == "windows" })
	setup := func() *rig {
		r := atV1(t)
		r.self = filepath.Join(r.dir(), "app.exe")
		return r
	}
	r := setup()
	e := r.engine()
	total := 0
	e.fault = func(string) error { total++; return nil }
	if err := e.Apply(context.Background(), r.plan(e, Request{}), ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if !fileExists(r.self + ".old") {
		t.Fatal("the counting run did not move app.exe aside")
	}

	sawAside, sawOld, sawNew := false, false, false
	for k := 1; k <= total; k++ {
		r := setup()
		e := r.engine()
		n, where := 0, ""
		e.fault = func(point string) error {
			if n++; n == k {
				where = point
				return errCrash
			}
			return nil
		}
		if err := e.Apply(context.Background(), r.plan(e, Request{}), ApplyOptions{}); !errors.Is(err, errCrash) {
			t.Fatalf("crash %d: Apply returned %v", k, err)
		}
		sawAside = sawAside || where == "aside"
		for _, ri := range r.engine().Repair(context.Background()) {
			if ri.Result != ResultRepaired {
				t.Errorf("crash %d (%s): repair %+v", k, where, ri)
			}
		}
		got := withoutAside(tree(t, r.dir()), "app.exe")
		v := r.state().Packages["app"].Version
		switch {
		case maps.Equal(got, v1Tree) && v == "1.0.0":
			sawOld = true
		case maps.Equal(got, v2Tree) && v == "2.0.0":
			sawNew = true
		default:
			t.Fatalf("crash %d (%s): state says %s and the folder holds:\n%s", k, where, v, show(got))
		}
		r.assertClean()
	}
	if !sawAside || !sawOld || !sawNew {
		t.Errorf("saw aside=%v old=%v new=%v", sawAside, sawOld, sawNew)
	}
}
