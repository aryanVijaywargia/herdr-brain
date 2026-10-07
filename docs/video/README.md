# README launch video

This folder holds the source of `assets/herdr-brain-launch.mp4` and `assets/herdr-brain-launch.webp`.
The README shows the WebP and links to the MP4.

- `index.html`: the [HyperFrames](https://www.npmjs.com/package/hyperframes) composition. 1080x1080, 30 fps, 18 s. One GSAP timeline and one camera, with no cuts.
- `make-webp.py`: makes the README WebP (720x720, 15 fps, loops) from the MP4.
- `package.json`, `hyperframes.json`: the HyperFrames project files. The CLI version is pinned to 0.8.140.

## Concept: a memory forming

The camera moves over one sheet of paper. A memory forms on it, and the video has no slides.

1. Brain reads what you tell your agents (0 to 2 s). Lines that you send in a claude pane and a codex pane of the workspace `api` lift out as fragments.
2. Known secrets are masked first (2 to 4 s). `password=hunter2` turns into `[secret]` while the fragment is still on its pane line.
3. You said it twice (4 to 6.5 s). The same correction, `no, use pnpm, not npm`, comes from both sessions. Thin lines join the fragments. The two copies find each other and fuse into one lesson, and the one-off fragments fade.
4. Brain asks you once (6.5 to 9 s). The lesson opens as the Brain review pane, and `a` approves it.
5. Every new session starts knowing (9 to 13 s). A line of light carries the item into the start brief of a new claude session and a new codex session. The claude agent runs `pnpm install` with no correction.
6. Close (13 to 18 s). The memory settles into the brain from the top of the repo README. Then the name, one line and the install command show.

## Keep it true

Show only what Brain does on `main`. Check each string on screen against the code or the HLD:

| On screen | Source |
|---|---|
| The correction needs 2 sessions, in any harness | HLD §7.2, "Correction: 2 occurrences, in any harnesses" |
| `[secret]` in place of a known secret pattern (`password=...` is the `password-pair` class) | `internal/secret/secret.go`, `internal/brained/filter.go` |
| The review pane lines and keys: a subset, in the same order | `internal/cli/testdata/pane/new-item.txt` |
| `proposal 7 approved: item 4 is active at revision 1 (workspace w3 (api))` | `internal/cli/review.go` (the approve message) |
| `## This workspace` and `- [4] Use pnpm, not npm.` in the start brief | `internal/brief/brief.go`, `internal/brief/testdata/` |

The ids (proposal 7, item 4, `w3`) are example values.
Use neutral example names (`api`), not real projects or people.

## Look

- Paper `#f3ecdd`, ink `#1e2230` and one accent, vermilion `#d9452b`.
- Fonts: Instrument Serif for the words, IBM Plex Mono for terminal text. Both have the SIL Open Font License and come from Google Fonts.
- No text is smaller than 22 px on the 1080 px frame, after the camera scale.

## Needs

- Node 22 or later (HyperFrames needs it), ffmpeg, and python3 with Pillow 10 or later.
- Chrome for the capture: HyperFrames uses the puppeteer Chrome in `~/.cache/puppeteer`.
- Network at render time: the fonts and GSAP come from links in `index.html`. HyperFrames caches the fonts in `~/.cache/hyperframes/fonts`. GSAP 3.14.2 comes from jsdelivr.

## Render again

Run these commands in this folder:

```sh
export HYPERFRAMES_NO_TELEMETRY=1 HYPERFRAMES_NO_UPDATE_CHECK=1 HYPERFRAMES_NO_FEEDBACK=1

# Lint, and stills to check text that is cut off, overlaps or is too small (in snapshots/).
npx --yes hyperframes@0.8.140 lint .
npx --yes hyperframes@0.8.140 snapshot . --at 1.5,2.5,4.95,8.7,12.4,17.6 --describe false --no-end

# The MP4: H.264, yuv420p. CRF 22 keeps it under 3 MB; the remux drops the render tags and
# puts the index at the start.
npx --yes hyperframes@0.8.140 render . --quality high --fps 30 --crf 22 -o out/raw.mp4
ffmpeg -y -i out/raw.mp4 -c copy -map_metadata -1 -movflags +faststart ../../assets/herdr-brain-launch.mp4

# The WebP for the README. The local ffmpeg has no WebP encoder, so Pillow writes it.
python3 make-webp.py ../../assets/herdr-brain-launch.mp4 ../../assets/herdr-brain-launch.webp
```

Keep the MP4 and the WebP each under 3 MB. The current render is 2.4 MB and 2.6 MB.
`node_modules/`, `out/` and `snapshots/` stay out of git (`.gitignore`).
