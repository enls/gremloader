# Decided against

Ideas that were considered and turned down, with the reasoning, so they are not re-litigated by
accident. Nothing here is deleted; if a decision is reversed, say so in the entry and link the ADR.

- **The WebUI library (webui-dev/webui).** The Go wrapper uses cgo (needs a C compiler, breaks
  one-command cross-compiling), requires an installed browser at runtime (headless servers have
  none), is still beta, and its Go v2 API is described as incomplete. Plain `net/http` + `go:embed`
  gives the same result in pure Go. See ADR 0003.
- **A terminal menu as the primary interface.** Chosen first, then replaced by the web UI. Possible
  later on the same core.
- **The UI writing changes back into `gremloader.toml`.** TOML libraries drop comments and
  reformat; UI edits go to `gremloader.local.toml`. See ADR 0004.
- **One file for desired state, lock and installed state.** Separate jobs, separate files.
- **Assuming semver everywhere.** apt, Docker and date-versioned projects don't follow it. See
  ADR 0005.
- **Loading htmx or fonts from a CDN.** Breaks offline use and the strict CSP; everything is
  embedded.
- **Shelling out to `gh` for attestation verification.** Chosen instead: native sigstore-go. Avoids
  requiring `gh` on every machine, at the cost of a larger dependency. See ADR 0006.
- **A `grem` short alias.** Taken by another repo-manager tool.
- **Treating the gangsheet app's npm dependencies as host dependencies.** They live in the image.
- **A y/n prompt on a hash mismatch ("this may be malicious, continue?").** Proposed 2026-10-10 and
  turned down in favour of a hard failure with `--accept-sha256 <hash>`. The scheduled run has no
  terminal, a bare "y" is answered by reflex, "may be malicious" overstates what is usually a corrupt
  download or a publisher who did not regenerate the checksum file, and the override should record
  *which bytes* were accepted. A prompt that defaults to N, only on a TTY, was considered a fallback
  but the hash form is the only way through. See the Phase 1 plan, E.3.4.
