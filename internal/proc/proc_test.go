package proc

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// TestHelperProcess is not a test: the tests below run a copy of this test binary as a stand-in
// for an installed app. It waits until its stdin closes.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GREMLOADER_PROC_HELPER") != "1" {
		t.Skip("helper process")
	}
	_, _ = os.Stdout.WriteString("ready\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// startApp copies the test binary into dir and runs it from there.
func startApp(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self) //nolint:gosec // G304: the test's own binary
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "app")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if err := os.WriteFile(exe, data, 0o755); err != nil { //nolint:gosec // G306: it must be executable
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHelperProcess$") //nolint:gosec // G204: our own copy
	cmd.Env = append(os.Environ(), "GREMLOADER_PROC_HELPER=1")
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
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "ready\n" {
		t.Fatalf("helper said %q", line)
	}
	return cmd
}

func pids(ps []Process) []int {
	var out []int
	for _, p := range ps {
		out = append(out, p.PID)
	}
	return out
}

func TestUnderFindsAProgramRunningFromTheDirectory(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("process paths are only read on Windows and Linux")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "O'Neil's Apps", "Photo Craft")
	cmd := startApp(t, filepath.Join(dir, "bin"))
	pid := cmd.Process.Pid

	found, err := Under(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pids(found), pid) {
		t.Fatalf("Under(%s) = %v, want pid %d among them", dir, found, pid)
	}
	for _, p := range found {
		if p.PID == pid && !strings.HasSuffix(strings.ToLower(p.Path), strings.ToLower(filepath.Join("bin", filepath.Base(cmd.Path)))) {
			t.Errorf("path = %q", p.Path)
		}
	}

	// A sibling folder whose name starts the same is a different install.
	if got, _ := Under(filepath.Join(base, "O'Neil's Apps", "Photo Cr")); slices.Contains(pids(got), pid) {
		t.Error("matched a folder that only shares a name prefix")
	}
	if got, _ := Under(t.TempDir()); slices.Contains(pids(got), pid) {
		t.Error("matched an unrelated folder")
	}
	if runtime.GOOS == "windows" {
		if got, _ := Under(strings.ToUpper(dir)); !slices.Contains(pids(got), pid) {
			t.Error("Windows paths must match without regard to case")
		}
	}

	// Once it has exited it is no longer reported.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if got, _ := Under(dir); slices.Contains(pids(got), pid) {
		t.Error("still reported after exit")
	}
}

func TestUnderNeverReportsItself(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Under(filepath.Dir(self))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(pids(got), os.Getpid()) {
		t.Error("the calling process was reported")
	}
}

func TestInside(t *testing.T) {
	sep := string(filepath.Separator)
	d := filepath.Join(sep+"x", "Apps", "Photo")
	for _, c := range []struct {
		file string
		fold bool
		want bool
	}{
		{filepath.Join(d, "a.exe"), false, true},
		{filepath.Join(d, "sub", "a.exe"), false, true},
		{d, false, false},
		{d + "2" + sep + "a.exe", false, false},
		{filepath.Join(sep+"x", "Apps", "a.exe"), false, false},
		{strings.ToUpper(filepath.Join(d, "a.exe")), false, false},
		{strings.ToUpper(filepath.Join(d, "a.exe")), true, true},
	} {
		if got := inside(d, c.file, c.fold); got != c.want {
			t.Errorf("inside(%q, %q, fold=%v) = %v, want %v", d, c.file, c.fold, got, c.want)
		}
	}
	if !inside(d+sep, filepath.Join(d, "a.exe"), false) {
		t.Error("a trailing separator on dir must not matter")
	}
}
