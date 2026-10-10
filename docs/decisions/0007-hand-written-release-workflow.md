# 0007. A hand-written release workflow, not GoReleaser

Status: accepted (written 2026-10-10 during Phase 0, when the plan left it open; confirmed by the
project owner 2026-10-10).

## Context

Releases need: four targets (linux and windows, amd64 and arm64), archives, a checksum file, an SBOM,
build-provenance attestations, and a *draft* GitHub release that is tested by hand before it is
published. GoReleaser does all of that from one config file.

## Decision

Write the release pipeline by hand: `scripts/build-release.sh` builds and packages one target, and
`.github/workflows/release.yml` loops over the targets, checksums, attests and creates the draft.

## Reasons

- This is a learn-end-to-end project. The pipeline is part of what is being learned, and about 100
  lines of shell and YAML is small enough to understand completely.
- The script runs unchanged on a developer machine, so a release build can be reproduced and
  compared (the same commit gives the same archive bytes; checked locally for linux/amd64).
- One less third-party tool in the path that produces the artifacts users run, and one less action
  to pin and keep patched.
- What we need is narrow: no Homebrew tap, no Docker images, no package formats.

## Cost

- Archive naming, checksum format and the changelog are ours to maintain.
- If targets or package formats multiply (deb/rpm, a winget manifest), this should be revisited. A
  new ADR would supersede this one; GoReleaser can be used as a reference in the meantime.

## Consequences

- Archive layout is `gremloader-<version>-<os>-<arch>.tar.gz|zip` containing the binary, both licence
  files and the README.
- `SHA256SUMS.txt` is produced by GNU `sha256sum` (text mode, two spaces). The checksum parser must
  accept both that and the binary-mode `*name` form that CraftWerk published; both are in the test
  fixtures.
