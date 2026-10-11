package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/enls/gremloader/internal/clock"
	"github.com/enls/gremloader/internal/download"
	"github.com/enls/gremloader/internal/engine"
	"github.com/enls/gremloader/internal/events"
	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/hooks"
	"github.com/enls/gremloader/internal/paths"
	"github.com/enls/gremloader/internal/pkgfile"
	"github.com/enls/gremloader/internal/proc"
	"github.com/enls/gremloader/internal/runlock"
	"github.com/enls/gremloader/internal/source/github"
	"github.com/enls/gremloader/internal/version"
)

// deps is everything a run takes from the outside world. Tests replace it to run the whole
// command line in-process against a fake GitHub, in temporary folders.
type deps struct {
	getenv func(string) string
	cwd    func() (string, error)
	home   string // the user's home folder ("~" in package files)
	clock  clock.Clock

	// Network: empty bases and a nil policy mean the real GitHub.
	apiBase, webBase string
	policy           *download.Policy
	sleep            func(ctx context.Context, d time.Duration) error // retry backoff; nil = real sleep

	// Engine hooks into the machine; nil = the real thing.
	running    func(dir string) ([]proc.Process, error)
	freeSpace  func(path string) (uint64, error)
	exeVersion func(path string) (string, error)
	hooks      engine.HookRunner
}

func systemDeps() deps {
	home, _ := os.UserHomeDir()
	return deps{getenv: os.Getenv, cwd: os.Getwd, home: home, clock: clock.System{}}
}

// failf reports a failure that ends the command before any package result exists, in the
// output form the command line asked for, and returns code.
func (c *call) failf(code int, format string, a ...any) int {
	msg := fmt.Sprintf(format, a...)
	say(c.io.Stderr, "gremloader %s: %s\n", c.name, msg)
	if c.g.json {
		c.writeJSON(jsonOut{Command: c.name, ExitCode: code, Error: msg})
	}
	return code
}

// world is what every command that reads the package file has: where things are, and the file.
type world struct {
	paths paths.Paths
	file  string
	cfg   *pkgfile.Config
}

// getenv applies --home, which stands for GREMLOADER_HOME.
func (c *call) getenv(k string) string {
	if k == "GREMLOADER_HOME" && c.g.home != "" {
		return c.g.home
	}
	return c.d.getenv(k)
}

// load finds and reads the package file. On failure it has reported the error and returns the
// exit code.
func (c *call) load() (*world, int) {
	env := paths.System()
	env.Getenv, env.Home = c.getenv, c.d.home
	p, err := paths.Resolve(env)
	if err != nil {
		return nil, c.failf(exitcode.Failed, "%v", err)
	}
	cwd, err := c.d.cwd()
	if err != nil {
		return nil, c.failf(exitcode.Failed, "%v", err)
	}
	file, err := paths.FindPackageFile(c.g.file, env, cwd, p)
	if err != nil {
		return nil, c.failf(exitcode.Usage, "%v", err)
	}
	var opts pkgfile.Options
	if c.g.root != "" {
		opts.Vars = map[string]string{"root": c.g.root}
	}
	cfg, err := pkgfile.Load(file, opts)
	if err != nil {
		return nil, c.failf(exitcode.Failed, "%v", err)
	}
	return &world{paths: p, file: file, cfg: cfg}, exitcode.OK
}

// session is an open engine: the run lock is held and the event log is open.
type session struct {
	*world
	e    *engine.Engine
	log  *events.Logger
	lock *runlock.Lock
}

func (s *session) close() {
	_ = s.log.Close()
	_ = s.lock.Release()
}

// open takes the run lock and builds the engine. Every command that may write state.json goes
// through here, including check and verify --remote (a plan records what it learned): without
// the lock two runs could each overwrite the other's state.json.
func (c *call) open(w *world) (*session, int) {
	if err := os.MkdirAll(w.paths.State, 0o700); err != nil {
		return nil, c.failf(exitcode.Failed, "%v", err)
	}
	lk, err := runlock.Acquire(w.paths.RunLock(), runlock.Holder{PID: os.Getpid(), Version: version.Get().Version, Since: c.d.clock.Now()})
	if err != nil {
		var le *runlock.LockedError
		if errors.As(err, &le) {
			return nil, c.failf(exitcode.Locked, "%v", err)
		}
		return nil, c.failf(exitcode.Failed, "%v", err)
	}
	log, err := events.Open(w.paths.EventsLog(), events.WithClock(c.d.clock))
	if err != nil {
		_ = lk.Release()
		return nil, c.failf(exitcode.Failed, "the event log: %v", err)
	}
	_ = log.Emit(events.Event{Level: events.Info, Event: "run.start", Step: c.name, Msg: version.Get().String()})

	policy := download.GitHubPolicy()
	if c.d.policy != nil {
		policy = *c.d.policy
	}
	token := c.d.getenv("GITHUB_TOKEN")
	if token == "" {
		token = c.d.getenv("GH_TOKEN")
	}
	client := download.New(download.Config{Policy: policy, Token: token, Clock: c.d.clock, Sleep: c.d.sleep})
	var runner engine.HookRunner = &hooks.Runner{Events: log}
	if c.g.verbose {
		runner = &hooks.Runner{Events: log, Echo: c.io.Stderr}
	}
	if c.d.hooks != nil {
		runner = c.d.hooks
	}
	e, err := engine.New(engine.Config{
		Paths:      w.paths,
		Packages:   w.cfg,
		Source:     github.New(client, github.Options{APIBase: c.d.apiBase, WebBase: c.d.webBase}),
		Fetcher:    client,
		Events:     log,
		Hooks:      runner,
		Clock:      c.d.clock,
		Home:       c.d.home,
		Running:    c.d.running,
		FreeSpace:  c.d.freeSpace,
		ExeVersion: c.d.exeVersion,
	})
	if err != nil {
		_ = log.Close()
		_ = lk.Release()
		return nil, c.failf(exitcode.Failed, "%v", err)
	}
	return &session{world: w, e: e, log: log, lock: lk}, exitcode.OK
}
