# GremLoader

A single-binary package deployer: install software from GitHub releases, OS package managers and
Docker Compose, keep it updated on a schedule, and keep a log of everything it did.

> **Status: early, expect breaking changes.** GremLoader is at `0.x`. The package-file format, the
> command line and the on-disk state can all change between releases, and every breaking change is
> listed in the changelog. Nothing here is promised stable until `1.0`.

GremLoader is the successor to [CraftWerk](https://github.com/enls/craftwerk), a prototype that
installs and updates one fixed set of apps. GremLoader generalises that: a declarative package file,
exact versions and checksums in a lock file, and a plan-then-apply workflow.

## What it is meant to do

- **One package file** (`gremloader.toml`) describing what you want, plus a lock file of exact
  versions, URLs and checksums.
- **Several sources** behind one interface: GitHub releases, apt / dnf / winget, Docker Compose.
- **Safe by default:** checksums and attestations verified before anything is unpacked, a cool-down
  before adopting a new release, staged installs with automatic restore on failure.
- **Plan, then apply:** every change is shown before it happens.
- **A local web UI** embedded in the binary (`gremloader ui`), and the same core for the CLI and for
  scheduled runs.
- **Logged:** one JSON event per line, so "what changed and when" is queryable.

## Current state

Phase 1 (GitHub releases) is built but not released: no binary is published yet, so build it
from source (below). It installs, updates, rolls back and removes software published as GitHub
release assets, on Windows and Linux:

- `check`, `plan`, `apply`, `install`, `update`, `remove`, `rollback`, `verify`, `adopt`,
  `repair`, `pin`/`unpin`, `hold`/`unhold`, `lock`, `explain`, `log`; `--json` on every command,
  stable exit codes (`gremloader help` lists them all).
- Downloads are checked against the release's checksum file (or GitHub's digest) before anything
  is unpacked; a mismatch is refused, never prompted.
- Every change is journaled: a failure or a crash at any point leaves the old version or the new
  one, never a mix (`repair` finishes an interrupted run). One version back is kept for `rollback`.
- Hooks (`post_install`, `post_update`, `pre_remove`, `post_remove`) run as argument lists, never
  through a shell, with a timeout.
- [`examples/craft/`](examples/craft/) is a working package file for the twelve Craft apps, with
  notes on moving over from CraftWerk.

Not there yet: attestation checks (next), updating GremLoader itself, the scheduled run, other
sources (apt, winget, Docker Compose) and the web UI.

What was tested: everything has automated tests, including CraftWerk's behaviours ported as a
scenario suite, and CI runs them on Windows and Linux. All of them run against a fake GitHub; a
run against the real one, on a copy of a real CraftWerk install, comes before the first release.

## Building

Requires Go (see `go.mod` for the version).

```sh
go build -o gremloader ./cmd/gremloader
./gremloader version
go test ./...
```

## Licence

Dual-licensed under either of

- [Apache License, Version 2.0](LICENSE-APACHE)
- [MIT license](LICENSE-MIT)

at your option.

Unless you explicitly state otherwise, any contribution intentionally submitted for inclusion in
GremLoader by you, as defined in the Apache-2.0 license, shall be dual licensed as above, without
any additional terms or conditions.
