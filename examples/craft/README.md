# The Craft apps with GremLoader

`gremloader.toml` in this folder installs and updates the twelve Craft apps (github.com/storytold): CadCraft, DeckCraft, DesignCraft, EffectCraft, FilmCraft, GridCraft, LightCraft, PdfCraft, PhotoCraft, SoundCraft, VectorCraft and WordCraft. It does what [CraftWerk](https://github.com/enls/craftwerk) does, with a plan before every change, a journal that survives a crash, and a log you can query.

## Where things go

| | Windows | Linux |
|---|---|---|
| Apps | `C:\CraftWerk\<App>\` (one folder each; `--root D` or `[vars] root` moves them) | `~/.local` (`bin/` and `share/`, shared by all twelve) |
| Asset | `<app>-<version>-windows-x64-portable.zip` (arm64 and x86 too) | `<app>-<version>-linux-x86_64.tar.gz` (aarch64 too) |
| Checked against | the release's `SHA256SUMS.txt` | the same |
| After a change | `C:\CraftWerk\_update\register-apps.ps1 -Only <app>`: file types, Default apps, Start menu | desktop menu, file-type and icon caches refreshed; a warning if the app is missing system libraries or `~/.local/bin` is not on `PATH` |

Your own files in an app's folder (settings, `PhotoCraftData`, anything the release does not ship) are never touched by an update, a rollback or a remove.

## Using it

Copy the file to GremLoader's config folder (`%APPDATA%\GremLoader\gremloader.toml` on Windows, `~/.config/gremloader/gremloader.toml` on Linux), or pass `--file path\to\gremloader.toml` each time. Then:

```sh
gremloader check            # what is new (exit 2 when there are updates); changes nothing
gremloader plan             # exactly what apply would do
gremloader apply            # install what is missing, update the rest (asks first; --yes to skip)
gremloader install pdfcraft # one app
gremloader rollback wordcraft
gremloader verify           # installed files against what was installed
gremloader log              # what happened, and when
```

To add only some apps, delete the other `[package.*]` tables, or keep them and `gremloader hold <app>` the ones you don't want updated.

## Moving over from CraftWerk

GremLoader does not know what CraftWerk installed, and it never installs over a folder it did not create. Tell it what is there, one app at a time:

```sh
gremloader adopt photocraft --verify
```

`--verify` downloads that version's release, checks that every file of it is in the folder with the same bytes, and records the file list, so a later update or remove knows exactly which files are the app's. Without `--verify` (Windows only) the version is read from the `.exe` and nothing is compared; such an app updates fine, but files left by older versions stay behind for good (GremLoader cannot tell them from yours), and removing it needs `--delete-dir`. On Linux (`~/.local` is shared) `--verify` is required.

Things that change:

- **Rollback** keeps one version back, like CraftWerk, but CraftWerk's `_update\previous\` is not imported: rolling back becomes possible after GremLoader's first update of an app.
- **The weekly schedule** (CraftWerk's task, with its 10-minute login wait and 2-hour retry) is not part of GremLoader yet. Keep CraftWerk's task until it is, or run `gremloader apply --yes` from your own.
- **`register-apps.ps1` stays** where CraftWerk put it (`C:\CraftWerk\_update\`); the package file calls it after every change. Remove those `post_*` lines if you don't want file types touched.
- **Linux:** CraftWerk rewrote the `Exec=` line of each `.desktop` file to a full path. GremLoader leaves the files as released (so `verify` can check them) and warns instead when `~/.local/bin` is not on `PATH`; most desktops add it at login once the folder exists.

Not ported: CraftWerk's report-only `.msi` list (it was empty) and the scheduled-run behaviour above.
