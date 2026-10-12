# 0006. Verify attestations natively with sigstore-go

Status: accepted (2026-10-10)

## Context

A checksum file next to a release proves the download matches the file, not who built it. GitHub artifact attestations (SLSA provenance via Sigstore) bind an asset to the workflow that built it. `gh attestation verify` can check them.

## Decision

Verify attestations inside GremLoader using `github.com/sigstore/sigstore-go`, when the source repository publishes them. Do not shell out to `gh`.

## Reasons

- `gh` is not installed on most target machines, and requiring it adds a second trust root and a second thing to keep updated.
- Verification is the security core of the tool; it should be testable in-process, with fixtures, and use the same code in the release pipeline's own self-check.

## Cost

- sigstore-go is the largest dependency in the module graph. That is accepted for this one job, and nothing else may pull it in. It is isolated behind a small `verify` package so it can be swapped.
- Trust roots must be handled (TUF metadata refresh, offline behavior). That is designed in Phase 1, not bolted on.

## Consequences

- Checksum verification remains the baseline for repositories without attestations.
- Whether a package requires an attestation is a per-package setting; a missing or failing one for a package that requires it is a hard failure, never a warning.
