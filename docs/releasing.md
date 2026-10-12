# Releasing

Nothing is published automatically. A tag produces a **draft**; a person tests it and publishes it.

## One-time setup: signing tags

Release tags are signed with an SSH key, so GitHub can show them as Verified. Use a key that is only for signing, not the one used to log in to servers.

```sh
ssh-keygen -t ed25519 -C "gremloader signing" -f ~/.ssh/id_ed25519_signing
```

1. On GitHub: Settings, SSH and GPG keys, New SSH key, **Key type: Signing Key**, paste `~/.ssh/id_ed25519_signing.pub`.
2. In this repository only (not globally):

   ```sh
   git config gpg.format ssh
   git config user.signingkey ~/.ssh/id_ed25519_signing.pub
   git config tag.gpgsign true
   # lets `git tag -v` verify locally:
   echo "<commit email> $(cat ~/.ssh/id_ed25519_signing.pub)" > ~/.ssh/allowed_signers
   git config gpg.ssh.allowedSignersFile ~/.ssh/allowed_signers
   ```
3. Try it: `git tag -s signing-test -m test && git tag -v signing-test && git tag -d signing-test`.

## Cutting a release

1. `main` is green (the **CI passed** check) and contains everything intended for the release.
2. Update `CHANGELOG.md`: move "Unreleased" under the new version, hand-edit the generated entries. Breaking changes to the package file, the command line or the on-disk state are called out.
3. Tag a checkpoint first if anything risky is in the release: `git tag checkpoint/<what>`.
4. Create and push a **signed** tag from `main`:

   ```sh
   git tag -s v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

   Use `v0.1.0-rc.1` style tags to test the pipeline: they become draft *pre-releases* and are never "latest".
5. The **Release** workflow validates the tag (format, and the commit is on `main`), runs the tests, builds all four targets, writes the SBOM and `SHA256SUMS.txt`, smoke-tests the linux/amd64 binary's reported version, attests everything, checks the attestations, and creates the draft.

## Before publishing the draft

Never publish before a manual run on real machines:

- [ ] Download the asset for the real Ubuntu server; verify `sha256sum -c SHA256SUMS.txt` and `gh attestation verify <file> --repo enls/gremloader`.
- [ ] Run it on the server: `gremloader version`, then the scenarios this release changed.
- [ ] Same on a real Windows PC.
- [ ] `gremloader self-update --check --prerelease` from a copy of the previous release (on a Windows PC): it finds this release and reports it as attested.
- [ ] Release notes read sensibly (they are generated from commits; edit them).

Then publish the draft from the GitHub UI. A release candidate (`-rc.N` tag) is published with **Release label: Pre-release** selected (the workflow sets it on the draft; a pre-release is never "latest"): `gremloader self-update` on a release candidate (or with `--prerelease`) finds it there, and a stable install never sees it. A draft is invisible to self-update. See ADR 0015.

## Building locally

```sh
scripts/build-release.sh v0.0.0-test linux amd64 /tmp/out
```

Needs `go`, `tar`, `gzip` (and `zip` for Windows targets). The same commit built twice gives the same bytes, on any host: the linux archives rebuilt on a Windows PC matched the hashes CI published for v0.0.1-rc.1. Windows `.zip` archives have not been compared.

## If a release is bad

Delete the draft and the tag (`git push --delete origin <tag>`) before publishing. After publishing, do not move or reuse a tag: mark the release as superseded in its notes and cut the next version.
