package cli

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/enls/gremloader/internal/engine"
	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/lock"
	"github.com/enls/gremloader/internal/pkgfile"
	"github.com/enls/gremloader/internal/ver"
	"github.com/enls/gremloader/internal/version"
)

// withEngine loads the package file, opens a session and runs f in it.
func (c *call) withEngine(f func(s *session) int) int {
	w, code := c.load()
	if w == nil {
		return code
	}
	s, code := c.open(w)
	if s == nil {
		return code
	}
	defer s.close()
	return f(s)
}

// done reports items and exits with their combined code (E.4: 6 > 5 > 7 > 1).
func (c *call) done(items ...*engine.Item) int {
	return c.report(items, nil, (&engine.Plan{Items: items}).ExitCode(false))
}

// ---- remove and rollback: confirmed like apply ----

func defineRemove(fs *flag.FlagSet) func(*call, []string) int {
	deleteDir := fs.Bool("delete-dir", false, "for a package installed without a file list (adopted): delete its whole folder")
	return func(c *call, args []string) int {
		if len(args) != 1 {
			return c.usagef("name one package")
		}
		return c.withEngine(func(s *session) int {
			name := args[0]
			if rec := s.e.State().Packages[name]; rec != nil && rec.Version != "" {
				it := &engine.Item{Package: name, Action: engine.ActRemove, Result: engine.ResultPending, From: rec.Version, Dir: rec.Dir}
				if *deleteDir && rec.Manifest == "" {
					it.Notes = append(it.Notes, "deletes the whole folder "+rec.Dir)
				}
				if ok, code := c.confirm([]*engine.Item{it}); !ok {
					return code
				}
			}
			return c.done(s.e.Remove(c.ctx(), name, engine.RemoveOptions{DeleteDir: *deleteDir}))
		})
	}
}

func defineRollback(*flag.FlagSet) func(*call, []string) int {
	return func(c *call, args []string) int {
		if len(args) != 1 {
			return c.usagef("name one package")
		}
		return c.withEngine(func(s *session) int {
			name := args[0]
			if rec := s.e.State().Packages[name]; rec != nil && rec.Version != "" && rec.Previous != nil {
				it := &engine.Item{Package: name, Action: engine.ActRollback, Result: engine.ResultPending, From: rec.Version,
					To: rec.Previous.Version, Dir: rec.Dir}
				if ok, code := c.confirm([]*engine.Item{it}); !ok {
					return code
				}
			}
			return c.done(s.e.Rollback(c.ctx(), name))
		})
	}
}

// ---- verify ----

type checkRow struct {
	Package    string   `json:"package"`
	Version    string   `json:"version,omitempty"`
	Dir        string   `json:"dir,omitempty"`
	OK         bool     `json:"ok"`
	Files      int      `json:"files"`
	Missing    []string `json:"missing,omitempty"`
	Modified   []string `json:"modified,omitempty"`
	NoManifest bool     `json:"no_manifest,omitempty"`
	Quarantine string   `json:"quarantine,omitempty"`
	Error      string   `json:"error,omitempty"`
}

func defineVerify(fs *flag.FlagSet) func(*call, []string) int {
	remote := fs.Bool("remote", false, "download the installed version again and compare with it, instead of the file list")
	return func(c *call, args []string) int {
		return c.withEngine(func(s *session) int {
			for _, n := range args {
				if s.cfg.Packages[n] == nil && s.e.State().Packages[n] == nil {
					return c.failf(exitcode.Usage, "package %q is not in %s", n, s.file)
				}
			}
			checks := s.e.Verify(c.ctx(), args, *remote)
			code := exitcode.OK
			var rows []checkRow
			for _, ch := range checks {
				r := checkRow{Package: ch.Package, Version: ch.Version, Dir: ch.Dir, OK: ch.OK(), Files: ch.Files, Missing: ch.Missing,
					Modified: ch.Modified, NoManifest: ch.NoManifest, Quarantine: ch.Quarantine}
				if ch.Err != nil {
					r.Error = ch.Err.Error()
				}
				// Files that differ from what was installed are a failed check (exit 1); exit 5 is
				// kept for downloads that do not match (plan E.4).
				if !r.OK {
					code = exitcode.Failed
				}
				rows = append(rows, r)
			}
			if c.g.json {
				c.writeJSON(jsonOut{Command: c.name, ExitCode: code, Items: rows})
				return code
			}
			if len(rows) == 0 {
				say(c.io.Stdout, "nothing installed to verify\n")
			}
			for _, r := range rows {
				say(c.io.Stdout, "%s\n", verifyLine(r))
			}
			return code
		})
	}
}

