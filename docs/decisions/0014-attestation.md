# 0014. Attestation: build provenance from the package's own repository, checked natively

Status: accepted (2026-10-11)

## Context

ADR 0006 chose to verify GitHub artifact attestations in-process with sigstore-go. The plan puts
it in M8 (E.5), with the regression rule P3-10 and the levels of E.3.4. A checksum file and
GitHub's `digest` protect against corruption and transit tampering, not against someone who can
publish a release: they control both files. A build-provenance attestation ties an asset to the
workflow run that built it. Packages: `internal/attest` (the types the engine uses: policy,
result, "unreachable") and `internal/attest/sigstore`, the only importer of sigstore-go, which
only the command line imports. The engine, and everything that imports it, stays free of that
graph: when the engine imported it directly, every test binary that links the engine doubled in
size, and the hook tests, which start copies of their own binary, missed their 1.5 s window on
Windows (first start of a new executable).

What GitHub actually serves (checked 2026-10-11 against `cli/cli` v2.102.0):
`GET /repos/{owner}/{repo}/attestations/sha256:{digest}` works without a token (core quota,
60 an hour unauthenticated) and answers 404 when there is none. Each item has `bundle: null` and
a `bundle_url` on `tmaproduction.blob.core.windows.net`, a signed URL (`sig=`, about an hour)
whose payload is a Sigstore bundle compressed with snappy's block format. An asset can carry two
kinds: **build provenance** (`initiator: user`; signed by public-good Sigstore with a Fulcio
certificate for the workflow, a Rekor entry, predicate `https://slsa.dev/provenance/v1`) and
GitHub's **release attestation** (`initiator: github`; signed by GitHub's own Sigstore instance,
an RFC 3161 timestamp and no Rekor entry, SAN `https://dotcom.releases.github.com`, predicate
`https://in-toto.io/attestation/release/v0.2`, one bundle for every asset of the release).

## Decision

**What "attested" means.** A build-provenance bundle that sigstore-go verifies against the
public-good trusted root (one signed certificate timestamp, one transparency-log entry, one
observer timestamp: what `gh attestation verify` requires for that instance) for the asset's
SHA-256, and whose certificate says, the way `gh attestation verify --repo owner/name` checks it:

- OIDC issuer `https://token.actions.githubusercontent.com`;
- source repository `https://github.com/<owner>/<repo>` and owner `https://github.com/<owner>`
  (compared without regard to case, as gh does);
- SAN under `https://github.com/<owner>/<repo>/` (the workflow is in the same repository);
- predicate type `https://slsa.dev/provenance/v1`.

`verify.signer_workflow` (package file, optional) pins the workflow further, e.g.
`.github/workflows/release.yml`; it is matched like gh's `--signer-workflow`, anchored at both
ends, with the ref optional. One bundle that passes is enough.

