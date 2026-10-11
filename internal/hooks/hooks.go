// Package hooks runs a package's hook commands (plan E.3.8): argv arrays exactly as the package
// file wrote them, never through a shell, in the package's folder, with GREMLOADER_* variables
// saying what happened. A hook that outlives its timeout is stopped together with every process
// it started (a Windows job object, a process group elsewhere), so a stuck script cannot hold a
// run, or its output pipe, open.
package hooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/enls/gremloader/internal/engine"
	"github.com/enls/gremloader/internal/events"
)

const (
	// keepOutput is how much of a command's output (the end of it) is kept.
	keepOutput = 64 << 10
	// showOutput is how much of that end goes into an error message.
	showOutput = 2 << 10
	// waitDelay bounds the wait for the output pipe after the command exited (or was stopped): a
	// process it left running in the background may still hold the pipe.
	waitDelay = 5 * time.Second
)

// Runner runs hooks; it is the program's engine.HookRunner.
type Runner struct {
	// Env is the environment hooks start from; nil means this process's environment.
	Env []string
	// Events receives each command's output (event hook.output); nil logs nothing.
	Events *events.Logger
	// Echo, if set, receives the output as it is written (--verbose).
	Echo io.Writer
}

var _ engine.HookRunner = (*Runner)(nil)

// Error is a hook command that failed: it could not start, exited non-zero, or was stopped.
type Error struct {
	Command []string
	Err     error
	Output  string // the end of what it printed
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s: %v", show(e.Command), e.Err)
	if out := strings.TrimSpace(e.Output); out != "" {
		if len(out) > showOutput {
			out = "..." + strings.ToValidUTF8(out[len(out)-showOutput:], "")
		}
		msg += "; its output ends:\n" + out
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Run runs the hook's commands in order; the first one that fails stops the rest.
func (r *Runner) Run(ctx context.Context, h engine.Hook) error {
	for _, argv := range h.Commands {
		if err := r.runOne(ctx, h, argv); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) runOne(ctx context.Context, h engine.Hook, argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return &Error{Command: argv, Err: errors.New("an empty command")}
	}
	runCtx := ctx
	if h.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, h.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...) //nolint:gosec // G204: running the package file's own hook argv is the point
	cmd.Dir = workDir(h.Dir)
	env := r.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = append(append([]string{}, env...),
		"GREMLOADER_ACTION="+h.Action,
		"GREMLOADER_PACKAGE="+h.Package,
		"GREMLOADER_VERSION="+h.Version,
		"GREMLOADER_PREVIOUS="+h.Previous,
		"GREMLOADER_DIR="+h.Dir,
	)
	out := &tail{max: keepOutput}
	var w io.Writer = out
	if r.Echo != nil {
		w = io.MultiWriter(out, r.Echo)
	}
	cmd.Stdout, cmd.Stderr = w, w // one writer: one pipe, output in the order it was written
	cmd.WaitDelay = waitDelay

	// Stop the whole tree, not only the command, when the context ends. Until the tree is
	// known (a moment after Start) only the command itself can be stopped; it has not run any
	// code of its own yet on Windows (started suspended), and has had no time to on Linux.
	var tr atomic.Pointer[tree]
	cmd.Cancel = func() error {
		if t := tr.Load(); t != nil {
			return t.kill()
		}
		return cmd.Process.Kill()
	}
	prepare(cmd)

	err := cmd.Start()
	if err == nil {
		t, terr := startTree(cmd)
		if terr != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			err = fmt.Errorf("starting it: %w", terr)
		} else {
			tr.Store(t)
			err = cmd.Wait()
			t.close()
		}
	}

	switch {
	case errors.Is(err, exec.ErrWaitDelay):
		err = nil // it finished; something it left running still holds the output pipe
	case err != nil && ctx.Err() != nil:
		err = fmt.Errorf("stopped (%w); it and everything it started were ended", ctx.Err())
	case err != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded):
		err = fmt.Errorf("timed out after %s; it and everything it started were ended", h.Timeout)
	}
	if r.Events != nil && out.Len() > 0 {
		level := events.Info
		if err != nil {
			level = events.Warn
		}
		_ = r.Events.Emit(events.Event{Level: level, Event: "hook.output", Package: h.Package, Step: h.Event,
			Msg: show(argv) + ":\n" + out.String()})
	}
	if err != nil {
		return &Error{Command: argv, Err: err, Output: out.String()}
	}
	return nil
}

// workDir is the package folder, or when that is gone (a hook after the folder was deleted) the
// nearest folder above it that exists, so the hook still starts somewhere predictable.
func workDir(dir string) string {
	for d := dir; d != ""; {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return os.TempDir()
}

// show renders argv for a message, quoting what would otherwise be ambiguous.
func show(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		a = shorten(a)
		if a == "" || strings.ContainsAny(a, " \t\n\"'") {
			a = strconv.Quote(a)
		}
		parts[i] = a
	}
	return strings.Join(parts, " ")
}

// showArg is how much of an inline script's first line a message shows.
const showArg = 60

// shorten turns a multi-line argument (an inline script) into its first line, at most showArg
// bytes, followed by " ...". A one-line argument, however long (a program's path), is kept whole.
func shorten(a string) string {
	first, _, multi := strings.Cut(a, "\n")
	if !multi {
		return a
	}
	if len(first) > showArg {
		first = first[:showArg]
		for !utf8.ValidString(first) {
			first = first[:len(first)-1]
		}
	}
	return strings.TrimSpace(first) + " ..."
}

// tail keeps the last max bytes written to it.
type tail struct {
	max     int
	buf     []byte
	dropped bool
}

func (t *tail) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= t.max {
		t.buf, t.dropped = append(t.buf[:0], p[len(p)-t.max:]...), true
		return n, nil
	}
	if over := len(t.buf) + len(p) - t.max; over > 0 {
		t.buf, t.dropped = append(t.buf[:0], t.buf[over:]...), true
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tail) Len() int { return len(t.buf) }

func (t *tail) String() string {
	s := strings.ToValidUTF8(string(t.buf), "")
	if t.dropped {
		return "...[earlier output dropped]\n" + s
	}
	return s
}
