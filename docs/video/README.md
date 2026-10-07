# README launch video

This folder holds the source of `assets/herdr-brain-launch.mp4` and `assets/herdr-brain-launch.webp`.
The README shows the WebP and links to the MP4.

- `index.html`: the [HyperFrames](https://www.npmjs.com/package/hyperframes) composition. 1080x1080, 30 fps, 16 s. One GSAP timeline drives six scenes.
- `make-webp.py`: makes the README WebP (720x720, 15 fps, loops) from the MP4.
- `package.json`, `hyperframes.json`: the HyperFrames project files. The CLI version is pinned to 0.8.140.

## Story

1. Every session starts cold (0 to 2.9 s).
2. Tell it once: a plain-words line in a Claude session (2.9 to 6 s).
3. Review it beside you: the Brain review pane, and you approve (6 to 9.1 s).
4. Every agent gets it: new Claude Code and Codex sessions get the item at start (9.1 to 12.1 s).
5. It learns as you work: corrections, skills, tools, automations (12.1 to 13.7 s).
6. End card: "A plugin for herdr", "Herdr Brain.", the install command (13.5 to 16 s).

Show only what Brain does now. The pane text follows the real output: the review pane keys (`internal/cli/pane.go`) and the brief label `## This workspace` (`internal/brief`).
Use neutral example names (`api`, `notes-api`, `billing-page`, `docs`), not real projects or people.

## Needs

- Node 22 or later (HyperFrames needs it), ffmpeg, and python3 with Pillow 10 or later.
- Chrome for the capture: HyperFrames uses the puppeteer Chrome in `~/.cache/puppeteer`.
- Network at render time: the fonts and GSAP come from links in `index.html`.
  - Fonts: Archivo and JetBrains Mono from Google Fonts (SIL Open Font License). HyperFrames caches them in `~/.cache/hyperframes/fonts`.
  - GSAP 3.14.2 from jsdelivr.

## Render again

Run these commands in this folder:

```sh
export HYPERFRAMES_NO_TELEMETRY=1 HYPERFRAMES_NO_UPDATE_CHECK=1 HYPERFRAMES_NO_FEEDBACK=1

# Lint, and stills to check text that is cut off, overlaps or is too small (in snapshots/).
npx --yes hyperframes@0.8.140 lint .
npx --yes hyperframes@0.8.140 snapshot . --at 1.9,5.5,8.4,10.9,13.1,15.8 --describe false --no-end

# The MP4: H.264, yuv420p. The remux drops the render tags and keeps the index at the start.
npx --yes hyperframes@0.8.140 render . --quality high --fps 30 -o out/raw.mp4
ffmpeg -y -i out/raw.mp4 -c copy -map_metadata -1 -movflags +faststart ../../assets/herdr-brain-launch.mp4

# The WebP for the README. The local ffmpeg has no WebP encoder, so Pillow writes it.
python3 make-webp.py ../../assets/herdr-brain-launch.mp4 ../../assets/herdr-brain-launch.webp
```

Lint shows 7 warnings: it asks for one file per scene. One file is easier to keep true, so the warnings stay.
Keep the MP4 under 5 MB and the WebP under 3 MB. The first render was 2.7 MB and 2.1 MB.
`node_modules/`, `out/` and `snapshots/` stay out of git (`.gitignore`).
