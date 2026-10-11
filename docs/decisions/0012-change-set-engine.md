# 0012. The change-set engine: plan, apply, undo, repair

Status: accepted (2026-10-11)

## Context

Everything before M5 reads, downloads, checks or unpacks into a staging area. The engine is the
part that changes the user's install folders, which hold apps they use daily (CraftWerk's
`C:\CraftWerk`). The plan (E.3.2) asks for: no half-installed app after any failure, a crash at any
moment repairable, a rollback to the version before, and a package's own files never mixed up with
the user's. Packages: `internal/engine`, `journal`, `proc`, `peversion`.

## Decision

**Plan, then apply.** `Plan` resolves every package (ADR 0009's listing, then the resolver of plan
E.3.1 steps 3-9, a pure function with a table of cases), picks the asset and checksum file, and
refuses up front what cannot succeed: a required check that cannot be made, a non-empty folder
GremLoader does not manage (`--overlay` or `adopt`), a missing parent folder (`--create-dir`). It
notes drift (installed files changed since, by presence and size; never blocking) and records what
it learned in `state.json` (`latest_seen`, `last_check`, `last_error`). `Apply` carries out exactly
that plan, one package at a time; one failing does not stop the next.

**Every change goes through one executor and one journal.** An update, a rollback and a remove are
all lists of four operations (`mkdir`, `place`, `remove`, `rmdir`) run by the same code. Before
anything in the install folder changes, a journal is started (`state/journal/<pkg>/`), and each
operation is recorded by a write-ahead rule:

- `created` and `mkdir` are written *before* the act, so undo removes the path if it exists;
- `backup` and `removed` are written *after* the old file is saved and flushed, with its hash, so
  undo can trust the saved copy;
- each record is flushed before the next operation. No `placed` record is written (plan E.2.5
  had one): undo does not need it, finishing forward goes by the `state` step, and leaving it out
  halves the flushes per file.

**Saving the old file costs nothing on one volume.** The backup is a hard link
(`journal/<pkg>/b/NNNN`), and the new file then *replaces* the old name with a rename, so the old
bytes are never touched. Across volumes it is a flushed copy, and a new file is copied to a
temporary name beside the target and renamed over it, so the replace is still one step (P3-20).
A file that another program holds open for a moment (antivirus, Explorer's preview) is retried five
times over about three seconds (P3-13).

**Order of an update:** new folders (parents first), every new file, the old files the new version
no longer has (only when the old manifest is known: an adopted install keeps its unknown files),
then old folders that became empty (deepest first). A folder is GremLoader's to remove only if it
created it; the manifest lists those, and the user's own files in it keep it.

**Failure before the commit undoes the journal**, in reverse: created paths removed, saved files
renamed back, folders removed (if empty) or recreated. Undo is idempotent, so it can be run again
after it was itself interrupted: a saved file already moved back is recognised by its hash. If undo
fails too, the journal stays, the package is `needs-repair` (exit 6), and every later run skips
it until `gremloader repair` (decided with the user, 2026-10-10: no automatic repair at the start of
a run, so a recurring failure is not repeated silently every week). A committed journal that was
not yet tidied is *not* a reason to stop: its bookkeeping is finished automatically.

**Repair** finishes forward when the journal reached its `state` step (every file was in place;
the manifest and the state record to write are in the journal, so no network is needed), and
undoes otherwise. It also clears every staging area, since the run lock guarantees none is in use.

**Commit, then promote.** After the last operation: `step state`, the manifest, `state.json`, the
`commit` record. Then the journal becomes the package's rollback record (`previous/<pkg>/`; the
older one is moved aside first and deleted after, so a crash in between leaves something
`tidy` finishes). A same-version reinstall keeps the existing rollback record and drops its own;
a first install has nothing to roll back to. An `--overlay` install keeps its journal in
`previous/<pkg>/` without pointing state at it: it holds the user's files that the install
replaced, and `state.json`'s rollback record must name a version.

**The journal starts after staging**, not at the end of the preflight as plan E.3.2 says:
downloading, verifying and unpacking never touch the install folder, so a failure there only has
to delete the staging area, and the begin record can carry the final state record.

**Preflight** (nothing is written until all pass): an unfinished journal, a running program whose
executable lies in the install folder (`proc`, by path, not name; result `skipped`, not a failure:
an open app is normal for a scheduled run), and free space when the asset size is known (three
times the download plus 64 MiB; the real guarantee is that running out at any later point cleans
up).

**Parked decisions from the bug checks (plan E.9), decided with the user 2026-10-10:**

1. Overlapping tag prefixes in one repository: the longest prefix wins. A package's query excludes
   the longer prefixes of the other packages of the same repository; an equal prefix (two assets of
   the same releases) excludes nothing.
2. An archive with no files is refused by the extractor (`empty`, ADR 0010 amended), so an empty
   tree never replaces a working install.
3. A damaged zip is `RejectError{corrupt}`, a truncated tar.gz is `ErrCorrupt`: the engine's
   "corrupt download" check tests for both.
4. `Retry-After` is printed in rate-limit and status errors, saying when it was longer than the run
   waits.
5. A sidecar checksum file whose line names another file is accepted with a warning in the plan.

## Reasons

- One executor for every change means one place to get crash safety right, and one fault-injection
  suite proves it for all of them.
- A write-ahead journal with hard-link backups is cheap enough to run on every update (no copying)
  and exact enough to restore byte for byte.
- Refusing to auto-repair keeps a human in the loop for the rare failure that undo itself could
  not fix; the common failures undo themselves at once.

## Consequences

- An interrupted change blocks only its own package until `repair` runs.
- Old versions' bytes live in `previous/` as hard links; deleting an install folder by hand does
  not free that space until the next update replaces the record.
- A Windows path the archive's 200-byte cap allows can still exceed MAX_PATH once joined to the
  install folder; Go handles long paths itself, but other programs may not. Not checked yet.

## What was and was not verified

Verified by tests (Windows locally; Linux in CI): an update with replaced, unchanged, added and
removed files and folders, nested and explicit empty folders, the user's own files beside them;
**a failure injected at every fault point** of an update leaves exactly the old version (or, after
the commit, exactly the new one); **a simulated crash at every fault point**, then repair, leaves
exactly the old or exactly the new version with `state.json` agreeing; a failed undo leaves
`needs-repair` and a later repair restores; **a real process kill** at four points mid-update, then
repair; cancellation between operations; a running app; a hash mismatch and an empty archive
install nothing; a first install that fails removes the folder it made.

**Not verified:** two volumes for real (the cross-volume path is tested by making the first rename
fail as one would); the sharing-violation retry against a real antivirus; hard links on FAT or
network drives (the copy fallback is exercised by unit tests only); power loss, as opposed to a
process kill (flushes are in place, untested); Linux except through CI.
