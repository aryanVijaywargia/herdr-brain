# Contributing to Herdr Brain

## Develop

```sh
go test ./...
sh scripts/build.sh          # builds bin/brain
herdr plugin link .          # link never runs the build step; build first
herdr plugin unlink brain
```

## Release

1. Set `version` in `herdr-plugin.toml` and merge.
2. Push tag `v<version>` on that commit. `release.yml` fails when the tag does not match the manifest.

## Layout

| Path | What |
|---|---|
| `herdr-plugin.toml` | Plugin manifest: build step, actions, panes and events |
| `cmd/brain/` | The one `brain` binary |
| `internal/paths/` | Binary path and data home for every entry point |
| `internal/` | The other packages, for example `store` (SQLite), `output` (the compiler), `hook`, `cli` and `herdr` |
| `adapters/` | The adapter profiles of Claude Code, Codex and agy |
| `scripts/build.sh` | Build step that `herdr plugin install` runs |
| `scripts/check-tag.sh` | Release gate: the tag must be `v<manifest version>` |
| `.github/workflows/` | `ci.yml` (tests), `release.yml` (assets on tag `v*`) |
| `docs/design/` | HLD, decisions and owner questions |
| `spike/m0/` | M0 spike code and findings |
