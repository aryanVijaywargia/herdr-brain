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
| `cmd/brain-eval/` | The eval runner: it runs the cases in `evals/` against a test herdr session. `build.sh` does not build it. A herdr version joins the tested list only after it passes these evals. |
| `evals/` | The eval cases (probes) and prompts that `brain-eval` runs |
| `internal/paths/` | Binary path and data home for every entry point |
| `internal/` | The other packages, for example `store` (SQLite), `output` (the compiler), `hook`, `cli` and `herdr` |
| `adapters/` | The adapter profiles of Claude Code, Codex and agy |
| `scripts/build.sh` | Build step that `herdr plugin install` runs |
| `scripts/check-tag.sh` | Release gate: the tag must be `v<manifest version>` |
| `scripts/third-party-licenses.sh` | Release asset `THIRD_PARTY_LICENSES.txt`: the license files of the Go modules in the binary |
| `.github/workflows/` | `ci.yml` (tests), `release.yml` (assets on tag `v*`) |
