package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/enls/gremloader/internal/engine"
	"github.com/enls/gremloader/internal/events"
	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/journal"
	"github.com/enls/gremloader/internal/state"
	"github.com/enls/gremloader/internal/ver"
)

// ---- list: what is installed, from state.json and the package file; no network, no lock ----

type listRow struct {
	Package   string `json:"package"`
	Installed string `json:"installed,omitempty"`
	Latest    string `json:"latest_seen,omitempty"`
	Status    string `json:"status"`
	Dir       string `json:"dir,omitempty"`
	LastCheck string `json:"last_check,omitempty"`
}

func defineList(*flag.FlagSet) func(*call, []string) int {
	return func(c *call, args []string) int {
		if len(args) != 0 {
			return c.usagef("unexpected argument %q", args[0])
		}
		w, code := c.load()
		if w == nil {
			return code
		}
		// state.json is replaced atomically, so it can be read while a run holds the lock.
		st, err := state.Load(w.paths.StateFile())
		if err != nil {
			return c.failf(exitcode.Failed, "%v", err)
		}
		var rows []listRow
		names := w.cfg.Names()
		for n, rec := range st.Packages {
			if rec.Version != "" && w.cfg.Packages[n] == nil {
				names = append(names, n)
			}
		}
		slices.Sort(names)
		for _, n := range names {
			rows = append(rows, c.listRow(w, n, st.Packages[n]))
		}
		if c.g.json {
			c.writeJSON(jsonOut{Command: c.name, Items: rows})
			return exitcode.OK
		}
		out := [][]string{{"PACKAGE", "INSTALLED", "LATEST SEEN", "STATUS"}}
		for _, r := range rows {
			out = append(out, []string{r.Package, dash(r.Installed), dash(r.Latest), r.Status})
		}
		table(c.io.Stdout, out)
		return exitcode.OK
	}
}

func (c *call) listRow(w *world, name string, rec *state.Package) listRow {
	r := listRow{Package: name}
	if rec != nil {
		r.Installed, r.Dir = rec.Version, rec.Dir
		if rec.LatestSeen != nil {
			r.Latest = rec.LatestSeen.Version
		}
		if rec.LastCheck != nil {
			r.LastCheck = rec.LastCheck.UTC().Format(time.RFC3339)
		}
	}
	pkg := w.cfg.Packages[name]
	switch {
	case pkg == nil:
		r.Status = "unmanaged (no longer in the package file; left as it is)"
	case unfinished(w.paths.Journal(name)):
		r.Status = "needs repair (run `gremloader repair`)"
	case !pkg.Enabled:
		r.Status = "disabled"
	case rec != nil && rec.LastError != "":
		r.Status = "last check failed: " + firstLine(rec.LastError)
	case r.Installed == "":
		r.Status = "not installed"
	case r.Latest == "":
		r.Status = "not checked yet"
	case newer(r.Latest, r.Installed):
		r.Status = "update available"
		if pkg.Hold {
			r.Status += ", held"
		}
	default:
		r.Status = "up to date"
	}
	if pkg != nil && pkg.Pin != "" {
		r.Status += ", pinned to " + pkg.Pin
	}
	return r
}

// unfinished: an interrupted change that repair must finish or undo. A committed journal is not
// one (the next run tidies it).
func unfinished(dir string) bool {
	if !journal.Begun(dir) {
		return false
	}
	l, err := journal.Read(dir)
	return err != nil || !l.Committed()
}

func newer(a, b string) bool {
	va, errA := ver.Parse(a)
	vb, errB := ver.Parse(b)
	return errA == nil && errB == nil && va.Compare(vb) > 0
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 100 {
		s = s[:100] + "..."
	}
	return s
}

// ---- explain: a package's effective settings and where each came from ----

type setting struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Origin string `json:"origin"`
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
}

func defineExplain(*flag.FlagSet) func(*call, []string) int {
	return func(c *call, args []string) int {
		if len(args) != 1 {
			return c.usagef("name one package")
		}
		w, code := c.load()
		if w == nil {
			return code
		}
		pkg := w.cfg.Packages[args[0]]
		if pkg == nil {
			return c.failf(exitcode.Usage, "package %q is not in %s", args[0], w.file)
		}
		var out []setting
		for _, s := range pkg.Explain() {
			out = append(out, setting{Key: s.Key, Value: s.Value, Origin: s.Origin.String(), File: s.Origin.File, Line: s.Origin.Line})
		}
		if c.g.json {
			c.writeJSON(jsonOut{Command: c.name, Items: out})
			return exitcode.OK
		}
		say(c.io.Stdout, "%s (from %s)\n", args[0], w.file)
		rows := make([][]string, 0, len(out))
		for _, s := range out {
			rows = append(rows, []string{"  " + s.Key, "= " + s.Value, "# " + strings.Replace(s.Origin, w.file, filepath.Base(w.file), 1)})
		}
		table(c.io.Stdout, rows)
		return exitcode.OK
	}
}

