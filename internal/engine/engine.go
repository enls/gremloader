// Package engine plans and applies changes to installed packages: install, update, rollback,
// remove, verify, adopt and repair (plan E.3).
//
// Every change to an install directory goes through one change-set executor that records each
// step in a journal before it happens (package journal), so a failure is undone and a crash is
// repaired, never left half-done. The engine is single-threaded and assumes the caller holds the
// run lock (package runlock) for as long as it is used.
package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/enls/gremloader/internal/clock"
	"github.com/enls/gremloader/internal/download"
	"github.com/enls/gremloader/internal/events"
	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/paths"
	"github.com/enls/gremloader/internal/peversion"
	"github.com/enls/gremloader/internal/pkgfile"
	"github.com/enls/gremloader/internal/platform"
	"github.com/enls/gremloader/internal/proc"
	"github.com/enls/gremloader/internal/source/github"
	"github.com/enls/gremloader/internal/state"
)

// Lister lists releases and picks assets; *github.Source is one.
type Lister interface {
	List(ctx context.Context, q github.Query) (*github.Listing, error)
	SelectAsset(r github.Release, pattern string) (github.Asset, error)
}

// Fetcher downloads one file; *download.Client is one.
type Fetcher interface {
	Fetch(ctx context.Context, url, dst string, o download.FetchOptions) (download.Result, error)
}

// Hook is one hook event of one package, ready to run.
type Hook struct {
	Package  string
	Event    string // pkgfile.PostInstall ...
	Action   string // GREMLOADER_ACTION: install, update, rollback, remove ...
	Version  string
	Previous string
	Dir      string
	Commands [][]string
	Timeout  time.Duration
}

// HookRunner runs hooks. The command-line program supplies one (M6); without one, hooks are not
// run and the plan says so.
type HookRunner interface {
	Run(ctx context.Context, h Hook) error
}

// Config is what the engine works with. Paths, Packages, Source and Fetcher are required.
type Config struct {
	Paths    paths.Paths
	Packages *pkgfile.Config
	Source   Lister
	Fetcher  Fetcher
	Events   *events.Logger // nil: nothing is logged
	Hooks    HookRunner     // nil: hooks are not run
	Clock    clock.Clock    // default: the system clock
	Platform platform.Platform
	Home     string // the user's home, for "~" in dir templates
	// Running lists the processes started from a directory; default proc.Under.
	Running func(dir string) ([]proc.Process, error)
	// FreeSpace is the space available on the volume holding a path; default: asked of the system.
	FreeSpace func(path string) (uint64, error)
	// ExeVersion reads an executable's product version (adopt); default peversion.ProductVersion.
	ExeVersion func(path string) (string, error)
}

// Engine is a loaded state plus the means to change it.
type Engine struct {
	cfg   Config
	st    *state.State
	fault func(point string) error // tests: fail or "crash" at a named point
}

// New loads state.json. A damaged state file is an error, never an empty state (state.Load).
func New(cfg Config) (*Engine, error) {
	if cfg.Packages == nil || cfg.Source == nil || cfg.Fetcher == nil || cfg.Paths.State == "" {
		return nil, errors.New("engine: Paths, Packages, Source and Fetcher are required")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System{}
	}
	if cfg.Platform == (platform.Platform{}) {
		cfg.Platform = platform.Current()
	}
	if cfg.Running == nil {
		cfg.Running = proc.Under
	}
	if cfg.FreeSpace == nil {
		cfg.FreeSpace = freeBytes
	}
	if cfg.ExeVersion == nil {
		cfg.ExeVersion = peversion.ProductVersion
	}
	st, err := state.Load(cfg.Paths.StateFile())
	if err != nil {
		return nil, err
	}
	return &Engine{cfg: cfg, st: st}, nil
}

// State is the engine's view of state.json (read-only for callers).
func (e *Engine) State() *state.State { return e.st }

func (e *Engine) saveState() error { return state.Save(e.cfg.Paths.StateFile(), e.st) }

func (e *Engine) now() time.Time { return e.cfg.Clock.Now().UTC() }

// emit logs an event; logging never fails a change.
func (e *Engine) emit(ev events.Event) {
	if e.cfg.Events != nil {
		_ = e.cfg.Events.Emit(ev)
	}
}

