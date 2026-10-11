package engine

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/enls/gremloader/internal/proc"
	"github.com/enls/gremloader/internal/state"
)

// Shared folders (shared_dir, ADR 0012): a folder such as ~/.local that several packages and the
// user install into. It need not be empty for a first install, its top-level folders are never a
// package's (manifestOf), it is never deleted whole (remove), and only a package's own files tie
// it to a running program (running). Paths are compared without case everywhere here, as the
// executor does: on a case-sensitive file system that only refuses or waits more often.

// running lists the programs that stop a change to dir. A folder of its own belongs to the package,
// so any program started from it counts; in a shared folder only programs started from one of the
// package's files do (files: "/"-separated, relative to dir).
func (e *Engine) running(dir string, shared bool, files []string) ([]proc.Process, error) {
	procs, err := e.cfg.Running(dir)
	if err != nil || !shared {
		return procs, err
	}
	own := map[string]bool{}
	for _, f := range files {
		own[strings.ToLower(f)] = true
	}
	var out []proc.Process
	for _, p := range procs {
		if rel, err := filepath.Rel(dir, p.Path); err == nil && own[strings.ToLower(filepath.ToSlash(rel))] {
			out = append(out, p)
		}
	}
	return out, nil
}

// skipRunning marks it skipped because procs are running from its folder.
func skipRunning(it *Item, procs []proc.Process) {
	var names []string
	for _, p := range procs {
		names = append(names, fmt.Sprintf("%s (pid %d)", filepath.Base(p.Path), p.PID))
	}
	it.Action, it.Result = ActNone, ResultSkipped
	it.Reason = "running: " + strings.Join(names, ", ") + "; close it and run again"
}

// ownFiles is every file of rec's installed version and of the version a rollback would return
// to: what a change to it may touch.
func (e *Engine) ownFiles(rec *state.Package) ([]string, error) {
	var out []string
	for i, rel := range []string{rec.Manifest, previousManifest(rec)} {
		if rel == "" {
			continue
		}
		m, err := state.LoadManifest(state.Path(e.cfg.Paths.State, rel))
		if err != nil {
			if i == 1 {
				return nil, fmt.Errorf("the rollback version's manifest: %w", err)
			}
			return nil, fmt.Errorf("the installed version's manifest: %w", err)
		}
		for _, f := range m.Files {
			out = append(out, f.Path)
		}
	}
	return out, nil
}

func previousManifest(rec *state.Package) string {
	if rec.Previous == nil {
		return ""
	}
	return rec.Previous.Manifest
}

// ownedByOthers refuses to place files (relative, "/"-separated) that the file list of another
// package installed in the same folder names: removing either package would then delete the
// other's file. It runs before anything is changed.
func (e *Engine) ownedByOthers(name, dir string, files []string) error {
	want := map[string]string{}
	for _, f := range files {
		want[strings.ToLower(f)] = f
	}
	for _, other := range slices.Sorted(maps.Keys(e.st.Packages)) {
		rec := e.st.Packages[other]
		if other == name || rec.Version == "" || rec.Manifest == "" || !strings.EqualFold(filepath.Clean(rec.Dir), filepath.Clean(dir)) {
			continue
		}
		m, err := state.LoadManifest(state.Path(e.cfg.Paths.State, rec.Manifest))
		if err != nil {
			return fmt.Errorf("the file list of %s, installed in the same folder: %w", other, err)
		}
		var clash []string
		for _, f := range m.Files {
			if p, ok := want[strings.ToLower(f.Path)]; ok {
				clash = append(clash, p)
			}
		}
		if len(clash) == 0 {
			continue
		}
		slices.Sort(clash)
		shown := strings.Join(clash[:min(len(clash), 5)], ", ")
		if len(clash) > 5 {
			shown += fmt.Sprintf(" and %d more", len(clash)-5)
		}
		return fmt.Errorf("%s would replace %s, which belongs to %s (installed in the same folder %s); nothing was changed",
			name, shown, other, dir)
	}
	return nil
}
