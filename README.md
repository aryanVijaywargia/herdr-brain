# Herdr Brain

A herdr plugin that gives every coding agent in herdr the same global and workspace memory.
The name is temporary.

Status: planning and scaffold. Design: [`docs/design/HLD.md`](docs/design/HLD.md). M0 spike results: [`spike/m0/FINDINGS.md`](spike/m0/FINDINGS.md).

Needs herdr 0.9.1 or later, Linux or macOS.

## Develop

```sh
go test ./...
sh scripts/build.sh          # builds bin/brain
herdr plugin link .          # link never runs the build step; build first
herdr plugin unlink brain
```

## Install without Go

`scripts/build.sh` builds from source when Go is on `PATH`.
With no Go, it downloads the release asset `brain_<os>_<arch>` and checks its SHA-256 against `checksums.txt`.
It does this only when the checkout is clean and `HEAD` is the commit of tag `v<manifest version>`.
Any other ref needs Go 1.23 or later.
The asset path works only when the repo is public.

## Release

1. Set `version` in `herdr-plugin.toml` and merge.
2. Push tag `v<version>` on that commit. `release.yml` fails when the tag does not match the manifest.

## Layout

| Path | What |
|---|---|
| `herdr-plugin.toml` | Plugin manifest |
| `cmd/brain/` | The one `brain` binary |
| `scripts/build.sh` | Build step that `herdr plugin install` runs |
| `scripts/check-tag.sh` | Release gate: the tag must be `v<manifest version>` |
| `.github/workflows/` | `ci.yml` (tests), `release.yml` (assets on tag `v*`) |
| `docs/design/` | HLD and owner questions |
| `spike/m0/` | M0 spike code and findings |