// ---- log: read events.jsonl ----

func defineLog(fs *flag.FlagSet) func(*call, []string) int {
	pkg := fs.String("pkg", "", "only events about this package")
	since := fs.String("since", "", "only events after this: a duration (24h, 90m) or a date (2026-10-01, RFC 3339)")
	return func(c *call, args []string) int {
		if len(args) != 0 {
			return c.usagef("unexpected argument %q", args[0])
		}
		var from time.Time
		if *since != "" {
			t, err := parseSince(*since, c.d.clock.Now())
			if err != nil {
				return c.usagef("--since: %v", err)
			}
			from = t
		}
		w, code := c.load()
		if w == nil {
			return code
		}
		evs, skipped, err := readEvents(w.paths.EventsLog())
		if err != nil {
			return c.failf(exitcode.Failed, "%v", err)
		}
		var keep []events.Event
		for _, ev := range evs {
			if (*pkg == "" || ev.Package == *pkg) && (from.IsZero() || !ev.Time.Before(from)) {
				keep = append(keep, ev)
			}
		}
		if c.g.json {
			c.writeJSON(jsonOut{Command: c.name, Items: keep})
			return exitcode.OK
		}
		for _, ev := range keep {
			say(c.io.Stdout, "%s\n", formatEvent(ev))
		}
		if skipped > 0 {
			say(c.io.Stderr, "(%d unreadable line(s) skipped)\n", skipped)
		}
		return exitcode.OK
	}
}

// readEvents reads the rotated files (oldest first) and then the live one. A missing file is no
// events.
func readEvents(live string) ([]events.Event, int, error) {
	var all []events.Event
	skipped := 0
	var files []string
	for i := 9; i >= 1; i-- {
		files = append(files, fmt.Sprintf("%s.%d", live, i))
	}
	files = append(files, live)
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // G304: GremLoader's own log files under the state folder
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		evs, n, err := events.Scan(bytes.NewReader(data))
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", f, err)
		}
		all, skipped = append(all, evs...), skipped+n
	}
	return all, skipped, nil
}

func parseSince(s string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is neither a duration (24h) nor a date (2026-10-01)", s)
}

func formatEvent(ev events.Event) string {
	parts := []string{ev.Time.Local().Format("2006-01-02 15:04:05"), fmt.Sprintf("%-5s", ev.Level), ev.Event}
	if ev.Package != "" {
		parts = append(parts, ev.Package)
	}
	switch {
	case ev.From != "" && ev.To != "":
		parts = append(parts, ev.From+" -> "+ev.To)
	case ev.To != "":
		parts = append(parts, ev.To)
	case ev.Version != "":
		parts = append(parts, ev.Version)
	}
	for _, f := range []string{ev.Step, ev.Result, ev.Code} {
		if f != "" {
			parts = append(parts, f)
		}
	}
	line := strings.Join(parts, " ")
	if ev.Msg != "" {
		line += ": " + indent(ev.Msg)
	}
	return line
}

// ---- check: resolve only; exit 2 when there is something to update ----

func defineCheck(fs *flag.FlagSet) func(*call, []string) int {
	now := fs.Bool("now", false, "ignore cool-down periods")
	return func(c *call, args []string) int {
		w, code := c.load()
		if w == nil {
			return code
		}
		s, code := c.open(w)
		if s == nil {
			return code
		}
		defer s.close()
		p, err := s.e.Plan(context.Background(), engine.Request{Packages: args, IgnoreCooldown: *now})
		if err != nil {
			return c.failf(planErrCode(w, args), "%v", err)
		}
		return c.report(p.Items, p.Warnings, p.ExitCode(true))
	}
}

// planErrCode: a package the file does not name is a wrong command line (exit 4).
func planErrCode(w *world, names []string) int {
	for _, n := range names {
		if w.cfg.Packages[n] == nil {
			return exitcode.Usage
		}
	}
	return exitcode.Failed
}
