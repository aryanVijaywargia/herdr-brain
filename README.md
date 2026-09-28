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

### Without Go

`scripts/build.sh` builds from source when Go is on `PATH`.
With no Go, it downloads the release asset `brain_<os>_<arch>` and checks its SHA-256 against `checksums.txt`.
It does this only when the checkout is clean and `HEAD` is the commit of tag `v<manifest version>`.
Any other ref needs Go 1.23 or later.
The asset path works only when the repo is public.

## Uninstall

Run the two steps in this order:

```sh
brain uninstall            # dry run: lists what would change
brain uninstall --apply    # removes it; add --data to remove your stores too
herdr plugin uninstall brain
```

`brain uninstall --apply` removes the Brain hook entries from `~/.claude/settings.json` and `~/.codex/hooks.json`, the install record, the `~/.local/bin/brain` link and the session bindings file.
It keeps every other byte of the hook files. It keeps a hook entry that you edited and tells you to remove it by hand.
Without `--data`, your stores stay in `~/.local/share/brain`.
With `--data`, it also removes `~/.local/share/brain`, `~/.config/herdr/plugins/config/brain` and `~/.local/state/herdr/plugins/brain`.

If you ran `herdr plugin uninstall brain` first, the hook entries point to a missing binary. To fix this, do one of these:

- Install the plugin again (it gets the same path), run `brain uninstall --apply`, then uninstall the plugin.
- Or edit the files by hand. In `~/.claude/settings.json` and `~/.codex/hooks.json`, remove the `SessionStart` group whose command ends in `hook --profile claude --event start` or `hook --profile codex --event start`. Then remove `~/.local/share/brain/install.json`, `~/.local/share/brain/bindings.json` and the `~/.local/bin/brain` link.

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
| `herdr-plugin.toml` | Plugin manifest |
| `cmd/brain/` | The one `brain` binary |
| `scripts/build.sh` | Build step that `herdr plugin install` runs |
| `scripts/check-tag.sh` | Release gate: the tag must be `v<manifest version>` |
| `internal/paths/` | Binary path and data home for every entry point |
| `.github/workflows/` | `ci.yml` (tests), `release.yml` (assets on tag `v*`) |
| `docs/design/` | HLD, decisions and owner questions |
| `spike/m0/` | M0 spike code and findings |