func verifyLine(r checkRow) string {
	head := r.Package
	if r.Version != "" {
		head += " " + r.Version
	}
	var b strings.Builder
	switch {
	case r.Error != "":
		fmt.Fprintf(&b, "%s: failed: %s", head, indent(r.Error))
	case r.NoManifest:
		fmt.Fprintf(&b, "%s: no file list (adopted); `gremloader verify --remote` compares with the release", head)
	case r.OK:
		fmt.Fprintf(&b, "%s: ok, %d files as installed", head, r.Files)
	default:
		fmt.Fprintf(&b, "%s: %d missing, %d changed", head, len(r.Missing), len(r.Modified))
		for _, p := range r.Missing {
			fmt.Fprintf(&b, "\n  missing:  %s", p)
		}
		for _, p := range r.Modified {
			fmt.Fprintf(&b, "\n  changed:  %s", p)
		}
	}
	if r.Quarantine != "" {
		fmt.Fprintf(&b, "\n  a download that failed verification is kept in %s", r.Quarantine)
	}
	return b.String()
}

// ---- adopt ----

func defineAdopt(fs *flag.FlagSet) func(*call, []string) int {
	v := fs.String("version", "", "the version installed there (trusted as given)")
	check := fs.Bool("verify", false, "download that version and record the folder only if every file matches")
	return func(c *call, args []string) int {
		if len(args) != 1 {
			return c.usagef("name one package")
		}
		return c.adopt(args[0], *v, *check)
	}
}

// adopt records an existing folder as installed (adopt, install --adopt). Nothing is changed on
// disk, so there is nothing to confirm.
func (c *call) adopt(name, v string, check bool) int {
	return c.withEngine(func(s *session) int {
		if s.cfg.Packages[name] == nil {
			return c.failf(exitcode.Usage, "package %q is not in %s", name, s.file)
		}
		return c.done(s.e.Adopt(c.ctx(), name, engine.AdoptOptions{Version: v, Verify: check}))
	})
}

// ---- repair ----

func defineRepair(fs *flag.FlagSet) func(*call, []string) int {
	forget := fs.String("forget", "", "give up the interrupted change of this package instead (its folder is gone for good, or its journal is damaged)")
	return func(c *call, args []string) int {
		if len(args) != 0 {
			return c.usagef("unexpected argument %q", args[0])
		}
		return c.withEngine(func(s *session) int {
			if *forget == "" {
				items := s.e.Repair(c.ctx())
				if len(items) == 0 && !c.g.json {
					say(c.io.Stdout, "nothing to repair\n")
					return exitcode.OK
				}
				return c.done(items...)
			}
			it := &engine.Item{Package: *forget, Result: engine.ResultPending, Action: "forget",
				Reason: "give up its interrupted change without finishing or undoing it"}
			if ok, code := c.confirm([]*engine.Item{it}); !ok {
				return code
			}
			return c.done(s.e.Forget(*forget))
		})
	}
}

// ---- pin, unpin, hold, unhold: gremloader.local.toml ----

