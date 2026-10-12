# 0010. What the extractor accepts (`internal/archive`)

Status: accepted (2026-10-10)

## Context

A release archive is attacker-influenced input that is about to be written onto the user's disk under a path GremLoader chooses. The classic failures are all in the names and the entry types: a zip-slip `../../x`, an absolute path, a symlink that a later entry writes through, a device node, a file that declares 1 KB and holds 4 GB.

Two facts about this project make "just call the standard library" insufficient:

- **The same bytes mean different trees on different systems.** A backslash is a path separator on Windows and an ordinary letter on Linux. `NUL.txt` is a device on Windows. `a.txt:evil` is an NTFS data stream. `README` and `readme` are one file on Windows and macOS. CraftWerk's own zip once shipped with backslashes because Windows PowerShell's `Compress-Archive` wrote them.
- **The package file is written once and runs on both OSes.** A package that extracts on the Linux server must extract the same way on the Windows desktop, or "works on my machine" becomes a supply-chain property.

Observed shapes of the real archives (soundcraft v0.4.0, cadcraft v0.4.0): the Windows zip has seven files under one top-level folder, `/` separators, no directory entries, no unix modes; the Linux tarballs have a `bin/` + `share/` prefix under one top-level folder, explicit directory entries, no links.

## Decision

`internal/archive` is the only code that unpacks anything. Formats: zip, tar, tar.gz, detected from the file's first bytes (never its name). xz, zstd, bzip2 and 7z are refused by name.

**Everything is checked before the first byte is written** (pass 1: names, types, counts, declared sizes, the strip decision, conflicts), then the tree is written (pass 2) while the bytes actually produced are counted. An archive is accepted whole or not at all: any failure leaves the destination empty.

**Name rules, applied on every OS** (a rejection names the entry and exactly one rule):

| Rule | Rejects |
|---|---|
| `empty-name`, `invalid-utf8`, `control-char` | empty names, bytes that are not UTF-8, any control character (NUL, escapes, newline) |
| `backslash`, `colon` | a separator or an NTFS stream/drive letter depending on the OS |
| `absolute`, `dot-dot`, `empty-component` | leaving the destination |
| `windows-char` | `< > " | ? *` |
| `reserved-name` | `CON PRN AUX NUL COM0-9 LPT0-9 CONIN$ CONOUT$` (and superscript digits), with or without an extension, any case |
| `trailing-dot-or-space` | a name Windows silently renames |
| `component-too-long`, `path-too-long`, `too-deep` | 255 bytes a component, 200 bytes a path, 32 levels |
| `duplicate`, `case-collision`, `file-dir-conflict` | a path twice, two paths that differ only by case, a name that is both a file and a directory |

A leading `./` is accepted (tar writes it); a bare `./` is the archive root and is skipped.

**Type rules.** Symbolic links, hard links, device nodes, FIFOs, sockets, sparse files and encrypted entries are *rejected*, not skipped: an archive that contains one is not the archive the package author tested. A pax global header is metadata and is ignored. Any other tar type flag is `unsupported-type`.

**Limits** (counted, not trusted): 4 GiB total, 2 GiB per file, 20,000 entries. Declared sizes are used only to refuse early; the real count is enforced as bytes are written. The decompressed stream itself is capped (declared total + headers for the allowed entry count + 1 MiB), so metadata that carries no file cannot expand without bound. A header that lies about size is `size-mismatch` or `corrupt`, naming the entry.

**Empty.** An archive with no files (nothing at all, or only directories) is `empty` (amendment, 2026-10-10, M5): the engine would otherwise swap an empty tree over a working install. A tar with no entries is only its zero end-of-archive blocks and is recognized as a tar for this purpose; a zero-byte file is "not an archive" and says it is empty.

**Modes.** Files become 0644, or 0755 if any execute bit was set; directories 0755. setuid, setgid and sticky bits never reach the disk. Owners and times are not applied.

**Strip.** `auto` removes one component when every entry lives under the same single top-level folder (and there is no loose file beside it); an integer removes that many. A file left with no name is an error; two entries that collide after stripping are rejected like any other collision.

**Output** goes only through an `os.Root` opened on the destination, with `O_EXCL`, so a rule that was missed still cannot write outside it or over an earlier file. The destination must be empty.

**Result.** Files with size, mode and SHA-256 (hashed as written, so the manifest costs nothing extra), every directory including implied ones, the stripped prefix, total bytes.

## Reasons

- One policy for both OSes costs a few legitimate-but-odd archives (a name with a colon) and buys that a package cannot behave differently across machines. Neither of the two Craft archive shapes that were inspected (soundcraft, cadcraft) is affected.
- Reject, not skip: skipping a symlink silently produces an install nobody tested.
- Two passes cost a second decompression of a tar.gz (seconds for 150 MB) and buy that nothing is written for an archive that was going to be refused, and that `strip=auto` can be decided before writing.
- `os.Root` is the backstop, not the policy: it exists so that a bug in the rules above degrades to "refused" instead of "wrote outside".

## Consequences

- A release that starts publishing a symlink (some Linux tarballs do, for `lib.so.1`) fails to install until the package supports it. That is intended; it will be a new, explicit decision.
- Case-insensitive collision rejection applies on Linux too, where it is stricter than the file system. Unicode normalization (NFC vs NFD names) is **not** handled.
- Windows long paths (> 260 characters) are not a concern at the 200-byte path cap, but the *destination prefix* is not counted; the engine must check it.

## Rejected alternatives

- **Rely on `filepath.IsLocal` / `os.Root` alone.** They do not know about NTFS streams, device names with extensions, trailing dots, or case collisions, and they behave per OS.
- **Skip unsupported entries with a warning.** See Reasons.
- **Extract, then validate the tree.** A hostile archive has already acted by then.
- **One pass, streaming, for tar.** Cannot decide `strip=auto` or refuse before writing.
- **A third-party archive library.** No dependency is needed for these three formats.

## What was and was not verified

Verified by tests (all OSes in CI): every rule above with a hostile archive in zip and tar form, the lying-header and bomb cases with the real byte counter, a failing writer (`ENOSPC`) leaving nothing behind, two fuzz targets (zip, tar.gz) with the invariant that an accepted archive contains only valid paths inside the destination.

**Not verified:** `os.Root` behavior on Windows beyond the cases pinned in `TestOSRootRefusesToLeaveItsDirectory`; extraction on a case-sensitive NTFS directory; archives with tens of thousands of entries (the cap is tested at 10); Unicode normalization; behavior under antivirus scanning of the destination.
