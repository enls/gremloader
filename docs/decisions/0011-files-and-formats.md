# 0011. Files and formats: package file, local override, lock, state, run lock

Status: accepted (2026-10-11)

## Context

GremLoader keeps four kinds of file, each with one job (ADR 0004): what you want
(`gremloader.toml`, hand-written, committed), this machine's pins and holds
(`gremloader.local.toml`, machine-written, not committed), what was resolved (`gremloader.lock`,
machine-written, committed), and what is installed here (`state.json` plus one manifest per
installed version). A run must also be able to tell that another run is in progress.

The formats were sketched in the Phase 1 plan (E.2). This record fixes the rules the plan left
open and the reasons for them. Packages: `internal/pkgfile`, `lock`, `state`, `jsonfile`,
`atomicfile`, `runlock`, `platform`.

## Decision

**Every file carries `schema_version`.** It is read first. A file from a newer GremLoader is
refused with "update GremLoader", before any of its new keys can be complained about. Additive
keys do not bump the version: an older binary rejects them as unknown, which is the safe
direction. Breaking changes do.

**Unknown keys and fields are errors, in every file.** In TOML they are reported with line and
column (go-toml's strict mode). In JSON an older binary that silently dropped unknown fields on
rewrite would lose data. Every problem in a file is reported in one run, in line order.

**Package file layering**, least to most specific: `[settings]`, local `[settings]`, the preset
named by `use`, `[package.<name>]`, local `[package.<name>]`. The most specific layer that sets a
key wins. Tables merge per key (one platform's `asset` can be overridden); a plain value at a more
specific layer covers every platform; lists replace. `use`, `repo` and `pin` belong to a package.
A preset cannot use another preset. Local `[settings]` sits directly above the file's
`[settings]` and below presets: a machine-wide default should not override what a preset or
package states explicitly. `explain` shows which layer won.

**The local override's shape is its whitelist**: `pin hold channel cooldown_days enabled` per
package, `channel cooldown_days` under `[settings]`. Strict decoding makes anything else an error
with its line, so a command, URL or path can never be overridden locally. A local entry for a
package the file no longer has is a warning, not an error. The file is rewritten whole (comments
are not kept: it is machine-written), with a header, in sorted order.

**Templates** (`{name}`, `{Name}`, `{version}`, `{tag}`, `{os}`, `{arch}`, `{asset}`, `[vars]`)
are parsed when the file is loaded, so a typo fails with its line instead of at update time. Each
setting has its own variable whitelist: `dir` and `version_from_exe` cannot use `{version}` or
`{tag}` (an install folder that moves with every update leaves the old one behind); `{asset}`
exists only in `checksums`. User variables may not redefine a built-in; `--root` overrides
`[vars] root`. "Escaping" means:

- a substituted value is text and is never expanded again (`{{` and `}}` are literal braces);
- in a file name (`asset`, `checksums`) a substituted value containing `/ \ * ? [ ]` is refused,
  not escaped: the asset matcher has no escape syntax, and no real release needs one. A wildcard
  the author writes in the template itself is allowed (exactly one asset must match);
- hook arguments are argv elements: a space or an apostrophe in a value stays inside one
  argument, and no shell is involved;
- `dir` must expand to an absolute path; a leading `~/` is the home directory (`~user` is not
  supported); `version_from_exe` must stay inside the install directory.

**Value spellings chosen here** (the plan named the keys, not all the values):
`hook_timeout` is a duration string (`"90s"`, `"5m"`, 1 s to 1 h). `limits.max_download` and
`max_extracted` take bytes or a binary size (`"500MiB"`, `"2GiB"`, up to 64 GiB).
`verify.attestation` is `if-present` or `off`; requiring an attestation is
`verify.require = [..., "attestation"]`, one way to say it. `strip` is `"auto"` or 0-32.
Platform keys are Go's names (`windows/amd64`, `linux/arm64`), and an unknown OS or architecture
is an error: a typo like `window/amd64` would otherwise never match and never say so.
`platforms` defaults to the keys of the `asset` table; a single asset string names no platforms
and `lock` must be told.

**Verification cannot be switched off by omission.** `verify.require` defaults to `["checksum"]`,
and a package without `checksums` is an error unless `verify.require = []` is written, which
loads with a warning.

**Lock file**: JSON (Q2). `encoding/json` writes map keys sorted, `generated_at` is UTC whole
seconds, so the same content gives the same bytes. Each asset URL must be the stable release
download URL: a query, fragment or credentials are refused, so a signed, expiring CDN link can
never be committed. Which hosts may be contacted is decided again at install time by the download
policy, not by the lock. An asset entry may carry `"verified_by": "hash-override"` (added in M6,
decided with the user 2026-10-11; no release had shipped, so no schema bump): its hash was
accepted with `--accept-sha256` although the release's checksum file said otherwise, and a
reviewer of the lock should see that. An override is written only into a lock that already names
the package at that version; it never adds a package or changes a version. `gremloader lock`
reads releases through the GitHub API (every platform's asset list and size is only there) and
takes each hash from the checksum file and GitHub's digest, which must agree.

**State**: a missing `state.json` is an empty state; a damaged one is an error, never an empty
state (that would make every install folder look like someone else's). A record with
`version = ""` is "checked, not installed". Every stored path the engine will act on (manifest,
undo log) is relative to the state directory, `/`-separated, and checked to stay inside it, so
a hand-edited state cannot aim a rollback elsewhere. Manifests are separate files
(`manifests/<pkg>/<version>.json`), sorted, with their own `schema_version`.

**Writes are atomic**: temporary file in the same directory, flushed, renamed over the target,
directory flushed on Linux. On Windows the rename is retried for about 2.5 s, because Go opens
files without delete sharing and a concurrent reader blocks the rename until it closes. A crash
leaves a `.<name>.*.tmp` that readers ignore and a later write removes once it is 10 minutes old.

**Run lock**: a kernel lock on `<state>/run.lock` (`flock`; `LockFileEx` on one byte far past the
end of the file, so the run that is turned away can still read who holds it). The kernel releases
it when the holder exits however it exits, so there are no stale locks. Acquire never waits (exit
code 3). The PID, version and start time in the file are for the message only. `run.lock` is
never deleted.

## Reasons

- Strictness everywhere costs an older binary the ability to read a newer file, and buys that no
  file is ever half-understood and then rewritten without what it did not understand.
- Line numbers on semantic errors matter because the package file is written by hand: "unknown
  preset" without a line in a twelve-package file is a search.
- The whitelist-by-shape for the local file makes the safety property ("a local override cannot
  change what runs") a consequence of the type, not of a check someone can forget.

## Dependencies

- `github.com/pelletier/go-toml/v2` v2.4.3: TOML with a strict mode that reports positions, and a
  parser (`unstable`) that gives every key a line. BurntSushi/toml reports unknown keys without
  positions. The `unstable` package may change between minor versions; it is used only for
  positions and is covered by tests that check line numbers.
- `golang.org/x/sys` v0.49.0: `flock` and `LockFileEx`. The standard library's file locking is
  internal. Also needed later for process enumeration and PE versions (M5).

## Consequences

- A package file written for a newer GremLoader stops older ones with a clear message rather than
  half-applying it.
- Hand-editing `state.json` is possible but validated: a bad edit is refused, not trusted.
- Comments in `gremloader.local.toml` do not survive a `pin` or `hold`.
