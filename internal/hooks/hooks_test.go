package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/engine"
	"github.com/enls/gremloader/internal/events"
)

// The test binary doubles as the hook: with GLHOOKTEST=1 it runs helper() instead of the tests.
func TestMain(m *testing.M) {
	if os.Getenv("GLHOOKTEST") == "1" {
		os.Exit(helper(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type report struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
	Cwd  string            `json:"cwd"`
}

func helper(args []string) int {
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "echo": // report what the hook got
		cwd, _ := os.Getwd()
		r := report{Args: args[1:], Env: map[string]string{}, Cwd: cwd}
		for _, kv := range os.Environ() {
			if k, v, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GREMLOADER_") {
				r.Env[k] = v
			}
		}
		_ = json.NewEncoder(os.Stdout).Encode(r)
		return 0
	case "fail":
		fmt.Fprintln(os.Stderr, "boom")
		return 3
	case "touch":
		_ = os.WriteFile(args[1], nil, 0o600)
		return 0
	case "loud": // a lot of output, then a marker, then fail
		chunk := strings.Repeat("x", 1023) + "\n"
		for range 300 {
			fmt.Print(chunk)
		}
		fmt.Println("END")
		return 1
	case "sleep":
		time.Sleep(time.Minute)
		return 0
	case "spawn": // start a grandchild that writes a heartbeat, then sleep
		secs := "60"
		if args[2] == "exit" {
			secs = "2" // short: it is left running, and holds the copied executable while it runs
		}
		gc := exec.Command(os.Args[0], "beat", args[1], secs)
		if args[2] == "stay" {
			gc.Stdout = os.Stdout // holds the hook's output pipe open too
		}
		if err := gc.Start(); err != nil {
			return 4
		}
		if args[2] == "exit" {
			_ = gc.Process.Release()
			return 0
		}
		time.Sleep(time.Minute)
		return 0
	case "beat": // append a byte every 50 ms for N seconds
		var secs int
		_, _ = fmt.Sscan(args[2], &secs)
		end := time.Now().Add(time.Duration(secs) * time.Second)
		for time.Now().Before(end) {
			f, err := os.OpenFile(args[1], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				_, _ = f.Write([]byte{'.'})
				_ = f.Close()
			}
			time.Sleep(50 * time.Millisecond)
		}
		return 0
	}
	return 2
}

// hookExe copies the test binary to a folder and a name with an apostrophe and a space.
func hookExe(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "it's a dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "o'brien hook"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	exe := filepath.Join(dir, name)
	if err := os.WriteFile(exe, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func runner(echo *bytes.Buffer) *Runner {
	r := &Runner{Env: append(os.Environ(), "GLHOOKTEST=1", "GREMLOADER_ACTION=outer")}
	if echo != nil {
		r.Echo = echo
	}
	return r
}

func hook(dir string, cmds ...[]string) engine.Hook {
	return engine.Hook{Package: "photocraft", Event: "post_update", Action: "update", Version: "2.0.0",
		Previous: "1.0.0", Dir: dir, Commands: cmds, Timeout: 30 * time.Second}
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func TestArgumentsEnvironmentAndFolder(t *testing.T) {
	exe := hookExe(t)
	pkgDir := filepath.Join(t.TempDir(), "Photo Craft's")
	if err := os.Mkdir(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"it's", "a b", `quo"ted`, "", `back\slash\`, "-NoProfile", "{x}"}
	var echo bytes.Buffer
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := events.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	r := runner(&echo)
	r.Events = log
	if err := r.Run(context.Background(), hook(pkgDir, append([]string{exe, "echo"}, args...))); err != nil {
		t.Fatal(err)
	}
	_ = log.Close()

	var got report
	if err := json.Unmarshal(echo.Bytes(), &got); err != nil {
		t.Fatalf("hook output %q: %v", echo.String(), err)
	}
	if !slices.Equal(got.Args, args) {
		t.Errorf("args = %q, want %q", got.Args, args)
	}
	want := map[string]string{"GREMLOADER_ACTION": "update", "GREMLOADER_PACKAGE": "photocraft",
		"GREMLOADER_VERSION": "2.0.0", "GREMLOADER_PREVIOUS": "1.0.0", "GREMLOADER_DIR": pkgDir}
	for k, v := range want {
		if got.Env[k] != v {
			t.Errorf("%s = %q, want %q", k, got.Env[k], v)
		}
	}
	if !samePath(got.Cwd, pkgDir) {
		t.Errorf("cwd = %q, want %q", got.Cwd, pkgDir)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	evs, _, err := events.Scan(bytes.NewReader(data))
	if err != nil || len(evs) != 1 || evs[0].Event != "hook.output" || evs[0].Step != "post_update" ||
		!strings.Contains(evs[0].Msg, "GREMLOADER_PACKAGE") {
		t.Errorf("events = %+v, %v", evs, err)
	}
}

func TestFailureStopsTheRestAndCarriesTheOutput(t *testing.T) {
	exe := hookExe(t)
	marker := filepath.Join(t.TempDir(), "second-ran")
	err := runner(nil).Run(context.Background(), hook(t.TempDir(), []string{exe, "fail"}, []string{exe, "touch", marker}))
	var he *Error
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want *Error", err)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Errorf("err = %v, want exit status 3", err)
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "o'brien hook") {
		t.Errorf("message = %q", err)
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Error("the second command ran after the first failed")
	}
}

func TestEveryCommandRunsInOrder(t *testing.T) {
	exe := hookExe(t)
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := runner(nil).Run(context.Background(), hook(dir, []string{exe, "touch", a}, []string{exe, "touch", b})); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); err != nil {
			t.Error(err)
		}
	}
}

// beats returns the size of the heartbeat file twice, 600 ms apart.
func beats(t *testing.T, file string) (before, after int64) {
	t.Helper()
	size := func() int64 {
		fi, err := os.Stat(file)
		if err != nil {
			return 0
		}
		return fi.Size()
	}
	time.Sleep(300 * time.Millisecond) // let a kill land
	before = size()
	time.Sleep(600 * time.Millisecond)
	return before, size()
}

func TestTimeoutEndsTheWholeTree(t *testing.T) {
	exe := hookExe(t)
	hb := filepath.Join(t.TempDir(), "heartbeat")
	h := hook(t.TempDir(), []string{exe, "spawn", hb, "stay"})
	h.Timeout = 1500 * time.Millisecond
	start := time.Now()
	err := runner(nil).Run(context.Background(), h)
	if err == nil || !strings.Contains(err.Error(), "timed out after 1.5s") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("took %s", d)
	}
	before, after := beats(t, hb)
	if before == 0 {
		t.Fatal("the grandchild never ran; the test proves nothing")
	}
	if after != before {
		t.Errorf("the grandchild is still running after the timeout (heartbeat %d -> %d)", before, after)
	}
}

func TestCancelEndsTheWholeTree(t *testing.T) {
	exe := hookExe(t)
	hb := filepath.Join(t.TempDir(), "heartbeat")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(1500*time.Millisecond, cancel)
	err := runner(nil).Run(ctx, hook(t.TempDir(), []string{exe, "spawn", hb, "stay"}))
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("err = %v", err)
	}
	before, after := beats(t, hb)
	if before == 0 || after != before {
		t.Errorf("heartbeat %d -> %d: the grandchild did not run, or was not ended", before, after)
	}
}

// A hook that finishes normally may leave a program running on purpose.
func TestSuccessLeavesWhatTheHookStarted(t *testing.T) {
	exe := hookExe(t)
	hb := filepath.Join(t.TempDir(), "heartbeat")
	if err := runner(nil).Run(context.Background(), hook(t.TempDir(), []string{exe, "spawn", hb, "exit"})); err != nil {
		t.Fatal(err)
	}
	before, after := beats(t, hb)
	if after <= before {
		t.Errorf("heartbeat %d -> %d: the program the hook started was ended", before, after)
	}
	// Let it end by itself before the temp folder holding its executable is removed.
	for last, deadline := after, time.Now().Add(10*time.Second); time.Now().Before(deadline); {
		time.Sleep(300 * time.Millisecond)
		_, now := beats(t, hb)
		if now == last {
			break
		}
		last = now
	}
}

func TestOutputIsCappedAndKeepsTheEnd(t *testing.T) {
	exe := hookExe(t)
	err := runner(nil).Run(context.Background(), hook(t.TempDir(), []string{exe, "loud"}))
	var he *Error
	if !errors.As(err, &he) {
		t.Fatalf("err = %v", err)
	}
	if len(he.Output) > keepOutput+100 || !strings.Contains(he.Output, "earlier output dropped") ||
		!strings.HasSuffix(strings.TrimSpace(he.Output), "END") {
		t.Errorf("output: %d bytes, ends %q", len(he.Output), he.Output[max(0, len(he.Output)-20):])
	}
	if len(err.Error()) > showOutput+500 || !strings.Contains(err.Error(), "END") {
		t.Errorf("message: %d bytes", len(err.Error()))
	}
}

func TestCommandsThatCannotStart(t *testing.T) {
	for _, argv := range [][]string{nil, {""}, {filepath.Join(t.TempDir(), "no such program")}} {
		err := runner(nil).Run(context.Background(), hook(t.TempDir(), argv))
		var he *Error
		if !errors.As(err, &he) {
			t.Errorf("%q: err = %v", argv, err)
		}
	}
}

func TestMissingFolderRunsInTheNearestOneAbove(t *testing.T) {
	exe := hookExe(t)
	parent := t.TempDir()
	var echo bytes.Buffer
	if err := runner(&echo).Run(context.Background(), hook(filepath.Join(parent, "gone", "deeper"), []string{exe, "echo"})); err != nil {
		t.Fatal(err)
	}
	var got report
	if err := json.Unmarshal(echo.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !samePath(got.Cwd, parent) {
		t.Errorf("cwd = %q, want %q", got.Cwd, parent)
	}
}

func TestTail(t *testing.T) {
	tl := &tail{max: 5}
	for _, s := range []string{"ab", "cd", "efg"} {
		_, _ = tl.Write([]byte(s))
	}
	if got := string(tl.buf); got != "cdefg" || !tl.dropped {
		t.Errorf("buf = %q, dropped %v", got, tl.dropped)
	}
	_, _ = tl.Write([]byte("0123456789"))
	if got := string(tl.buf); got != "56789" {
		t.Errorf("buf = %q", got)
	}
	short := &tail{max: 5}
	_, _ = short.Write([]byte("abc"))
	if short.String() != "abc" {
		t.Errorf("String = %q", short.String())
	}
}

func TestShow(t *testing.T) {
	got := show([]string{"powershell", "-File", `C:\it's here\x.ps1`, ""})
	if got != `powershell -File "C:\\it's here\\x.ps1" ""` {
		t.Errorf("show = %s", got)
	}
}

// An inline script (sh -c '...', as the Craft file's Linux hooks are) is shown by its first line:
// the message is about the hook failing, and the script is in the package file.
func TestShowShortensLongArguments(t *testing.T) {
	script := "d=$GREMLOADER_DIR p=$GREMLOADER_PACKAGE\n[ -d \"$d/share\" ] && echo x\nexit 1\n"
	if got := show([]string{"sh", "-c", script, "craft-hook"}); got != `sh -c "d=$GREMLOADER_DIR p=$GREMLOADER_PACKAGE ..." craft-hook` {
		t.Errorf("show = %s", got)
	}
	long := strings.Repeat("é", 40) // 80 bytes on its first line
	if got := show([]string{"x", long + "\nmore"}); got != `x "`+strings.Repeat("é", 30)+` ..."` {
		t.Errorf("show = %s", got)
	}
	// One line, however long, is kept: a program's path is what the reader needs.
	path := `C:\` + strings.Repeat("folder\\", 20) + "x.exe"
	if got := show([]string{path}); got != path {
		t.Errorf("show = %s", got)
	}
}
