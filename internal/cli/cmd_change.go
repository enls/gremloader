package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/enls/gremloader/internal/engine"
	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/lock"
	"github.com/enls/gremloader/internal/ver"
	"github.com/enls/gremloader/internal/version"
)

// change is what plan, apply, install and update share once their arguments are understood.
type change struct {
	rq         engine.Request
	locked     bool
	updateLock bool
	accept     *engine.Acceptance
	planOnly   bool
}

func definePlan(fs *flag.FlagSet) func(*call, []string) int {
	now := fs.Bool("now", false, "ignore cool-down periods")
	locked := fs.Bool("locked", false, "plan exactly what gremloader.lock says")
	return func(c *call, args []string) int {
		return c.change(change{rq: engine.Request{Packages: args, InstallMissing: true, IgnoreCooldown: *now}, locked: *locked, planOnly: true})
	}
}

func defineApply(fs *flag.FlagSet) func(*call, []string) int {
	now := fs.Bool("now", false, "ignore cool-down periods")
	locked := fs.Bool("locked", false, "install exactly what gremloader.lock says (hash and size must match)")
	updateLock := fs.Bool("update-lock", false, "afterwards, record what was installed in gremloader.lock")
	accept := fs.String("accept-sha256", "", "not for apply: see install and update")
	return func(c *call, args []string) int {
		if *accept != "" {
			return c.usagef("--accept-sha256 is for one package and one version: " +
				"`gremloader install <package>@<version> --accept-sha256 <hash>` or `gremloader update <package> --version <version> --accept-sha256 <hash>`")
		}
		if *locked && *updateLock {
			return c.usagef("--locked installs what the lock says; --update-lock rewrites it: use one")
		}
		return c.change(change{rq: engine.Request{Packages: args, InstallMissing: true, IgnoreCooldown: *now}, locked: *locked, updateLock: *updateLock})
	}
}

func defineInstall(fs *flag.FlagSet) func(*call, []string) int {
	now := fs.Bool("now", false, "ignore cool-down periods")
	overlay := fs.Bool("overlay", false, "install into a folder that is not empty and that GremLoader does not manage")
	createDir := fs.Bool("create-dir", false, "create the install folder's missing parent folders")
	downgrade := fs.Bool("allow-downgrade", false, "allow installing a lower version than the installed one")
	force := fs.Bool("force", false, "reinstall the installed version")
	updateLock := fs.Bool("update-lock", false, "afterwards, record what was installed in gremloader.lock")
	accept := fs.String("accept-sha256", "", "accept this SHA-256 for the download although the release's checksum says otherwise (with <package>@<version>)")
	adopt := fs.Bool("adopt", false, "record the existing folder as this package instead of installing (like the adopt command; @<version> is trusted)")
	return func(c *call, args []string) int {
		if len(args) != 1 {
			return c.usagef("name one package, optionally with a version: <package>[@<version>]")
		}
		name, want, at := strings.Cut(args[0], "@")
		if at {
			if _, err := ver.ParseLoose(want); err != nil {
				return c.usagef("%q: the version after @ is not a version: %v", args[0], err)
			}
		}
		if *adopt {
			return c.adopt(name, want, false)
		}
		ch := change{rq: engine.Request{Packages: []string{name}, Want: want, InstallMissing: true, IgnoreCooldown: *now,
			Overlay: *overlay, CreateDir: *createDir, AllowDowngrade: *downgrade, Force: *force}, updateLock: *updateLock}
		var code int
		if ch.accept, code = c.acceptance(*accept, name, want, "install "+name+"@<version>"); code != exitcode.OK {
			return code
		}
		return c.change(ch)
	}
}

func defineUpdate(fs *flag.FlagSet) func(*call, []string) int {
	now := fs.Bool("now", false, "ignore cool-down periods")
	want := fs.String("version", "", "update to this exact version (one package)")
	force := fs.Bool("force", false, "reinstall the installed version")
	held := fs.Bool("include-held", false, "also update held packages")
	downgrade := fs.Bool("allow-downgrade", false, "allow --version to be lower than the installed one")
	updateLock := fs.Bool("update-lock", false, "afterwards, record what was installed in gremloader.lock")
	accept := fs.String("accept-sha256", "", "accept this SHA-256 for the download although the release's checksum says otherwise (with --version)")
	return func(c *call, args []string) int {
		if *want != "" && len(args) != 1 {
			return c.usagef("--version needs exactly one package")
		}
		if *want != "" {
			if _, err := ver.ParseLoose(*want); err != nil {
				return c.usagef("--version %q: %v", *want, err)
			}
		}
		ch := change{rq: engine.Request{Packages: args, Want: *want, IgnoreCooldown: *now, Force: *force, IncludeHeld: *held,
			AllowDowngrade: *downgrade}, updateLock: *updateLock}
		if *accept != "" && len(args) != 1 {
			return c.usagef("--accept-sha256 is for exactly one package")
		}
		var code int
		name := ""
		if len(args) == 1 {
			name = args[0]
		}
		if ch.accept, code = c.acceptance(*accept, name, *want, "update "+name+" --version <version>"); code != exitcode.OK {
			return code
		}
		return c.change(ch)
	}
}

var sha256Hex = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// acceptance turns --accept-sha256 into the engine's form: one package, one exact version, a
// well-formed hash. It is never implied by --yes and never read from a file (plan E.3.4).
func (c *call) acceptance(hash, name, want, how string) (*engine.Acceptance, int) {
	if hash == "" {
		return nil, exitcode.OK
	}
	if want == "" {
		return nil, c.usagef("--accept-sha256 needs the exact version it is for: gremloader %s --accept-sha256 <hash>", how)
	}
	v, err := ver.Parse(want)
	if err != nil {
		return nil, c.usagef("version %q: %v", want, err)
	}
	if !sha256Hex.MatchString(hash) {
		return nil, c.usagef("--accept-sha256 %q is not a SHA-256 (64 hexadecimal digits)", hash)
	}
	return &engine.Acceptance{Package: name, Version: v.String(), SHA256: strings.ToLower(hash)}, exitcode.OK
}

