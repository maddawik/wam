# wam

A declarative addon manager for World of Warcraft. List your addons in a
file, and `wam sync` installs them from GitHub Releases.

```toml
# wam.toml
[game]
flavor = "mainline"

[addons]
"github:DeadlyBossMods/DeadlyBossMods" = "latest"
"github:owner/pinned" = "v1.2.3"
"github:owner/picky" = { version = "latest", asset = "*-nolib.zip" }
```

Installed versions are pinned in `wam.lock`, and every download is checked
against its SHA-256, so the same manifest gives the same addons on every
machine.

## Install

Download the archive for your system from the
[latest release](https://github.com/maddawik/wam/releases/latest), extract
it, and put `wam` (`wam.exe` on Windows) somewhere on your `PATH`.

Or build it with Go:

```sh
go install github.com/maddawik/wam@latest
```

The binaries aren't signed. If macOS blocks a downloaded one, run
`xattr -d com.apple.quarantine <path to wam>`.

## Usage

```sh
wam init                                         # write a starter wam.toml
wam add github:DeadlyBossMods/DeadlyBossMods     # add an addon and install it
wam sync                                         # install everything in wam.toml
wam list                                         # show addons and their status
wam outdated                                     # show addons with a newer release
wam update                                       # move addons to their newest release
wam remove github:DeadlyBossMods/DeadlyBossMods  # remove an addon and its folders
```

`sync`, `add` and `remove` take `--preview` to show what would change.
Every command takes `--json`, and `--help` has the details.

wam finds the game by itself. If it can't, pass `--dir` or set `WAM_DIR` to
your `Interface/AddOns` folder. Set `GITHUB_TOKEN` if you hit GitHub's rate
limit.

## Files

wam keeps two files, `wam.toml` and `wam.lock`, in one folder:

| OS | Folder |
| --- | --- |
| macOS, Linux | `~/.config/wam` (or `$XDG_CONFIG_HOME/wam`) |
| Windows | `%APPDATA%\wam` |

Use `-f` or `WAM_FILE` to keep them somewhere else, such as a dotfiles
repo. Apart from those, wam only writes addon folders into `AddOns`.

## Uninstall

1. Run `wam remove <addon>` for each addon you don't want to keep.
2. Delete the folder above.
3. Delete the `wam` binary.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for how to build, test and release.
