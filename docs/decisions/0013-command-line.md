# 0013. The command line: one path for every change, confirmation, exit codes, JSON, hooks

Status: accepted (2026-10-11)

## Context

M6 puts a command line on the engine (ADR 0012) and runs package hooks for real. The plan fixes
the command table, the global flags and the exit codes (E.4), the hook rules (E.3.8) and the
interrupt behaviour (P3-12). It leaves open how confirmation, JSON output, the lock commands and
the hook runner behave in detail. Scripts and the scheduled run will depend on these, so they
are written down here. Packages: `internal/cli`, `internal/hooks`, and in the engine `lockgen.go`
and `Forget`.

## Decision

**Flags anywhere, no dependency** (Q14). A small pre-sorter moves every flag (and the value of a
value flag) ahead of the positional arguments before the standard `flag` package parses them;
`--` ends flags; the command is the first argument that is neither a flag nor a global flag's
value. Global flags: `--file --home --root --json --quiet --verbose --yes`. Help is generated
from the command table.

**Every change goes through one path:** plan, show the plan, confirm, apply, record in the lock,
report. `plan`, `apply`, `install` and `update` share it; `remove`, `rollback` and
`repair --forget` are confirmed the same way.

**Confirmation** (Q10). `--yes` confirms. Without it the plan is printed and a terminal must answer
`y` or `yes` (anything else changes nothing, exit 0). No terminal and no `--yes`: nothing changes,
exit 4. `--yes` is never a safety flag: an overlay (`--overlay`), a downgrade
(`--allow-downgrade`) and a hash override (`--accept-sha256`) each need their own flag. Commands
that change no file (`adopt`, `repair` without `--forget`, `pin`, `hold`, `lock`) are not
confirmed.

**Exit codes** (E.4, stable). Where the plan was silent:

| Situation | Exit |
|---|---|
| `check` found something to update | 2 |
| A version asked for on the command line and refused (a downgrade without `--allow-downgrade`) | 1 (decided with the user, 2026-10-11) |
| A pin below the installed version | 0, reported as skipped: it is configuration, and failing it would fail every scheduled run |
| `verify` finds a changed or missing file | 1 (5 stays for downloads that do not match) |
| Not confirmed at the prompt | 0 |
| No terminal and no `--yes`; an unknown package; a missing package file; wrong arguments | 4 |
| A package file that does not parse | 1 (its line and column are printed) |
| A second interrupt | 1 |

**JSON** (`--json`). Exactly one object on stdout, on every path including usage errors and
failures before any package result: `{"schema_version": 1, "command", "exit_code", "items",
"warnings", "error"}`. Fields are only ever added; a removal or a change of meaning bumps
`schema_version`. Prompts and messages go to stderr. `log --json` items are the events
themselves. `version --json` keeps printing the build-information object (unchanged since
Phase 0).

**The run lock** is taken by every command that builds the engine, including `check`, `plan` and
`verify` (a plan writes `state.json`; M5 bug check). `list`, `explain`, `log`, `pin`/`hold` and
`version` take none: `state.json` and `gremloader.local.toml` are replaced atomically, so they are
read whole or written whole.

**Hooks** (E.3.8). argv exactly as the package file wrote it, never a shell; the commands of one
event run in order and the first failure stops the rest. Working folder: the package folder, or
when it is gone (a `post_remove` after `--delete-dir`) the nearest folder above it that exists.
Environment: inherited plus `GREMLOADER_ACTION/PACKAGE/VERSION/PREVIOUS/DIR`. Stdin is empty.
`pre_remove` and `post_remove` both get the removed version as `VERSION` (and `{version}`,
`{tag}`), and an empty `PREVIOUS` (decided with the user after the M6+M7 bug check; before, a
`{tag}` in `pre_remove` failed the remove, and `post_remove`'s `PREVIOUS` repeated `VERSION`).
A timeout or an interrupt ends the hook **and everything it started**: on Windows the command is
created suspended, put in a job object, then resumed, so it cannot start a process outside the
job first; elsewhere it leads its own process group. A hook that finishes normally may leave a
program running (the job is closed without ending it). Output: the last 64 KiB is kept, logged as
`hook.output`, and its end is in the error; `--verbose` echoes it to stderr. A background process
holding the output pipe delays the run by at most 5 s.

**The lock.** `gremloader lock [pkg...]` writes `gremloader.lock` beside the package file (ADR 0011
has the format and the rules of `engine.Lock`). Locking the whole file drops entries for packages
it no longer names; locking named packages leaves the others alone. `--update-lock` (install,
update, apply) records what was just installed, at exactly those versions. `--locked` and
`--update-lock` together are refused. An accepted hash override goes into a lock that already
names that package at that version, marked `verified_by = "hash-override"`; an override alone
never creates a lock. Lock problems after a successful install are warnings, not failures.

**Interrupts** (P3-12). The first interrupt (Ctrl+C, Ctrl+Break, or SIGTERM elsewhere) cancels the
run: the engine stops between packages and undoes a change cut short, so every install is as it
was or as it should be. The second quits at once (exit 1) and names `gremloader repair`.

**The four M5 bug-check decisions built here** (recorded in ADR 0012): repair runs the hook a
finished-forward change skipped; `repair --forget <pkg>`; the overlay warning; the MAX_PATH note.

## Rejected alternatives

- `spf13/cobra` or `pflag` (Q14): a dependency for what a few dozen lines of pre-sorter do.
- Ending what a hook started when the hook succeeds (a job object with kill-on-close): a hook may
  start a program on purpose.
- Reading a confirmation from a non-terminal stdin: a scheduled run's stdin is whatever the
  scheduler connected, and a stray "y" there would apply a plan nobody saw.
- Hashing every platform's asset by downloading it when no checksum file entry or digest states
  its hash: up to 150 MB per platform for a command that should be quick; `lock` refuses instead
  and says why.

## Reasons

- One path for every change means one place where confirmation, the lock and reporting are right.
- A refused request that exits 0 reads as success to a script; a pin is configuration and must not
  fail every scheduled run.
- One JSON object on every path lets a script always parse stdout, whatever happened.
- Ending the whole tree on timeout is the only way a stuck hook cannot hold a run open; starting
  it suspended closes the moment in which it could escape the job.

## Consequences

- `--json` output is an interface: changing a field's meaning means `schema_version` 2.
- Hooks must tolerate a rare double run or miss (ADR 0012) and must not wait for input.
- An interrupt while the y/N prompt waits on stdin cancels the run, but the read goes on until
  Enter or a second interrupt.

## What was and was not verified

Verified on Windows (Go 1.27.2): the pre-sorter and command dispatch (table tests); the hook runner
with a real child (the test binary) in a folder and with arguments containing an apostrophe,
spaces, quotes, an empty string and trailing backslashes; the tree ended on timeout and on
interrupt and kept on success (a grandchild's heartbeat); every command end to end in-process
against the fake GitHub, including real hooks through a whole package life; 17 golden outputs;
one JSON object for 24 command lines; exits 2, 3, 4, 5, 6 and 7; the interrupt handler driven
through its channel. Mutation spot-checks on the confirmation gate, `--accept-sha256` rules, lock
writes, lifecycle commands and interrupts (counts in the commit messages).

Not verified: anything on Linux beyond `GOOS=linux` vet and lint (the process-group kill, the
terminal check and the goldens first run in CI); a real Ctrl+C in a console (the handler is tested
through its channel, the signal wiring is not); anything against the real GitHub (M7).