func lockPath(w *world) string { return filepath.Join(filepath.Dir(w.file), "gremloader.lock") }

// change plans, shows the plan, asks (or takes --yes), applies, and records in the lock what was
// asked for. The exit code is the plan's (E.4).
func (c *call) change(ch change) int {
	w, code := c.load()
	if w == nil {
		return code
	}
	if ch.locked {
		lf, found, err := lock.Load(lockPath(w))
		switch {
		case err != nil:
			return c.failf(exitcode.Failed, "%v", err)
		case !found:
			return c.failf(exitcode.Usage, "--locked: there is no %s (`gremloader lock` writes it)", lockPath(w))
		}
		ch.rq.Locked = lf
	}
	s, code := c.open(w)
	if s == nil {
		return code
	}
	defer s.close()
	ctx := c.ctx()

	p, err := s.e.Plan(ctx, ch.rq)
	if err != nil {
		return c.failf(planErrCode(w, ch.rq.Packages), "%v", err)
	}
	if ch.planOnly || !p.Pending() {
		return c.report(p.Items, p.Warnings, p.ExitCode(false))
	}
	if ok, code := c.confirm(p.Items); !ok {
		return code
	}
	if err := s.e.Apply(ctx, p, engine.ApplyOptions{Accept: ch.accept}); err != nil {
		return c.failf(exitcode.Usage, "%v", err)
	}
	warnings := append(p.Warnings, c.recordInLock(ctx, s, p, ch.updateLock)...)
	return c.report(p.Items, warnings, p.ExitCode(false))
}

// confirm asks before anything changes (Q10): --yes confirms; otherwise the plan is shown and a
// terminal must answer. Without a terminal and without --yes nothing is changed (exit 4). --yes
// only confirms: a downgrade, an overlay or a hash override still needs its own flag, which the
// engine enforces.
func (c *call) confirm(items []*engine.Item) (bool, int) {
	if c.g.yes {
		return true, exitcode.OK
	}
	if !c.g.json {
		printItems(c.io.Stdout, items, c.g.quiet)
	}
	if !c.d.stdinTTY() {
		return false, c.failf(exitcode.Usage, "nothing was changed: there is no terminal to confirm on; run again with --yes to apply this plan")
	}
	if c.g.json {
		// stdout is the JSON object's; the plan being confirmed still has to be seen.
		printItems(c.io.Stderr, items, c.g.quiet)
	}
	say(c.io.Stderr, "Apply these changes? [y/N] ")
	line, _ := bufio.NewReader(c.io.Stdin).ReadString('\n')
	if ans := strings.ToLower(strings.TrimSpace(line)); ans == "y" || ans == "yes" {
		return true, exitcode.OK
	}
	return false, c.failf(exitcode.OK, "not confirmed; nothing was changed")
}

// recordInLock writes to gremloader.lock what the run asks for: with --update-lock the packages
// it installed or updated (at exactly the versions installed), and in every case an accepted hash
// override into a lock that names that package at that version. It never creates a lock for an
// override alone. Problems are returned as warnings: the install itself is done.
func (c *call) recordInLock(ctx context.Context, s *session, p *engine.Plan, updateLock bool) []string {
	path := lockPath(s.world)
	f, found, err := lock.Load(path)
	if err != nil {
		return []string{fmt.Sprintf("%s was not updated: %v", path, err)}
	}
	var warnings []string
	dirty := false
	if updateLock {
		want := map[string]string{}
		var names []string
		for _, it := range p.Items {
			if it.Result == engine.ResultInstalled || it.Result == engine.ResultUpdated {
				want[it.Package], names = it.To, append(names, it.Package)
			}
		}
		if len(names) > 0 {
			entries, items, err := s.e.Lock(ctx, engine.LockOptions{Packages: names, Want: want})
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%s was not updated: %v", path, err))
			}
			for _, li := range items {
				if li.Err != nil {
					warnings = append(warnings, fmt.Sprintf("%s: %s was not updated: %v", filepath.Base(path), li.Package, li.Err))
				}
			}
			if !found {
				f, found = lock.New("", c.d.clock.Now()), true
			}
			for n, e := range entries {
				f.Packages[n] = e
				dirty = true
			}
		}
	}
	for _, it := range p.Items {
		if it.Override == nil || !found {
			continue
		}
		// Only a download that is now installed: a run that failed after accepting it must not
		// teach other machines' --locked runs to trust it.
		if rec := s.e.State().Packages[it.Package]; rec == nil || rec.Version != it.To || rec.AssetSHA256 != it.Override.SHA256 {
			continue
		}
		if f.Override(it.Package, it.To, c.platform(), *it.Override) {
			it.Notes = append(it.Notes, fmt.Sprintf("%s records the accepted hash, marked verified_by = %q", filepath.Base(path), lock.HashOverride))
			dirty = true
		} else {
			it.Notes = append(it.Notes, fmt.Sprintf("%s does not name %s %s on this platform; it was not changed", filepath.Base(path), it.Package, it.To))
		}
	}
	if dirty {
		f.GeneratedBy, f.GeneratedAt = "gremloader "+version.Get().Version, c.d.clock.Now().UTC().Truncate(time.Second)
		if err := lock.Save(path, f); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s was not updated: %v", path, err))
		}
	}
	return warnings
}