// at is a fault-injection point. Production code has no fault function.
func (e *Engine) at(point string) error {
	if e.fault == nil {
		return nil
	}
	return e.fault(point)
}

// errCrash simulates the process dying: the engine returns at once, undoing nothing, as if the
// power had gone. Only tests produce it.
var errCrash = errors.New("simulated crash")

// VerifyError means a download did not match what it was checked against, or a required check
// could not be made (exit 5). Nothing was installed.
type VerifyError struct {
	Package string
	Msg     string

	received string // set for a hash mismatch: the hash of the bytes received
}

func (e *VerifyError) Error() string { return e.Package + ": " + e.Msg }

// RepairError means a package has an unfinished change (exit 6): run repair.
type RepairError struct {
	Package string
	Msg     string
}

func (e *RepairError) Error() string { return e.Package + ": " + e.Msg }

// Item is one package in a plan, and after applying, its outcome.
type Item struct {
	Package string
	Action  Action // what applying does; ActNone when nothing will be done
	Result  Result // the outcome, or why there is no action
	From    string // installed version ("" none)
	To      string // version to install
	Reason  string
	Notes   []string // warnings worth showing: drift, listing warnings, odd checksum files
	Err     error

	Dir       string
	Release   github.Release
	Asset     github.Asset
	Checksums *github.Asset // the checksum file; nil when the package has none
	Sidecar   bool          // Checksums is a per-asset file ("{asset}.sha256")
	Via       github.Mode   // how the release was found; the GitHub digest is only known via the API
	Locked    bool          // from gremloader.lock: the lock's hash stands in for the checksum file
	LockHash  string
	LockSize  int64
	Overlay   bool // installing into a non-empty folder that GremLoader does not manage (--overlay)
	CreateDir bool // the install folder's parent is missing and may be created (--create-dir)
}

// Plan is what a run will do.
type Plan struct {
	Items    []*Item
	Warnings []string
}

// Pending reports whether any item has something to do.
func (p *Plan) Pending() bool {
	for _, it := range p.Items {
		if it.Action != ActNone {
			return true
		}
	}
	return false
}

// ExitCode is the run's exit code (plan E.4): the worst failure, else 2 when check-style output
// found something to do (updates), else 0.
func (p *Plan) ExitCode(updatesAvailable bool) int {
	var codes []int
	resolved := false
	offline := false
	for _, it := range p.Items {
		switch {
		case it.Result == ResultNeedsRepair:
			codes = append(codes, exitcode.NeedsRepair)
		case it.Result == ResultFailed && isVerify(it.Err):
			codes = append(codes, exitcode.VerifyFailed)
		case it.Result == ResultFailed && isOffline(it.Err):
			offline = true
		case it.Result == ResultFailed:
			codes = append(codes, exitcode.Failed)
		default:
			resolved = true
		}
		if updatesAvailable && it.Action != ActNone {
			codes = append(codes, exitcode.UpdatesAvailable)
		}
	}
	if offline {
		// Offline is the answer only when nothing could be resolved at all; otherwise the packages
		// that could not be reached are ordinary failures of a run that otherwise worked.
		if resolved {
			codes = append(codes, exitcode.Failed)
		} else {
			codes = append(codes, exitcode.Offline)
		}
	}
	return exitcode.Worst(codes...)
}

func isVerify(err error) bool {
	var ve *VerifyError
	return errors.As(err, &ve)
}

// isOffline reports a failure to reach GitHub: a rate limit, or a network error that never got an
// HTTP answer.
func isOffline(err error) bool {
	var rl *download.RateLimitError
	var se *download.StatusError
	var pe *download.PolicyError
	switch {
	case err == nil:
		return false
	case errors.As(err, &rl):
		return true
	case errors.As(err, &se), errors.As(err, &pe), errors.Is(err, github.ErrNotFound),
		errors.Is(err, context.Canceled):
		return false
	}
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) || errors.Is(err, download.ErrStalled)
}

// fail marks an item failed with err.
func (it *Item) fail(err error) {
	it.Action, it.Result, it.Err = ActNone, ResultFailed, err
	var re *RepairError
	if errors.As(err, &re) {
		it.Result = ResultNeedsRepair
	}
}

func (it *Item) notef(format string, args ...any) {
	it.Notes = append(it.Notes, fmt.Sprintf(format, args...))
}
