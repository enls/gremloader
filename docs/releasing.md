# Releasing

Nothing is published automatically. A tag produces a **draft**; a person tests it and publishes it.

## Cutting a release

1. `main` is green (the **CI passed** check) and contains everything intended for the release.
2. Update `CHANGELOG.md`: move "Unreleased" under the new version, hand-edit the generated entries.
   Breaking changes to the package file, the command line or the on-disk state are called out.
3. Tag a checkpoint first if anything risky is in the release: `git tag checkpoint/<what>`.
4. Create and push a **signed** tag from `main`:

   ```sh
   git tag -s v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

   Use `v0.1.0-rc.1` style tags to test the pipeline: they become draft *pre-releases* and are never
   "latest".
5. The **Release** workflow validates the tag (format, and the commit is on `main`), runs the tests,
   builds all four targets, writes the SBOM and `SHA256SUMS.txt`, smoke-tests the linux/amd64 binary's
   reported version, attests everything, checks the attestations, and creates the draft.

## Before publishing the draft

Never publish before a manual run on real machines:

- [ ] Download the asset for the real Ubuntu server; verify `sha256sum -c SHA256SUMS.txt` and
      `gh attestation verify <file> --repo enls/gremloader`.
- [ ] Run it on the server: `gremloader version`, then the scenarios this release changed.
- [ ] Same on a real Windows PC.
- [ ] Release notes read sensibly (they are generated from commits; edit them).

Then publish the draft from the GitHub UI.

## Building locally

```sh
scripts/build-release.sh v0.0.0-test linux amd64 /tmp/out
```

Needs `go`, `tar`, `gzip` (and `zip` for Windows targets). The same commit built twice gives the
same bytes.

## If a release is bad

Delete the draft and the tag (`git push --delete origin <tag>`) before publishing. After publishing,
do not move or reuse a tag: mark the release as superseded in its notes and cut the next version.
