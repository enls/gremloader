# Architecture decision records

Short records of decisions that are hard to reverse or easy to forget the reason for. One file per
decision, numbered, never rewritten: a changed decision gets a new record that supersedes the old
one. Turned-down ideas are filed in [../decided-against.md](../decided-against.md).

| # | Decision |
|---|---|
| [0001](0001-use-go.md) | Use Go |
| [0002](0002-github-releases-first.md) | GitHub releases are the first source backend |
| [0003](0003-plain-net-http-ui.md) | The web UI is plain net/http, server-rendered, embedded |
| [0004](0004-local-override-file.md) | UI edits go to a local override file |
| [0005](0005-per-backend-version-comparison.md) | Version comparison belongs to the backend |
| [0006](0006-native-attestation-sigstore-go.md) | Verify attestations natively with sigstore-go |
| [0007](0007-hand-written-release-workflow.md) | A hand-written release workflow, not GoReleaser |
| [0008](0008-package-version-rules.md) | Package version rules (`ver`) |
| [0009](0009-resolution-chain.md) | How a release is found and fetched: web first, API as fallback, one network layer |
| [0010](0010-extractor-policy.md) | What the archive extractor accepts: one policy for both OSes, reject not skip, two passes |
| [0011](0011-files-and-formats.md) | Files and formats: strict everywhere, layered package file, whitelisted local override, atomic writes, kernel run lock |
| [0012](0012-change-set-engine.md) | The change-set engine: one journaled executor for every change, undo and repair, rollback from the journal, hash mismatch is a hard failure |

Format: Context, Decision, Reasons, Consequences (and Rejected alternatives where there were any).
