# wam

A declarative World of Warcraft addon manager. List your addons in
`wam.toml`, run `wam sync`, and wam installs them from their GitHub
Releases. It pins what it installed in `wam.lock` and checks every download
against the lock's SHA-256.

## Install

Download the archive for your OS and CPU from the
[latest release](https://github.com/maddawik/wam/releases/latest). Archives
are named `wam_<version>_<os>_<arch>`, where `os` is `darwin`, `linux` or
`windows` and `arch` is `amd64` or `arm64`.

Check the archive against `checksums.txt` from the same release:

```sh
shasum -a 256 wam_*.tar.gz   # macOS, Linux; compare with checksums.txt
```

```powershell
Get-FileHash wam_*.zip       # Windows; compare with checksums.txt
```

**macOS and Linux:** extract `wam` and move it onto your `PATH`:

```sh
tar -xzf wam_*.tar.gz wam
mkdir -p ~/.local/bin && mv wam ~/.local/bin/
```

The binary isn't signed. If macOS blocks it after a browser download, clear
the quarantine flag: `xattr -d com.apple.quarantine ~/.local/bin/wam`.

**Windows:** extract `wam.exe` into a folder such as
`%LOCALAPPDATA%\Programs\wam` and add that folder to your user `PATH`.

**From source:** `go install github.com/maddawik/wam@latest`. A build made
this way reports `dev` from `wam version`.

Check it works:

```sh
wam version
```

## Quick start

```sh
wam init                     # write a starter wam.toml for the installed game
wam add github:owner/repo    # add an addon and install it
wam sync                     # install everything in the manifest
wam outdated                 # list addons with a newer release
wam update                   # move addons to their newest release
```

Every command takes `--help`, and `--json` for machine-readable output.
Set `GITHUB_TOKEN` to raise GitHub's API rate limit or reach private repos.

## What wam writes where

| What | Where |
| --- | --- |
| `wam.toml` (manifest) and `wam.lock` (what was installed) | The config directory: `$XDG_CONFIG_HOME/wam` when set, else `~/.config/wam` on macOS and Linux and `%APPDATA%\wam` on Windows. `-f` or `$WAM_FILE` picks another manifest; its lock sits beside it. |
| Addon folders | The game's `Interface/AddOns` folder (`--dir`, `$WAM_DIR`, or auto-detected). The lock records which folders each addon owns. |
| `.wam-tmp-*` and `.wam-old-*` | Inside `AddOns`, only while a sync runs. If a run crashes, the next run puts things back and deletes them. |
| `<file>.tmp-*` | Beside the manifest or lock, only while it's being written. |

wam writes nothing else: no cache, no registry keys, no telemetry. On
Windows it reads the Battle.net registry keys to find the game.

## Uninstall

1. Optionally remove the addons wam installed. `wam list` shows them, and
   `wam remove <addon>` deletes each one's folders. If you skip this step,
   the folders stay and the game keeps loading them.
2. Delete the config directory listed above, or your own manifest and the
   `.lock` beside it.
3. If a sync ever crashed, delete any leftover `.wam-tmp-*` or `.wam-old-*`
   in `AddOns`.
4. Delete the `wam` binary, and the completion script if you installed one.

## Development

Tooling runs through [mise](https://mise.jdx.dev):

```sh
mise run test               # tests
mise run lint               # golangci-lint
mise run build              # dist/wam
mise run release:snapshot   # every release archive and checksums into dist/, nothing published
```

### Releasing

```sh
mise run release v0.1.0
```

This checks that the tag is `v`-prefixed semver and doesn't exist yet, that
the working tree is clean, that `main` is checked out and matches
`origin/main`, and that CI passed for `HEAD`. Then it pushes an annotated
tag. The release workflow runs GoReleaser on the tag and publishes the
archives and checksums to GitHub Releases.

Two tag rulesets on `v*` keep published tags immutable: only admins can
create them, and nobody can update or delete them. They live in
`.github/rulesets/` and are applied with:

```sh
for f in .github/rulesets/*.json; do
  gh api repos/maddawik/wam/rulesets --method POST --input "$f"
done
```
