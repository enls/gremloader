# 0009. How a GitHub release is found and fetched (the resolution chain)

Status: accepted (2026-10-10)

## Context

Every package update starts with "what is the latest release, and where are its files?". GitHub answers that in two ways: the REST API, and the website. The API is rate-limited to 60 requests an hour per IP address when unauthenticated, shared with everything else behind that address. Twelve packages checked weekly is twelve requests; two machines and a retry can exhaust it. Observed against the real service: a `304 Not Modified` still costs one unit unauthenticated, so ETag caching does not fix this.

Release assets download from `github.com`, which redirects to a signed, expiring URL on `*.githubusercontent.com`. That URL carries its credential in the query string.

## Decision

**Two sources, one `Listing`.** `internal/source/github` produces the same result from either:

| | web path | API path |
|---|---|---|
| Requests | `GET /{repo}/releases/latest` (redirect, not followed) and `GET /{repo}/releases.atom` | `GET /repos/{repo}/releases?per_page=30` |
| API quota | none | one unit |
| Gives | the tag GitHub calls "latest stable"; recent tags with publish times | prerelease flag, assets with `state` and `digest`, publish times |
| Cannot see | prerelease flag, asset list, asset state, digest | (nothing we use) |

**Choice (`api = "auto"`, the default).** Web first. API first only when a token is configured (5,000 an hour makes it cheap and it gives more) or the package needs API-only data (the prerelease channel). Each path falls over to the other on a rate limit, a server error or a network error. It does **not** fall over on "not found", on a policy refusal (a redirect to a foreign host), or when the web path cannot answer the question: a request that needs the prerelease flag fails with `ErrNeedsAPI` and says to set a token, rather than quietly installing a prerelease as if it were stable. `api = "api"` and `api = "web"` disable the fall-over.

**Zero quota on a plain update.** Listing, asset selection (an exact name from the package's template) and both downloads (asset and checksum file) use only `github.com`. A test asserts zero requests under `/api/`.

**The network layer (`internal/download`)** is the only code that opens a connection:

- https only; a redirect allow-list (`github.com`, `api.github.com`, `*.githubusercontent.com`) applied to the first URL and every hop; at most 5 redirects; TLS verification cannot be disabled.
- The token is sent only to hosts in `TokenHosts` (`api.github.com`), compared by **host and port**, and is removed again by our own redirect check. Go's HTTP client forwards credentials across a redirect when only the port differs; a test pins this.
- Connect, TLS and header timeouts of 10/10/30 s, plus a 30 s stall timeout on the body (no bytes for that long fails the attempt).
- Three attempts with exponential backoff and jitter on network errors, 5xx, 429 and a 403 that carries `Retry-After`; `Retry-After` is honored up to 60 s. An exhausted **primary** quota (`X-RateLimit-Remaining: 0` without `Retry-After`) is not retried: nothing within a run changes it, and waiting would only delay the fall-over. 404 and other 4xx fail at once.
- A size cap on every body; the announced length is checked before reading, and bytes are counted as they arrive. Bodies are hashed while streaming and written to `<dst>.part`, renamed only after a complete transfer; nothing is left behind on failure. No resume.
- Errors and logs carry URLs without their query string and credentials (`internal/redact`).
- One request at a time per run.

**Our own names, not theirs.** Tags must pass `ver.ValidTag` and asset names must contain no path separators or control characters before they are used. Download URLs are built from the repository, validated tag and escaped asset name; a `browser_download_url` from a response is never followed. Local file names are chosen by the engine, never taken from an asset name.

**One asset, exactly.** A package names its asset by a pattern (usually an exact template). Zero matches or more than one is an error that lists candidates; nothing is guessed. (PdfCraft publishes a second, near-identical `-cli-` tarball, so a loose pattern would be wrong half the time.) On the web path the asset list is unknown, so a plain name is trusted and a wildcard returns `ErrNeedsAPI`.

## Reasons

- A weekly check of twelve packages should not depend on a shared 60-per-hour budget.
- The web redirect is GitHub's own answer for "latest stable", including its prerelease rule, so nothing has to be reimplemented for the common case.
- Failing loudly on API-only questions is safer than answering them wrongly from the web path.
- The token rule is the one most likely to be broken by someone "simplifying" the client; it is enforced in one place and has a test that fails if our check is removed.

## Consequences and known limits

- The web path cannot see GitHub's prerelease flag on a tag that looks stable, so a prerelease that was tagged `v1.2.0` would be offered as stable. Only the API path is exact. A package that cares uses `api = "api"` or the prerelease channel (which requires the API).
- `releases.atom` lists about ten releases. A cool-down that has to fall back past the newest ten needs the API.
- `/releases?per_page=30` reads one page. A repository with more than thirty releases newer than the wanted one is not handled in Phase 1.
- Private repositories are not supported: the token is not sent to `github.com`, where the web path downloads from.
- The downloaded bytes are not trusted because they came from GitHub. Verification (checksum file, GitHub's `digest`, later attestation) is a separate step with its own policy.
- `digest` and `state` were observed on every asset of all twelve Craft repositories, but older releases of other projects may lack `digest`; it is treated as optional.

## Amendment (2026-10-10): corrections found by the review of M1 and M2

The decision stands. Four statements above were sharpened after a bug check; the code and tests follow these, not the sentences they replace.

- **"Does not fall over on not found."** That holds for a repository that does not exist and for a repository with no releases. It does *not* hold for "the feed has no usable tag": the feed lists only the ten most recent releases, so in a monorepo a package's own releases can have scrolled out of it. That case is a partial view and falls over to the API.
- **"Fails with `ErrNeedsAPI` and says to set a token."** `ErrNeedsAPI` now means exactly "the API was refused for quota". A bad token, an outage or a network error keep their own error, and the advice to set a token is given only when none is configured.
- **"`Retry-After` is honored up to 60 s."** A pause longer than the cap is not shortened to the cap and retried early (GitHub says not to retry before it has elapsed); the request is not repeated and the caller gets the rate-limit error with `RetryAfter` set.
- **`Listing.Latest`** is GitHub's own latest (or empty) on the web path and the newest stable release under the tag prefix on the API path. A resolver must not depend on it being set.
