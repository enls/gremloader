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

**Saving the old file copies nothing on one volume.** The backup is a hard link
(`journal/<pkg>/b/NNNN`), and the new file then *replaces* the old name with a rename, so the old
bytes are never written. They are read once: the backup's hash goes into its journal record, so
undo can recognise a file already moved back and rollback can refuse a damaged backup (a 150 MB
program costs one sequential read per update; corrected by the M5 bug check, which first said
"costs nothing"). Across volumes it is a flushed copy, and a new file is copied to a
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
undoes otherwise, so the forward rule needs "and not `abort`": every in-process undo first writes
an `abort` record, and if that undo is itself interrupted, repair undoes again rather than
finishing forward over a half-undone folder. (Without it, a `step state` record that reached the
disk but whose flush reported an error, followed by an undo that failed part-way, left state.json
naming the new version over a mostly old folder; found by the M5 bug check, 2026-10-11.) If the
`abort` record cannot be written, nothing is undone and repair later finishes forward, or undoes
when there was no `step state`. Failures while writing the manifest or `state.json` are never
undone: the files are all in place, so they leave the package for repair, which finishes forward.
Repair also clears every staging area, since the run lock guarantees none is in use.

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

**The begin record carries the package's state record before and after the change.** Finishing
forward writes "after"; a rollback restores "before", complete with tag, asset and verification
levels (plan E.2.4's `previous` names only a version). Because a rollback is itself a change, a
rollback of a rollback restores the newer version the same way.

**Rollback** (plan E.3.3) reads `previous/<pkg>/` backwards and runs it as a new change: files the
update created are removed (and saved), saved files are placed back as links or copies (the record
must stay whole until the rollback commits), folders are removed or recreated. Every saved file is
re-hashed against its record first; one that is missing or changed refuses the rollback with the
current install untouched (P3-3). The version rolled away from is added to `rejected`, so the next
scheduled run does not reinstall it (P3-2); a version installed or rolled forward to on purpose
leaves `rejected`. The rollback's own journal becomes the new `previous/`, so a second rollback goes
forward. `post_update` runs with `GREMLOADER_ACTION=rollback`.

**Remove** runs `pre_remove` (its failure stops the remove before anything is deleted), deletes the
manifest's files and the folders GremLoader made once empty, then `post_remove`, and forgets the
rollback record, manifests and rejections. An adopted package without a file list is refused
unless `--delete-dir`, which renames the folder aside and deletes it whole.

**Adopt** records an existing folder without touching a file: `--version` is trusted;
`version_from_exe` reads a Windows executable's ProductVersion (`peversion`, the value CraftWerk
read); `--verify` downloads that release, verifies it like an install, and records the folder only
if every file of the release is there with the same bytes, which also yields a manifest. **Verify**
re-hashes installed files against the manifest, or with `--remote` against the release itself, and
lists a quarantined download.

**Hash mismatch** (plan E.3.4, decided with the user 2026-10-10): a hard failure, exit 5, never a
prompt. The download is fetched once more first (transient corruption is the common cause), as is
an archive found damaged when nothing had verified its hash (one whose hash checked out is the
publisher's: the same bytes would come again). A final mismatch keeps the bytes, never unpacked,
in `state/quarantine/<pkg>/` (one per package, replaced by the next failure, at most 512 MiB, with
an `info.json` either way), and the message names every trusted hash and its source, the hash
received, the likely causes without claiming malice, and the exact command to accept it.
`--accept-sha256 <hash>` is honoured for one package and version per run, only when it equals the
bytes received, only for a mismatch (not, say, a checksum file that does not list the asset); it
is logged with code `hash-override` and recorded as `verified_by = ["hash-override"]`. A package
file cannot carry one. `--locked` mismatches need it too. Writing it into the lock is M6.

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

## Rejected alternatives

- **Automatic repair at the start of every run** (filed in decided-against).
- **A copy of the old folder as the rollback record** (CraftWerk's `previous\`): it copies every
  byte on every update and cannot say which files the update added.
- **Writing the journal from the end of the preflight** (plan E.3.2): steps 1-3 do not touch the
  install, so the journal would only record staging, which a failure deletes anyway.

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
install nothing; a first install that fails removes the folder it made. The same failure and crash
sweeps over **rollback** and **remove**. Rollback both ways with the full record restored;
update, rollback, update ends where the first update did and the next plan is empty; a damaged
rollback record refuses; an adopted install updated and rolled back is exactly what was adopted;
remove keeps the user's files; adopt by version, by executable version, and with `--verify`
(matching and not); verify locally and remotely; every hash-mismatch rule above.

**Not verified:** two volumes for real (the cross-volume path is tested by making the first rename
fail as one would); the sharing-violation retry against a real antivirus; hard links on FAT or
network drives (the copy fallback is exercised by unit tests only); power loss, as opposed to a
process kill (flushes are in place, untested); Linux except through CI.
