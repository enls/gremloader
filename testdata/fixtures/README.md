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
