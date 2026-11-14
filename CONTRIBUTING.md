# Contributing

Tooling runs through [mise](https://mise.jdx.dev). `mise install` sets up
Go, golangci-lint and GoReleaser.

```sh
mise run test               # tests
mise run lint               # golangci-lint
mise run build              # dist/wam
mise run release:snapshot   # every release archive and checksums into dist/, nothing published
```

Commits and PR titles follow
[Conventional Commits](https://www.conventionalcommits.org). See
[AGENTS.md](AGENTS.md) for the branch and PR conventions.

## Releasing

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
`.github/rulesets/` and were applied with:

```sh
for f in .github/rulesets/*.json; do
  gh api repos/maddawik/wam/rulesets --method POST --input "$f"
done
```

## Temporary files

While a sync runs, wam creates `.wam-tmp-*` and `.wam-old-*` entries in the
`AddOns` folder, and `<file>.tmp-*` beside the manifest or lock while
writing it. If a run crashes, the next run puts things back and deletes
them.
