# Decided against

Ideas that were considered and turned down, with the reasoning, so they are not re-litigated by
accident. Nothing here is deleted; if a decision is reversed, say so in the entry and link the ADR.

- **The WebUI library (webui-dev/webui).** The Go wrapper uses cgo (needs a C compiler, breaks
  one-command cross-compiling), requires an installed browser at runtime (headless servers have
  none), is still beta, and its Go v2 API is described as incomplete. Plain `net/http` + `go:embed`
  gives the same result in pure Go. See ADR 0003.
- **A terminal menu as the primary interface.** Chosen first, then replaced by the web UI. Possible
  later on the same core.
- **The UI writing changes back into `gremloader.toml`.** TOML libraries drop comments and
  reformat; UI edits go to `gremloader.local.toml`. See ADR 0004.
- **One file for desired state, lock and installed state.** Separate jobs, separate files.
- **Assuming semver everywhere.** apt, Docker and date-versioned projects don't follow it. See
  ADR 0005.
- **Loading htmx or fonts from a CDN.** Breaks offline use and the strict CSP; everything is
  embedded.
- **Shelling out to `gh` for attestation verification.** Chosen instead: native sigstore-go. Avoids
  requiring `gh` on every machine, at the cost of a larger dependency. See ADR 0006.
- **A `grem` short alias.** Taken by another repo-manager tool.
- **Treating the gangsheet app's npm dependencies as host dependencies.** They live in the image.
- **A y/n prompt on a hash mismatch ("this may be malicious, continue?").** Proposed 2026-10-10 and
  turned down in favour of a hard failure with `--accept-sha256 <hash>`. The scheduled run has no
  terminal, a bare "y" is answered by reflex, "may be malicious" overstates what is usually a corrupt
  download or a publisher who did not regenerate the checksum file, and the override should record
  *which bytes* were accepted. A prompt that defaults to N, only on a TTY, was considered a fallback
  but the hash form is the only way through. See the Phase 1 plan, E.3.4.
- **Escaping template values that contain wildcard characters** (so a `[vars]` value with `*` could
  appear in an asset name literally). The asset matcher (`path.Match` behind `SelectAsset`) refuses
  `\`, so there is no escape syntax to use, and inventing one for a case no real release has adds a
  rule to learn. A substituted value that would add `/ \ * ? [ ]` to a file name is refused instead.
  See ADR 0011.
- **Local `[settings]` overriding presets and packages.** A machine-wide default set in the UI
  would then silently beat a value the package file states for one package. It sits just above
  the file's `[settings]` instead; per-package local values still win over everything. See ADR 0011.
- **A wait-for-the-lock mode on the run lock.** A second run fails at once with exit code 3; a
  scheduler that wants to retry can do so. Waiting hides a hung run behind a silent one.
- **Repairing an interrupted change automatically at the start of every run.** Proposed while
  planning M5 (2026-10-10) and turned down with the user: a failure that undo itself could not fix
  (a file another program keeps locked, a full disk) would be retried silently every week. The
  package is skipped as `needs-repair` (exit 6) until `gremloader repair` runs; other packages
  still update. Leftovers of an already *committed* change are tidied automatically, since there
  is nothing to decide. See ADR 0012.
- **A full copy of the install folder as the rollback record** (what CraftWerk does with
  `previous\`). Every update would copy every byte, and the copy cannot tell which files the update
  added. The update's journal, with hard-link backups, is the rollback record instead. See ADR 0012.
- **Keeping an overlay install's overwritten files for the life of the install, or refusing to
  remove one.** Decided with the user after the M5 bug check (2026-10-11): `--overlay` says at
  install time that the files it overwrites are kept only until the next update, and remove names
  what it discards. A separate store restored on remove (`state/overlay/<pkg>/`) means a new store
  with its own crash handling. The main overlay case (a shared folder like `~/.local`) now has its
  own key, so the case is rare. Refusing remove behind a flag only adds a step. Revisit if it bites.
- **`overlay = true` in a preset, or installing Linux packages under `~/.local/share/<app>` with
  links into `~/.local/bin`, for a shared target like `~/.local`.** Decided with the user
  (2026-10-11): a package-file key `shared_dir = true` instead (never require the folder empty,
  never claim existing folders, never `--delete-dir`). Overlay means a one-time install over
  existing files and brings its backup rules into every install. The `share/<app>` layout is
  cleaner ownership but not what the Craft tarballs are built for.
- **Refusing, or ignoring, a Windows path over 260 characters.** Decided with the user
  (2026-10-11): the plan warns and does not block. GremLoader itself copes (Go uses long-path
  forms), and a machine with `LongPathsEnabled` should not be refused. Ignoring it leaves the app
  or a hook to fail later with no hint.
- **Leaving hooks out of repair entirely.** Decided with the user (2026-10-11): a change that
  repair finishes forward runs the `post_install`/`post_update` hook the failed run skipped. A
  crash after the commit but before the hook cannot be detected cheaply (nothing may follow the
  commit record), so that case only gets a note naming the hook to run.
- **Giving up an unfinished journal automatically when its install folder stays missing, or only
  documenting "delete the journal by hand".** Decided with the user (2026-10-11):
  `gremloader repair --forget <pkg>`, with confirmation. An automatic give-up could fire while a
  drive is just unplugged; deleting state files by hand is the kind of step GremLoader exists to
  remove.
- **Always refusing when a file becomes a folder between versions (or the reverse).** Decided
  with the user (2026-10-11): when the thing in the way is in the old manifest (ours), the update
  removes it first, journaled so undo restores it; only a user's own file in the way is refused,
  with a message naming it. Refusing always would leave that package unable to update, ever.
- **Clearing the read-only attribute on GremLoader's own files and replacing them.** Briefly
  chosen by the user (2026-10-11) over "only a clearer message", then **reversed the same day**:
  the message is simpler. Clearing the attribute also clears it on the hard-link backup (the bit
  belongs to the file, not the name), so undo and rollback would bring the file back without it
  unless the journal recorded the bit. And a user who set read-only may have meant it. The update
  fails as before (exactly the old version), with a message saying the file is read-only and how to
  clear it.