**Bundles that are not build provenance are skipped, not failed:** a release attestation, and any
bundle not issued by public-good Fulcio (a private repository's attestations are signed by
GitHub's instance). Skipping cannot let a forged bundle through; at worst it makes the package
look unattested, which the regression rule then catches.

**When it runs.** In the apply's verify step (E.3.2 step 2), after the hash checks, before
anything is unpacked, for every download: install, update, `verify --remote`, `adopt --verify`.
`check` and `plan` never call it, so a plain weekly check still costs no API request.

**Outcomes.**

| Situation | `attestation = "off"` | `"if-present"` (default) | `require` lists `attestation` |
|---|---|---|---|
| A bundle passes | not checked | level `attestation`, recorded | level `attestation`, recorded |
| None published (404, or only skipped kinds) | not checked | no level, carry on | refused, exit 5 |
| Bundles published, none passes | not checked | refused, exit 5 | refused, exit 5 |
| API, bundle or trust root unreachable | not checked | warning, not recorded as attested | refused, exit 7 |

A refusal is a verify failure like a hash mismatch: nothing is installed, the current install is
untouched. It is not retried by downloading again (the bytes already matched their hash), and
`--accept-sha256` does not get past it (that answers a different question).

**Regression rule (P3-10).** When the installed version was recorded as attested and the new one
is not (none published, or it could not be checked), the change is refused (exit 5) unless
`--allow-attestation-regression` is given. `--yes` never covers it. This also closes "block the
attestation API, skip the check". Rollback downloads nothing and is not checked; the record it
restores keeps its own `attested`.

**The trust root** comes from Sigstore's TUF repository (`tuf-repo-cdn.sigstore.dev`) through
sigstore-go's client, bootstrapped from the root embedded in sigstore-go, cached under the cache
folder (`tuf/`), refreshed at most once a day. Without the network a cached root is used while its
metadata is valid; with neither, the check counts as unreachable. Tests never use the network:
they verify against a captured `trusted_root.json`.

**Network.** The API request goes through the existing client and policy (the token, when set,
only to `api.github.com`). The bundle is fetched with its own policy: GitHub's hosts plus
`*.blob.core.windows.net`, https only, never the token, at most 2 MiB compressed and 16 MiB
decoded. The bundle is verified cryptographically, so its host is reached, not trusted. The signed
query string is redacted wherever a URL is logged. The TUF fetcher may reach only
`tuf-repo-cdn.sigstore.dev`.

**Recorded.** `state.json` keeps `attested` per package and `attestation` in `verified_by`. The
lock file is unchanged: `apply --locked` still checks attestation live (the lock's hash stands in
for the checksum file and the digest, not for a build's provenance).

**Dependencies.** `github.com/sigstore/sigstore-go` v1.3.0 and `github.com/golang/snappy`
(no dependencies; sigstore-go's graph has none of its own in the build). Measured 2026-10-11: the
`go` line stays 1.27.2; the Windows binary grows from 13.5 MB to 29 MB; `go.mod` from 3 to about
70 requirements, 73 modules in the build. The weight comes from parsing the bundle itself
(`pkg/bundle` pulls Rekor's generated models, grpc, otel), so it cannot be trimmed while
sigstore-go reads the bundle. As pulled, govulncheck reported `golang.org/x/net` v0.57.0 (five
HTTP/2 advisories) and `google.golang.org/grpc` v1.82.1 (GO-2026-6348); both are raised to their
fixed versions (v0.60.0, v1.83.1). The user accepted this cost on 2026-10-11.

## Reasons

- The `gh --repo` identity is what our own release workflow checks after it attests (release.yml,
  "Verify the attestations just made"), so GremLoader and the pipeline mean the same thing.
- Requiring the workflow file by default would make every package file name one before it counted
  as attested; the repository is the boundary that matters for "built from this source".
- Public-good only: public repositories' build provenance is signed there; GitHub's instance would
  need a second TUF root (`tuf-repo.github.com`, only distributed inside `gh`) for statements that
  are either about private repositories or are release attestations.
- Skipping instead of failing on kinds we do not check keeps `if-present` usable for every
  repository, and the regression rule keeps the skip from being a hole.

## Consequences

- A package whose provenance is signed by a reusable workflow in another repository (for example
  `slsa-github-generator`) fails the SAN rule with `if-present` and is refused. Set
  `attestation = "off"` for it; a `signer_repo` key can be added when a real package needs it.
- One extra API request per downloaded package (a 404 for repositories that publish none, such as
  the Craft apps): 12 on a day all twelve update, within the anonymous 60 an hour.
- GitHub release attestations are not checked. They could become a level of their own later.
- Changing the bundle host or format on GitHub's side makes the check unreachable (warning, or
  exit 7 when required) until GremLoader is updated; it never makes a bad bundle pass.

## Rejected alternatives

- Allowing only the exact host `tmaproduction.blob.core.windows.net`: safer reach, but breaks on a
  rename on GitHub's side; the bundle is verified anyway. Chosen: the wildcard, for the bundle only.
- A workflow pin as the default identity; the owner-only identity (`gh --owner`): see Reasons.
- Using the `bundle` field only: GitHub sends `null` and a `bundle_url` today.
- Verifying GitHub's release attestation as "attested": it says the asset belongs to a release, not
  how it was built.
- Writing the Sigstore checks ourselves to avoid the dependency: certificate chains, SCTs,
  transparency-log proofs and DSSE in our own security-critical code (ADR 0006).
