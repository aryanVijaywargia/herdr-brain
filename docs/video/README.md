# README launch video

This folder holds the source of `assets/herdr-brain-launch.mp4` and `assets/herdr-brain-launch.webp`.
The README shows the WebP at its top and links to the MP4.

- `index.html`: the [HyperFrames](https://www.npmjs.com/package/hyperframes) composition. 1080x1080, 30 fps, 22 s. One GSAP timeline and one camera, with no cuts.
- `make-webp.py`: makes the README WebP (720x720, 15 fps, loops) from the MP4.
- `package.json`, `hyperframes.json`: the HyperFrames project files. The CLI version is pinned to 0.8.140.

## Concept: a memory forming, inside herdr

One camera moves over herdr's ink ground and one herdr window. A memory forms above the window, and the video has no slides.

1. Brain reads what you tell your agents (0 to 2 s). The herdr window shows the sidebar (spaces: `api` / `main`; agents: claude and codex, working) and the tab `api` with a claude pane above a codex pane. Lines that you send lift out of the panes as fragments.
2. Known secrets are masked first (2 to 4 s). `password=hunter2` turns into `[secret]` while the fragment is still on its pane line. Then the agents go idle, and Brain reads their panes.
3. You said it twice (4 to 6.5 s). The same correction, `no, use pnpm, not npm`, comes from both sessions. Thin lines join the fragments. The two copies find each other and fuse into one lesson, and the one-off fragments fade. The window steps back while this happens.
4. Brain asks you once (6.5 to 9 s). Brain's review pane splits to the right of the claude pane, with no focus, and `a` approves the item. The agent glyphs do not change.
5. Every new session starts knowing (9 to 13 s). Brain closes its pane. A new tab `deps` opens with a new claude pane and a new codex pane, and the sidebar lists the two new agents. A line of light carries the item into their start brief, and the claude agent runs `pnpm install` with no correction.
6. Repeat a prompt, and Brain suggests a tool (13 to 16 s). In the `deps` claude pane you send `run lint and tests`. Brain's review pane splits beside it with a ready tool proposal and its contract (§5.5).
7. Close (16 to 22 s). The pieces settle into a small memory graph: square nodes and thin lines. The lesson `Use pnpm, not npm.` (kind `preference`) is the mauve node at the center. A skill, a tool and an automation sit around it, the two sessions that said it feed it, light runs to the two new sessions, and `brain recall pnpm` marks a recall during work. The caption names the kinds. Then the name, the tagline, the typed install line and the facts line show.

## Keep it true

Show only what Brain does on `main`. Check each string on screen against the code or the HLD:

| On screen | Source |
|---|---|
| The correction needs 2 sessions, in any harness | HLD §7.2, "Correction: 2 occurrences, in any harnesses" |
| Brain reads a pane when its agent changes state; it extracts at idle or session end | HLD §5.3, §6.2 |
| `[secret]` in place of a known secret pattern (`password=...` is the `password-pair` class) | `internal/secret/secret.go`, `internal/brained/filter.go` |
| The review pane title `Brain review`, a split that keeps the focus on the caller, on its right | `herdr-plugin.toml` (`[[panes]]` review), `internal/herdr/client.go` (`--no-focus`), `spike/m0/FINDINGS.md` (E5-S3: the split opens on the right) |
| The review pane lines and keys of the lesson: a subset, in the same order | `internal/cli/testdata/pane/new-item.txt` |
| `proposal 7 approved: item 4 is active at revision 1 (workspace w3 (api))` | `internal/cli/review.go` (the approve message) |
| `## This workspace` and `- [4] Use pnpm, not npm.` in the start brief (the video shows only the workspace part) | `internal/brief/brief.go`, `internal/brief/testdata/` |
| A prompt that repeats earlier prompts opens a ready proposal in a review pane beside the agent; it can be a lesson, a skill, a tool or an automation | HLD §5.5 |
| The tool proposal: `proposal 9 (pending): new item`, `kind:     tool`, `target:`, `benefit:  runs these commands with one reviewed contract`, `contract:`, `step 1:`, `step 2:` (a subset, in the same order) | `internal/cli/testdata/pane/tool-draft.txt`, `internal/cli/review.go` (`toolLine`, `contractLines`) |
| The item kinds `preference`, `skill`, `tool`, `automation`; a correction makes a `preference`, "the kind of a lesson" | `internal/store/tables.go`, `internal/extract/extract.go` (`itemKind`) |
| `automation · lint-test after each stop`: an automation runs one approved tool after each stop of an agent | HLD §10.4, `internal/store/automations.go` (`stopTrigger`), `internal/cli/review.go` (the approval line: "the tool runs after each stop") |
| `brain recall pnpm` | `internal/brief/brief.go` (the brief line "To find more Brain items on a topic during work, run: brain recall <words>"), E20-S2 |
| "Lessons, skills, tools and automations." | HLD §5.5 ("a lesson, a skill, a tool or an automation") |
| `herdr 0.9.1+` | `herdr-plugin.toml` (`min_herdr_version`) |

The ids (proposal 7 and 9, item 4, `w3`), the names `deploy` and `lint-test`, and the steps `pnpm lint` and `pnpm test` are example values. The agent lines (`● Bash(pnpm install)`, `• Running pnpm test.`) are example agent output, not Brain output.
Use neutral example names (`api`, `deps`), not real projects or people.

## Look: herdr's theme

- herdr.dev's ink theme: ground `#17171a`, panel `#1e1e22`, lines `#35353d`, text `#eae8ee` and `#908f96`, and the spot color mauve `#cba6f7` with `#17171a` text on it. Brain uses mauve; herdr's red is not used.
- Agent states as herdr shows them: working `#e6b84a` (●), idle `#52c97a` (○).
- The herdr window follows the herdr.dev UI mock: the sidebar with "spaces" and "agents", the tab bar with the active tab as a mauve block, panes with the border through the edge cell and the title in it (mauve for the focused pane). Square corners.
- The close is a memory graph of square nodes and thin lines, the same mark as the text block at the top of the repo README.
- Fonts, as herdr.dev: JetBrains Mono for terminal text, Inter for captions, Archivo 800 for the name. All have the SIL Open Font License and come from Google Fonts.
- No herdr logo or wordmark, no mascot. No text is smaller than 22 px on the 1080 px frame, after the camera scale.

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
npx --yes hyperframes@0.8.140 snapshot . --at 1.5,2.5,4.95,8.7,12.4,14.8,18.45,21.6 --describe false --no-end

# The MP4: H.264, yuv420p. CRF 22 keeps it near 3 MB. The fast capture (drawElementImage)
# dropped the review pane border and title, so the render uses screenshots. The remux drops
# the render tags and puts the index at the start.
npx --yes hyperframes@0.8.140 render . --quality high --fps 30 --crf 22 --no-experimental-fast-capture -o out/raw.mp4
ffmpeg -y -i out/raw.mp4 -c copy -map_metadata -1 -movflags +faststart ../../assets/herdr-brain-launch.mp4

# The WebP for the README. The local ffmpeg has no WebP encoder, so Pillow writes it.
python3 make-webp.py ../../assets/herdr-brain-launch.mp4 ../../assets/herdr-brain-launch.webp
```

Lint shows 1 warning: the file has more than 300 lines. One file is easier to keep true, so the warning stays.
Keep the MP4 and the WebP each under 3.5 MB. The current render is 2.9 MB and 3.3 MB.
`node_modules/`, `out/` and `snapshots/` stay out of git (`.gitignore`).
