# 0004. UI edits go to a local override file, not into the package file

Status: accepted (2026-10-10)

## Context

`gremloader.toml` is hand-written and meant to be committed, with comments. The UI lets the user
change per-package settings (schedule, channel, cool-down, pin, hold).

## Decision

The UI never writes to `gremloader.toml`. It writes `gremloader.local.toml`, which overrides the
package file per package and is machine-local (git-ignored). The detail pane shows which value came
from which file.

## Reasons

- TOML libraries drop comments and reformat on write. Rewriting a hand-written file silently
  destroys the author's notes and produces noisy diffs.
- Separate files have separate jobs: what you want (`gremloader.toml`), what was resolved
  (`gremloader.lock`), what is installed here (`state.json`), what happened (`events.jsonl`).
- Settings a person changes on one machine should not leak into the shared file by accident.

## Consequences

- Merging is a defined step with its own tests: override wins per key, unknown keys are an error with
  a line number, and the effective value plus its origin can be explained.
- A "promote to package file" action may come later, as an explicit copy the user reviews.
