package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/enls/gremloader/internal/engine"
	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/paths"
	"github.com/enls/gremloader/internal/ver"
	"github.com/enls/gremloader/internal/version"
)

// selfRequest is what self-update was asked for.
type selfRequest struct {
	check, prerelease, downgrade, regression bool
	want                                     string
}

func defineSelfUpdate(fs *flag.FlagSet) func(*call, []string) int {
	check := fs.Bool("check", false, "only look for a newer version; exit 2 when there is one")
	pre := fs.Bool("prerelease", false, "consider pre-releases too (always, when this binary is one)")
	want := fs.String("version", "", "install this exact version")
	downgrade := fs.Bool("allow-downgrade", false, "allow --version to be lower than this binary's version")
	regression := regressionFlag(fs)
	return func(c *call, args []string) int {
		if len(args) != 0 {
			return c.usagef("unexpected argument %q", args[0])
		}
		if *want != "" {
			if _, err := ver.ParseLoose(*want); err != nil {
				return c.usagef("--version %q: %v", *want, err)
			}
		}
		return c.selfUpdate(selfRequest{check: *check, prerelease: *pre, want: *want, downgrade: *downgrade, regression: *regression})
	}
}

// selfUpdate replaces the running binary with a release of GremLoader (ADR 0015): the built-in
// package, installed in the binary's own folder, through the engine like any other package.
func (c *call) selfUpdate(o selfRequest) int {
	vs := c.d.selfVersion
	if vs == "" {
		vs = version.Get().Version
	}
	cur, err := ver.Parse(vs)
	if err != nil {
		return c.failf(exitcode.Usage, "this is a development build (version %q), which cannot update itself; install a release", vs)
	}
	exe, err := c.selfExe()
	if err != nil {
		return c.failf(exitcode.Failed, "finding the running program: %v", err)
	}
	windows := c.platform().OS == "windows"
	name := selfName
	if windows {
		name += ".exe"
	}
	// The archive holds the binary under this name: a renamed one would get a second copy beside it.
	if base := filepath.Base(exe); base != name && (!windows || !strings.EqualFold(base, name)) {
		return c.failf(exitcode.Usage, "the running program is %s; self-update installs %s beside it, so it only updates a binary of that name", exe, name)
	}
	channel := "stable"
	if o.prerelease || cur.IsPrerelease() {
		channel = "prerelease"
	}
	cfg, err := selfConfig(filepath.Dir(exe), channel)
	if err != nil {
		return c.failf(exitcode.Failed, "%v", err)
	}
	p, err := paths.Resolve(c.env())
	if err != nil {
		return c.failf(exitcode.Failed, "%v", err)
	}
	s, code := c.open(&world{paths: p, file: cfg.File, cfg: cfg, engineState: filepath.Join(p.State, "self"), self: exe})
	if s == nil {
		return code
	}
	defer s.close()
	ctx := c.ctx()

	// An interrupted self-update is finished or undone first: `repair` does not look here.
	var warnings []string
	for _, it := range s.e.Repair(ctx) {
		if it.Result != engine.ResultRepaired {
			return c.done(it)
		}
		warnings = append(warnings, fmt.Sprintf("an interrupted self-update was repaired first: %s", it.Reason))
	}
	// The running binary is the truth: a record of another version or folder is replaced.
	dir := filepath.Dir(exe)
	if rec := s.e.State().Packages[selfName]; rec == nil || rec.Version != cur.String() || !sameDir(rec.Dir, dir, windows) {
		if it := s.e.Adopt(ctx, selfName, engine.AdoptOptions{Version: cur.String(), Self: true}); it.Result != engine.ResultAdopted {
			return c.done(it)
		}
	}

	pl, err := s.e.Plan(ctx, engine.Request{Packages: []string{selfName}, Want: o.want, AllowDowngrade: o.downgrade})
	if err != nil {
		return c.failf(exitcode.Failed, "%v", err)
	}
	if o.check || !pl.Pending() {
		return c.report(pl.Items, warnings, pl.ExitCode(o.check))
	}
	if ok, code := c.confirm(pl.Items); !ok {
		return code
	}
	if err := s.e.Apply(ctx, pl, engine.ApplyOptions{AllowAttestationRegression: o.regression}); err != nil {
		return c.failf(exitcode.Usage, "%v", err)
	}
	return c.report(pl.Items, warnings, pl.ExitCode(false))
}

// selfExe is the running binary, links resolved: the file self-update replaces.
func (c *call) selfExe() (string, error) {
	get := c.d.selfExe
	if get == nil {
		get = os.Executable
	}
	p, err := get()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return filepath.Abs(p)
}

func sameDir(a, b string, foldCase bool) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if foldCase {
		return strings.EqualFold(a, b)
	}
	return a == b
}
