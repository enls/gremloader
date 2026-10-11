# Test fixtures

Bytes captured from real releases, so the parsers and extractor are tested against what the world
actually publishes and not against what we assume it does. These files are marked `-text` in
`.gitattributes`: Git must never normalise them.

| Path | Origin | Notes |
|---|---|---|
| `craftwerk-v0.2.0/SHA256SUMS.txt` | `enls/craftwerk` release v0.2.0, captured as published | Binary-mode lines (`<hash> *<name>`), LF endings |
| `craftwerk-v0.2.0/CraftWerk-Manager.zip` | same release, captured as published | A real zip (8 files, nested folders, a space in two names). Its hash matches the SHA256SUMS entry |
| `craftwerk-v0.2.0/CraftWerk-Manager.tar.gz` | **derived**: the zip above, unpacked and repacked with GNU tar (`--sort=name`, fixed mtime, owner 0) and `gzip -n` | Not published by anyone. No real Craft tarball is small enough to commit (the app tarballs are ~70 MB) |
| `photocraft-v0.6.0/SHA256SUMS.txt` | `storytold/photocraft` release v0.6.0, captured as published | Text-mode lines (`<hash>  <name>`, two spaces), 23 assets' worth of names, LF endings |

The large release assets themselves are deliberately not here. Tests that need an archive of a
specific shape build one in memory.

## Added for Phase 1 (M0)

| Path | Origin | Notes |
|---|---|---|
| `photocraft-v0.6.0/release-latest.json` | `GET api.github.com/repos/storytold/photocraft/releases/latest`, captured as returned (2026-10-10) | 23 assets, each with `digest` and `state: "uploaded"`; `created_at` precedes `published_at` (assets are uploaded in between) |
| `photocraft-v0.6.0/releases.atom` | `github.com/storytold/photocraft/releases.atom`, captured 2026-10-10 | **Trimmed:** every `<content>` element (release notes, ~1 MB) was replaced by a comment; everything else is byte-exact. 8 entries, newest first, prereleases included, no prerelease flag |
| `ghcli-v2.102.0/gh_2.102.0_checksums.txt` | `cli/cli` release v2.102.0 asset, captured as published (1,971 bytes) | A real, attested asset: its digest is `sha256:afe49e9a...e8d`. Used for native attestation verification in M8 |
| `ghcli-v2.102.0/gh_2.102.0_checksums.txt.bundle.sn` | the `bundle_url` payload for that asset, captured as returned | **Snappy-compressed** Sigstore bundle JSON (`application/x-snappy`); needs a snappy decoder (M8) |
| `ghcli-v2.102.0/attestations-response.json` | `GET api.github.com/repos/cli/cli/attestations/sha256:...` | Shows `bundle: null` + `bundle_url`. The signed query string of `bundle_url` was replaced by `?REDACTED` |

## Added for M8 (captured 2026-10-11)

The bundle above (`gh_2.102.0_checksums.txt.bundle.sn`) turned out to be GitHub's **release
attestation** (`initiator: github`, predicate `https://in-toto.io/attestation/release/v0.2`, signed
by GitHub's own Sigstore instance, one bundle listing all 23 assets), not build provenance.

| Path | Origin | Notes |
|---|---|---|
| `ghcli-v2.102.0/gh_2.102.0_windows_amd64.zip.provenance.sn` | the second `bundle_url` of `GET .../attestations/sha256:ae64e556...67f4` (the Windows amd64 zip's digest), as returned | **Build provenance** (`initiator: user`), snappy block format: public-good Fulcio, a Rekor entry, predicate `https://slsa.dev/provenance/v1`, SAN `https://github.com/cli/cli/.github/workflows/deployment.yml@refs/heads/trunk`. The 13 MB zip itself is not kept: tests use its digest |
| `ghcli-v2.102.0/attestations-zip-response.json` | that API response | Two items: the release attestation (the same bundle as the checksum file's) and the provenance. Query strings replaced by `?REDACTED` |
| `sigstore/trusted_root.json` | target `trusted_root.json` of Sigstore's public-good TUF repository (`tuf-repo-cdn.sigstore.dev`), fetched through sigstore-go v1.3.0's TUF client, byte-exact | SHA-256 `6494e21e...0b66`. Verification checks certificates at the bundle's own timestamps, so this root does not go stale for these bundles |

Facts these fixtures pin down (see the Phase 1 plan, E.8): GitHub release assets carry `digest`
and `state`; the author's `SHA256SUMS.txt` and GitHub's `digest` agree for every PhotoCraft
asset (checked by a test); Craft apps publish no attestations, cli/cli does.
