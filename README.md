# Herdr Brain

A herdr plugin that gives every coding agent in herdr the same global and workspace memory.
The name is temporary.

Status: planning and scaffold. Design: [`docs/design/HLD.md`](docs/design/HLD.md). M0 spike results: [`spike/m0/FINDINGS.md`](spike/m0/FINDINGS.md).

Needs herdr 0.9.1 or later, Linux or macOS.

## Install

```sh
herdr plugin install aryanVijaywargia/herdr-brain
herdr plugin action invoke brain.setup   # or run the "Brain: setup" action in herdr
```

herdr does not put the plugin on your `PATH`. `brain setup` shows the binary path and offers a `~/.local/bin/brain` link.
It never edits your shell config.

## Develop

```sh
go test ./...
sh scripts/build.sh          # builds bin/brain
herdr plugin link .          # link never runs the build step; build first
herdr plugin unlink brain
```

## Layout

| Path | What |
|---|---|
| `herdr-plugin.toml` | Plugin manifest |
| `cmd/brain/` | The one `brain` binary |
| `scripts/build.sh` | Build step that `herdr plugin install` runs |
| `internal/paths/` | Binary path and data home for every entry point |
| `docs/design/` | HLD, decisions and owner questions |
| `spike/m0/` | M0 spike code and findings |
