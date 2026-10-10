# 0005. Version comparison belongs to the backend, not a global semver rule

Status: accepted (2026-10-10)

## Context

GitHub tags are usually semver but not always. apt uses dpkg ordering with epochs and revisions.
Docker images use tags and digests. Some projects use dates or build numbers.

## Decision

A version is a typed value with its own `Compare()`. Each backend supplies the implementation for the
versions it produces. There is no global "parse everything as semver" step.

## Reasons

- Assuming semver everywhere gets apt and date-versioned projects wrong in ways that surface as
  "no update available" or, worse, a downgrade presented as an update.
- Ordering is cheap to get right per scheme and easy to table-test.

## Consequences

- Comparing versions from different schemes is an error, not a guess.
- Pre-release handling (`channel`) is defined per scheme too.
- Fuzz and table tests for each comparator are part of the backend's definition of done.
- Digests have no order: "newer" for a digest is decided by the source (a tag moved), not by
  comparison.