func definePin(*flag.FlagSet) func(*call, []string) int {
	return func(c *call, args []string) int {
		if len(args) != 2 {
			return c.usagef("name a package and a version: gremloader pin <package> <version>")
		}
		v, err := ver.Parse(args[1])
		if err != nil {
			return c.usagef("version %q: %v", args[1], err)
		}
		pin := v.String()
		return c.local(args[0], func(p *pkgfile.LocalPackage) string {
			p.Pin = &pin
			return "pinned to " + pin
		})
	}
}

func defineUnpin(*flag.FlagSet) func(*call, []string) int {
	return func(c *call, args []string) int {
		if len(args) != 1 {
			return c.usagef("name one package")
		}
		return c.local(args[0], func(p *pkgfile.LocalPackage) string { p.Pin = nil; return "not pinned" })
	}
}

func defineHold(*flag.FlagSet) func(*call, []string) int {
	return func(c *call, args []string) int {
		if len(args) != 1 {
			return c.usagef("name one package")
		}
		hold := true
		return c.local(args[0], func(p *pkgfile.LocalPackage) string { p.Hold = &hold; return "held: automatic updates skip it" })
	}
}

func defineUnhold(*flag.FlagSet) func(*call, []string) int {
	return func(c *call, args []string) int {
		if len(args) != 1 {
			return c.usagef("name one package")
		}
		return c.local(args[0], func(p *pkgfile.LocalPackage) string { p.Hold = nil; return "not held" })
	}
}

// local edits one package's entry in gremloader.local.toml (whitelisted keys only, ADR 0004).
// The file is replaced atomically, so a run reading it sees the old or the new file.
func (c *call) local(name string, edit func(*pkgfile.LocalPackage) string) int {
	w, code := c.load()
	if w == nil {
		return code
	}
	if w.cfg.Packages[name] == nil {
		return c.failf(exitcode.Usage, "package %q is not in %s", name, w.file)
	}
	l, err := pkgfile.LoadLocal(w.file)
	if err != nil {
		return c.failf(exitcode.Failed, "%v", err)
	}
	if l.Package == nil {
		l.Package = map[string]*pkgfile.LocalPackage{}
	}
	p := l.Package[name]
	if p == nil {
		p = &pkgfile.LocalPackage{}
		l.Package[name] = p
	}
	msg := edit(p)
	if *p == (pkgfile.LocalPackage{}) {
		delete(l.Package, name)
	}
	if err := pkgfile.SaveLocal(w.file, l); err != nil {
		return c.failf(exitcode.Failed, "%v", err)
	}
	if c.g.json {
		c.writeJSON(jsonOut{Command: c.name, Items: []jsonItem{{Package: name, Result: msg}}})
		return exitcode.OK
	}
	say(c.io.Stdout, "%s: %s\n", name, msg)
	return exitcode.OK
}

// ---- lock: write gremloader.lock ----

func defineLock(*flag.FlagSet) func(*call, []string) int {
	return func(c *call, args []string) int {
		return c.withEngine(func(s *session) int {
			entries, items, err := s.e.Lock(c.ctx(), engine.LockOptions{Packages: args})
			if err != nil {
				return c.failf(planErrCode(s.world, args), "%v", err)
			}
			path := lockPath(s.world)
			f, found, err := lock.Load(path)
			if err != nil {
				return c.failf(exitcode.Failed, "%v", err)
			}
			if !found {
				f = lock.New("", c.d.clock.Now())
			}
			if len(args) == 0 {
				// The whole file was locked: entries for packages it no longer names go.
				for n := range f.Packages {
					if s.cfg.Packages[n] == nil {
						delete(f.Packages, n)
					}
				}
			}
			for n, e := range entries {
				f.Packages[n] = e
			}
			f.GeneratedBy, f.GeneratedAt = "gremloader "+version.Get().Version, c.d.clock.Now().UTC().Truncate(time.Second)
			if len(f.Packages) > 0 {
				if err := lock.Save(path, f); err != nil {
					return c.failf(exitcode.Failed, "%v", err)
				}
			}
			return c.done(items...)
		})
	}
}
