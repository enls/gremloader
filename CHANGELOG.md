# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/),
and the project follows [Semantic Versioning](https://semver.org/). While the version is `0.x`, the
package-file format, the command line and the on-disk state may change between releases; every such
change is listed under **Breaking**.

## Unreleased

### Added

- Project foundation: licences (MIT or Apache-2.0), CI quality gates, release pipeline, test
  harness (fake release server, injectable clock, captured fixtures).
- `gremloader version` (and `version -json`).
- The package file `gremloader.toml` (presets, per-platform assets and folders, variables, hooks),
  the machine-local override `gremloader.local.toml` (pin, hold, channel, cool-down, enabled) and
  the lock file `gremloader.lock` (exact release, URL, hash and size per platform).
- Installing from GitHub releases: the release page and its feed first, GitHub's API only when
  needed (a token is used for the API only); channels, cool-down, pins and holds.
- Verification before unpacking: the release's checksum file (GNU, BSD and sidecar forms), GitHub's
  asset digest, the lock's hash. A mismatch is a hard failure (exit 5); the download is kept for
  inspection, and `--accept-sha256` lets one exact hash through.
- A strict archive extractor (zip, tar, tar.gz) that refuses unsafe names, links, devices and
  archive bombs, and unpacks nothing until the whole archive has been checked.
- Journaled changes: an install, update, rollback or remove that fails or is interrupted leaves
  the old version or the new one, never a mix; `repair` finishes or undoes an interrupted one.
  The user's own files in an install folder are never touched.
- Commands: `check`, `plan`, `apply`, `install`, `update`, `remove`, `rollback`, `verify`, `adopt`,
  `repair`, `pin`, `unpin`, `hold`, `unhold`, `lock`, `explain`, `log`, with `--json` and stable
  exit codes. A running program blocks only its own package's change.
- Hooks after install, update and remove (and before remove), run without a shell, with a timeout
  that stops everything they started.
- `shared_dir` for install folders several packages share, such as `~/.local`; two packages may
  never install the same file into one folder.
- `examples/craft/`: a package file for the twelve Craft apps, replacing CraftWerk.
- Build-provenance attestations (GitHub artifact attestations) verified natively with Sigstore
  before anything is unpacked: the asset must have been built by a workflow of the package's own
  repository, or a named one (`verify.signer_workflow`). `verify.attestation = "if-present"` (the
  default) or `"off"`; `verify.require` may list `attestation`. An attested package is not moved to
  a version without a passing attestation unless `--allow-attestation-regression` is given. The
  trust root comes from Sigstore's TUF repository and is cached for a day.
