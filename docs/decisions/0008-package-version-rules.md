# 0008. Package version rules (`ver`)

Status: accepted (2026-10-10)

## Context

GremLoader compares the version of installed software with release tags. Real inputs are messy:
`v0.6.0`, `0.6.0.0` (a Windows file version), `v0.1.1-rc.5`, and tags that are not versions at all.
ADR 0005 says versions are typed and compared per backend; this ADR fixes the GitHub-release rules.

## Decision

- `internal/ver` implements **Semantic Versioning 2.0.0 precedence** exactly, including
  pre-release ordering and ignoring build metadata. It is separate from `internal/version`, which
  is GremLoader's own build identity.
- `Parse` is strict. `ParseLoose` additionally accepts a leading `v`, one or two numeric parts
  (padded with zeros), and a fourth numeric part that is exactly `0` (dropped). A non-zero fourth
  part is an error, because dropping it would make two different versions compare equal.
- A tag must pass `ValidTag` (letters, digits, `. _ + -`, at most 128 bytes, no `..`, no slashes or
  backslashes) before it is used in a URL or a path. Tags come from the network.
- A tag that does not parse is **skipped with a warning**, never guessed at and never a crash.
- Numeric pre-release identifiers longer than 64 bits are compared as numbers without parsing.

## Reasons

- Craft apps and GremLoader itself use SemVer tags with `-rc.N` pre-releases; ordering them by
  string or by "newest created" is wrong (`v0.1.1-rc.5` must sort below `v0.1.1`).
- The Windows `ProductVersion` read during adoption is four-part; accepting `.0` and refusing
  anything else keeps adoption honest.

## Verification

Table tests from the specification's own ordering chain, a **differential test** against
`golang.org/x/mod/semver` (5,000 random pairs; a *test-only* dependency used as an oracle), and a
fuzz target asserting that every accepted string round-trips and compares consistently.

## Consequences

- Distro-style versions (epochs, `~`) are out of scope here; the apt backend brings its own
  comparator (ADR 0005).
- Date-versioned projects need a different `Version` implementation later.
