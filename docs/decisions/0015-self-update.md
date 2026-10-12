# 0015. Self-update: GremLoader as a built-in package of its own repository

Status: accepted (2026-10-11)

## Context

The plan's last Phase 1 milestone (M9, Q16) is `gremloader self-update`: replace the running binary with a newer release of `enls/gremloader`, through the same change-set engine as every other package (ADR 0012), and only when the release's build-provenance attestation passes (ADR 0014). Our own releases are attested by `.github/workflows/release.yml`; the first one is `v0.0.2-rc.1`.

Release candidates are drafts until a person has run them on real machines (`docs/releasing.md`). A draft is invisible to anyone without push access, and GitHub's "latest release" skips drafts and pre-releases, so a release candidate can only be found by self-update once it is published.

On Windows the running `.exe` cannot be replaced or deleted, but it can be hard-linked and renamed (checked 2026-10-11 on Windows 11: `os.Rename(new, running)` = "Access is denied", `os.Link` and `os.Rename(running, running+".old")` work, and the new file can then be renamed into place). Linux replaces a running binary by rename without complaint.

## Decision

**A built-in package.** `self-update` does not read the package file. It parses a TOML text embedded in the binary (`internal/cli/self.toml`) with the ordinary parser: repository `enls/gremloader`, the four archives `scripts/build-release.sh` produces, `SHA256SUMS.txt`, `verify.require = ["checksum", "attestation"]`, `verify.signer_workflow = ".github/workflows/release.yml"`, `strip = "auto"`, and `dir = "{root}"` with `{root}` set to the folder of the running binary (links resolved) and `shared_dir = true`, since that folder may hold other things (`~/.local/bin`, `C:\Tools`). The whole archive is installed: the binary, the README and both licences.

**Release candidates are published as pre-releases.** After its manual run, an rc's draft is published as a GitHub pre-release (never "latest"). Self-update considers pre-releases when the running binary is itself one (its version has a semver pre-release part) or with `--prerelease`; a stable binary sees only stable releases. An rc therefore moves to the next rc or to the final release, and from then on stays on stable releases. The pre-release channel needs the API (one request without a token; ADR 0009).

**Its own state, the shared run lock.** The self package's engine keeps its state under `<state>/self` (state.json, journal, manifests, previous): `gremloader` never shows up among the user's packages, never as "unmanaged", and cannot clash with a user package of that name. The run lock and the event log are the main ones, so a self-update and a scheduled apply never overlap.

**The running binary is the truth.** Self-update needs its own version to be a semver version: a development build (`dev`) refuses. When the self record disagrees with the running binary (another version or another folder: the user replaced or moved it by hand) the record is dropped and the binary is treated as a first install into a shared folder. A first install compares the release with the running version: equal is "up to date", lower is refused unless `--version` and `--allow-downgrade` ask for it. A binary not named `gremloader` (`gremloader.exe`) refuses: the archive would put a second copy beside it.

**Rename-aside on Windows.** The engine is told the running binary's path (`Config.Self`). When a change places a file there on Windows, the executor backs the old file up as usual, then renames it to `<name>.old` (`.old.1` .. `.old.9` when that name is taken), then puts the new file in place. The aside rename is not journaled: undo restores the backup to the path, which then holds nothing or the new, not running, file. Every start of GremLoader removes `<exe>.old` and `<exe>.old.N` beside itself, ignoring errors.

**Unfinished self-updates.** `self-update` repairs its own journal (finish or undo) before it plans; the `repair` command does not look under `<state>/self`.

## Reasons

- One engine for every change: the journal, undo, repair, hash and attestation checks, the running check (which already leaves out our own process) apply unchanged.
- An embedded package needs no package file, so self-update works on a machine that has none, and the user cannot weaken its verification by accident.
- Publishing rcs as pre-releases keeps the "draft until tested" rule and needs no token.

## Consequences

- A rc must be published (as a pre-release) before any self-update can see it; until then only a manual download installs it.
- `ownedByOthers` (ADR 0012) does not see user packages installed in the same shared folder, so a user package that also installs `README.md` there is not caught.
- A binary in a folder the user cannot write (`/usr/local/bin`) cannot update itself; the README recommends `~/.local/bin` on Linux.
- `self-update --rollback` is not provided yet; the engine could do it.

## Rejected alternatives

- Listing draft releases with the user's token: needs a token with push access on every machine.
- Installing only the binary: needs a file filter in the engine; the user chose the whole archive.
- Keeping the self record in the main state.json under a reserved name: `apply`, `list`, `remove` and `rollback` would all need exceptions for it.
