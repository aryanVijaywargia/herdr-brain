package brained

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// prompts returns the excerpts of the stored observations of sender unconfirmed, and how many
// of them count.
func (r rows) prompts() (excerpts []string, counted int) {
	for _, o := range r.obs {
		if o.Sender == store.SenderUnconfirmed && o.Status != store.ObservationGap {
			excerpts = append(excerpts, o.Excerpt)
			if o.Counts {
				counted++
			}
		}
	}
	return excerpts, counted
}

// signals returns the signals of the store and the excerpt of each observation.
func (k *capHarness) signals(t *testing.T) ([]signal.Signal, map[int64]string) {
	t.Helper()
	var ins []store.SignalInput
	k.tx(t, func(tx *sql.Tx) (err error) {
		ins, err = store.SignalInputs(context.Background(), tx)
		return err
	})
	text := map[int64]string{}
	for _, in := range ins {
		text[in.ID] = in.Excerpt
	}
	return signal.Find(ins), text
}

// #278: one Codex task of the M2 eval run (codex 0.160.1), made from the stored excerpts of
// session S1: the prompt, a read in the approval dialog that shows the prompt cut to one line
// with diff lines under it, a read after the approval, the read at idle where Codex draws the
// banner and the whole prompt again, and the next turn under the cut prompt. Before the fix
// the prompt was in 4 observations, 3 of them counted; now it is one observation, and the next
// prompt is one more.
func TestCaptureCodexPromptCopies(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.reads(t, "w1:p1", "codex-i278-0-base.txt", "codex-i278-1-working.txt", "codex-i278-2-blocked.txt",
		"codex-i278-3-working.txt", "codex-i278-4-idle.txt", "codex-i278-5-working.txt")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	for _, st := range []string{"working", "blocked", "working", "idle", "working"} {
		k.must(t, Change, "w1:p1", st, GapNone)
	}
	excerpts, counted := k.rows(t).prompts()
	if len(excerpts) != 2 || counted != 2 || !strings.HasPrefix(excerpts[0], "› Add pagination") ||
		!strings.HasSuffix(excerpts[0], "so skip npm install.") || !strings.HasPrefix(excerpts[1], "› Run the full check") {
		t.Errorf("user text = %q (%d counted), want the two prompts once each", excerpts, counted)
	}
	sigs, text := k.signals(t)
	for _, s := range sigs {
		if s.Kind == signal.Correction && len(s.Observations) > 1 {
			t.Errorf("correction signal %+v repeats a copy: %q", s, text[s.Observations[0]])
		}
		for _, id := range s.Observations {
			if s.Kind != signal.BlockedApproval && !strings.HasPrefix(text[id], "› ") {
				t.Errorf("signal %s points at %q, want a prompt", s.Kind, text[id])
			}
		}
	}
}

// #278: a prompt sent again in a new turn is a second occurrence, also when the first
// copy is gone from the read; a read of the same turn that shows it again is not.
func TestCapturePromptNewTurn(t *testing.T) {
	const (
		prompt = "› Run the full check now: lint, then tests, then build, in one chained command, and tell me what fails."
		foot   = "\n\n  GPT-6.1-Sol high · ~/work/notes-api\n"
	)
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.raw("user@host notes-api % codex"+foot,
		"user@host notes-api % codex\n"+prompt+"\n\n• Working (1s • esc to interrupt)"+foot,
		"user@host notes-api % codex\n"+prompt+"\n\n• Ran npm run lint && npm test && npm run build\n\n• All passed."+foot,
		// The first prompt scrolled out of the read, and the same prompt is sent again.
		"• All passed.\n"+prompt+"\n\n• Working (1s • esc to interrupt)"+foot,
		"• All passed.\n"+prompt+"\n\n• Ran npm run lint && npm test && npm run build\n\n• All passed again."+foot,
		// A read of the same turn shows the prompt again under a banner.
		"  >_ OpenAI Codex (v0.160.1)\n"+prompt+"\n\n• All passed again."+foot)
	k.must(t, Baseline, "w1:p1", "", GapNone)
	for _, st := range []string{"working", "idle", "working", "idle", "idle"} {
		k.must(t, Change, "w1:p1", st, GapNone)
	}
	excerpts, counted := k.rows(t).prompts()
	if len(excerpts) != 2 || counted != 2 || excerpts[0] != prompt || excerpts[1] != prompt {
		t.Errorf("user text = %q (%d counted), want the prompt two times", excerpts, counted)
	}
}

// #278: Claude (2.1.292) shows a prompt again after the screen scrolled it away, with another
// wrap, as after a resize. Made from the excerpts of session S2 of the M2 eval run. Before the
// fix the prompt was in 2 observations (1 counted); now it is in 1.
func TestCaptureClaudePromptCopies(t *testing.T) {
	const (
		p1 = "❯ When a POST /notes body is not valid JSON, or the title is missing or empty, return a 400 with a JSON error, and log\n" +
			"  the bad request with the method, the path and the reason. Add tests for both cases and run them.\n"
		p2 = "❯ When a POST /notes body is not valid JSON, or the title is missing or empty, return a 400 with a JSON error, and log the bad\n" +
			"  request with the method, the path and the reason. Add tests for both cases and run them.\n"
		update = "⏺ Update(src/server.js)\n  ⎿  Added 12 lines, removed 2 lines\n"
	)
	var long strings.Builder
	for i := range 30 {
		long.WriteString("⏺ Step " + strings.Repeat("x", i%7+1) + "\n")
	}
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "claude", "")
	k.raw("❯ \n", p1+"\n"+update, update+long.String(), p2+"\n"+update+long.String()+"⏺ Done. Both cases return a 400.\n")
	k.must(t, Baseline, "w1:p1", "", GapNone)
	for _, st := range []string{"working", "working", "idle"} {
		k.must(t, Change, "w1:p1", st, GapNone)
	}
	excerpts, counted := k.rows(t).prompts()
	if len(excerpts) != 1 || counted != 1 || !strings.HasPrefix(excerpts[0], "❯ When a POST") {
		t.Errorf("user text = %q (%d counted), want the prompt once", excerpts, counted)
	}
}

// #368 (M2 eval run 5): the end of a Claude spinner tip under a sent prompt was stored as
// part of the prompt (observation 2326). The eval report gives the line with no indent, so the
// screen is made twice: the end of the tip at column 6, which the tip block does not take, and
// at column 0. Each time the prompt is stored without it, once, and it counts.
func TestCaptureTipUnderPrompt(t *testing.T) {
	const (
		prompt = "❯ Commit the docs fix and the test split as two commits on main."
		tip    = "  ⎿  Tip: You can control how big a workflow is just by prompting. Ask for a small workflow, cap it with \"use at most 5\n"
		end    = `agents", or set a default with Dynamic workflow size in /config.`
		box    = "\n─────────────\n❯ \n─────────────\n  ⏵⏵ auto mode on (shift+tab to cycle)\n"
	)
	for _, indent := range []string{"      ", ""} {
		t.Run(strconv.Itoa(len(indent)), func(t *testing.T) {
			k := newCapture(t, "s1", t.TempDir())
			k.pane("w1:p1", "claude", "")
			k.raw("⏺ Earlier.\n"+box, "⏺ Earlier.\n\n"+prompt+"\n\n✻ Committing… (4s · thinking)\n"+tip+indent+end+"\n"+box,
				"⏺ Earlier.\n\n"+prompt+"\n\n⏺ Done: two commits on main.\n"+box)
			k.must(t, Baseline, "w1:p1", "", GapNone)
			k.must(t, Change, "w1:p1", "working", GapNone)
			k.must(t, Change, "w1:p1", "idle", GapNone)
			if excerpts, counted := k.rows(t).prompts(); !slices.Equal(excerpts, []string{prompt}) || counted != 1 {
				t.Errorf("user text = %q (%d counted), want only %q, counted", excerpts, counted, prompt)
			}
			for _, o := range k.rows(t).obs {
				if strings.Contains(o.Excerpt, "Dynamic workflow size") {
					t.Errorf("observation %q holds the tip", o.Excerpt)
				}
			}
		})
	}
}

// #281: the approval dialog, the approval echo line and the diff lines of a Codex task (made
// from session S1 of the M2 eval run) and the folder trust dialog and the diff lines at the top
// of a Claude screen (session S2) are no user text: they make no user text observation and no
// signal. The question of the approval dialog is agent text, and it still makes the blocked
// approval signal.
func TestCaptureDialogLines(t *testing.T) {
	for _, tc := range []struct {
		harness string
		texts   []string
		noise   []string
		want    []signal.Kind
	}{
		{"codex", []string{fixture(t, "codex-i278-1-working.txt"), fixture(t, "codex-i278-2-blocked.txt"),
			fixture(t, "codex-i278-3-working.txt")},
			[]string{"Yes, proceed", "don't ask again", "No, and tell Codex", "Press enter", "Would you like", "Reason:",
				"You approved codex", "const raw", "GET /notes?limit", "Show details", "ctrl+t"},
			[]signal.Kind{signal.BlockedApproval}},
		{"codex", []string{"user@host notes-api % codex\n", "user@host notes-api % codex\n" + fixture(t, "codex-i281-trust.txt")},
			[]string{"Folder access", "Trust this folder", "Trust and continue", "Quit", "esc quit"}, nil},
		{"claude", []string{"❯ \n", fixture(t, "claude-i281-trust.txt")},
			[]string{"Accessing workspace", "Quick safety check", "project, or work", "No, exit", "I trust this folder",
				"Enter to confirm"}, nil},
		{"claude", []string{"❯ \n", fixture(t, "claude-i281-diff.txt")},
			[]string{"JSON.parse", "} catch", "badRequest", "...", "Ran 2 shell commands"}, nil},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			k := newCapture(t, "s1", t.TempDir())
			k.pane("w1:p1", tc.harness, "")
			k.raw(tc.texts...)
			k.must(t, Baseline, "w1:p1", "", GapNone)
			for range tc.texts[1:] {
				k.must(t, Change, "w1:p1", "blocked", GapNone)
			}
			excerpts, _ := k.rows(t).prompts()
			for _, e := range excerpts {
				for _, n := range tc.noise {
					if strings.Contains(e, n) {
						t.Errorf("user text %q holds %q", e, n)
					}
				}
			}
			sigs, text := k.signals(t)
			var kinds []signal.Kind
			for _, s := range sigs {
				kinds = append(kinds, s.Kind)
				if s.Kind == signal.BlockedApproval {
					continue // the approval screen itself
				}
				for _, n := range tc.noise {
					if strings.Contains(text[s.Observations[0]], n) {
						t.Errorf("%s signal starts at %q", s.Kind, text[s.Observations[0]])
					}
				}
			}
			if !slices.Equal(kinds, tc.want) {
				t.Errorf("signals %v, want %v", kinds, tc.want)
			}
		})
	}
}

// #316: the CLI screen text of the M2 eval rerun (claude 2.1.293, codex 0.160.1) makes no
// observation and no signal: the claude placeholder with its footer, the MCP warning, the fold
// lines and the tool output under them, the shell line that started the CLI, the codex hooks
// dialog and /hooks table, and the lines of a stopped turn. The reads are made from the lines
// of the eval report. Real prompts with the same words are user text, and each counts.
func TestCaptureCLINoise(t *testing.T) {
	const rule = "────────────────────────────────────────"
	hooks := "  Hooks need review\n  3 hooks are new or changed.\n  Hooks can run outside the sandbox after you trust them.\n\n"
	for _, tc := range []struct {
		harness string
		texts   []string
		prompts []string
		noise   []string
	}{
		{"claude", []string{"user@host notes-api-r % claude\n",
			" ▐▛███▜▌   Claude Code v2.1.293\n▝▜█████▛▘  Opus 5.5 (1M context) · Claude Pro\n\n" +
				"⚠ 1 MCP server needs authentication · run /mcp\n\n" +
				"❯ Try \"write a test for <filepath>\"\n  Opus 5.5 (1M context)\n",
			"❯ Read 1 file and fix it\n\n  Read 1 file, ran 2 shell commands\n     M test/helpers.js\n\n" +
				"⏺ Fixed test/helpers.js.\n\n❯ the conversation went wrong, use the tests\n\n" +
				"  Listed 1 directory, ran 1 shell command\n     lint: ok\n\n  Searched for 1 pattern\n\n" +
				"⏺ The lint passes.\n\n" + rule + "\n❯\n" + rule + "\n  Opus 5.5 (1M context)\n"},
			[]string{"❯ Read 1 file and fix it", "❯ the conversation went wrong, use the tests"},
			[]string{"Try \"", "(1M context)", "% claude", "MCP server", "Read 1 file, ran", "Listed 1 directory",
				"Searched for", "M test/helpers.js", "lint: ok"}},
		{"codex", []string{hooks + "› 1. Review hooks\n  2. Trust all and continue\n  3. Continue without trusting (hooks won't run)\n",
			hooks + "  1. Review hooks\n› 2. Trust all and continue\n  3. Continue without trusting (hooks won't run)\n\n" +
				"  Press enter to confirm or esc to go back\n",
			"user@host notes-api-r % codex -m gpt-6.1-sol -c model_reasoning_effort=\"high\"\n" +
				"  >_ OpenAI Codex (v0.160.1)\n     ~/brain-eval/notes-api-r\n\n› Read 1 file and fix it\n\n" +
				"• Fixed test/helpers.js.\n\n› the conversation went wrong, use the tests\n\n" +
				"  Hooks\n  Lifecycle hooks from config and enabled plugins.\n\n" +
				"  Event                 Installed   Active      Review      Description\n" +
				"  SessionStart          2           2           0           When a new session starts\n\n" +
				"  Press t to trust all; enter to review hooks; esc to close\n\n" +
				"✗ You canceled the request to run npm test\n\n" +
				"■ Conversation interrupted - use /feedback if something went wrong\n\n" +
				"  GPT-6.1-Sol high · ~/brain-eval/notes-api-r\n"},
			[]string{"› Read 1 file and fix it", "› the conversation went wrong, use the tests"},
			[]string{"Hooks", "hooks", "Trust all", "% codex", "Installed", "SessionStart", "You canceled",
				"Conversation interrupted", "/feedback"}},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			k := newCapture(t, "s1", t.TempDir())
			k.pane("w1:p1", tc.harness, "")
			k.raw(tc.texts...)
			k.must(t, Baseline, "w1:p1", "", GapNone)
			for range tc.texts[1:] {
				k.must(t, Change, "w1:p1", "idle", GapNone)
			}
			r := k.rows(t)
			if excerpts, counted := r.prompts(); !slices.Equal(excerpts, tc.prompts) || counted != len(tc.prompts) {
				t.Errorf("user text = %q (%d counted), want %q, each counted", excerpts, counted, tc.prompts)
			}
			for _, o := range r.obs {
				for _, n := range tc.noise {
					if strings.Contains(o.Excerpt, n) {
						t.Errorf("observation %q holds %q", o.Excerpt, n)
					}
				}
			}
			sigs, text := k.signals(t)
			for _, sg := range sigs {
				for _, n := range tc.noise {
					if strings.Contains(text[sg.Observations[0]], n) {
						t.Errorf("%s signal starts at %q", sg.Kind, text[sg.Observations[0]])
					}
				}
			}
		})
	}
}

// #338, #341: the CLI screen text of the M2 eval run 3 (claude 2.1.294, codex 0.160.1), made
// from the lines of its report, through Capture. No user text observation and no prompt index
// row holds it, and no signal starts at it. The real prompts next to it are user text and
// count, also a correction and a quoted prompt. Codex: the exit text of an earlier session,
// read again at the start of the next one; tool output and a reply line at column 0, also at
// the top of a read with its └ above the read; the answer to a question of Codex, as the Codex
// source draws it: the question view, then the quoted question and the answer. The question is
// Codex text, and the answer is yours. Claude: the start placeholder under a no-break space and
// the fold line with a memory part.
func TestCaptureRun3Noise(t *testing.T) {
	const (
		foot   = "\n\n  gpt-6.1-sol high · ~/brain-eval/notes-api-t\n"
		banner = "user@host notes-api-t % codex -m gpt-6.1-sol\n\n  >_ OpenAI Codex (v0.160.1)\n     ~/brain-eval/notes-api-t\n\n"
		exit   = "• The pagination works.\n\nToken usage: total=49,572 input=45,120 (+ 210,304 cached) output=4,452 (reasoning 1,920)\n" +
			"To continue this session, run: codex resume 01a11d03-5b1e-7c40-9d2a-4f0e8b6c1a22\n"
		question = "`data/db.json` is missing from this workspace. Where is the real database file?"
		answer   = "  Create it from the seed with ./scripts/reset-db.sh, then measure."
		work     = "› Reset the local db and run the tests.\n\n" +
			"• Ran npm test\n  └ > notes-api@0.1.0 test\n    > node --test\n# todo 0\n# duration_ms 134.68\n\n" +
			"• Ran git status --short\n  └  M src/routes/notes.js\n?? src/routes/\n\n" +
			"• Ran ls data/db.json\n  └ (no output)\nls: data/db.json: No such file or directory\n\n" +
			"• Ran npm start\n  └ > notes-api@0.1.0 start\nlistening on :3000\n^C\n\n" +
			"• Ran lsof -i :3000\n  └ (no output)\nCOMMAND   PID USER   FD   TYPE DEVICE SIZE/OFF NODE NAME\n\n" +
			"• The tests pass. They use a\ntemporary store seeded from the checked-in data so they run reliably.\n\n" +
			"• I need the database file to measure.\n\n"
		view = "  " + question + "\n\n  Type your answer\n\n  enter submit   ⌃] skip   ⇧→ main prompt\n"
		done = "› > " + question + "\n\n" + answer + "\n\n• Ran ./scripts/reset-db.sh\n\n" +
			"› > No, use the seed in data/seed.json.\n  And keep the old db.\n"
	)
	nb := "\u00a0"
	rule := strings.Repeat("─", 60)
	logo := " ▐▛███▜▌   Claude Code v2.1.294\n▝▜█████▛▘  Opus 5.5 (1M context) · Claude Pro\n\n"
	box := "  ◐ medium · /effort\n" + rule + "\n"
	for _, tc := range []struct {
		harness   string
		texts     []string
		prompts   []string
		noise     []string
		assistant string // a text that an observation of the assistant holds
	}{
		{"codex", []string{banner + foot, exit + banner + foot, exit + banner + work + view + foot, exit + banner + work + done + foot,
			// The └ of the npm test output is above the read.
			"# todo 0\n# duration_ms 98.40\n\n• The tests pass again.\n\n› Run it again.\n" + foot},
			[]string{"› Reset the local db and run the tests.", answer,
				"› > No, use the seed in data/seed.json.\n  And keep the old db.", "› Run it again."},
			[]string{"Token usage", "codex resume", "# todo", "duration_ms", "?? src/routes/", "No such file", "listening on",
				"^C", "COMMAND", "temporary store", "is missing", "Where is the real database"},
			"› > " + question},
		{"claude", []string{logo + "❯ Try \"edit <filepath> to...\"\n",
			logo + box + "❯" + nb + "Try \"refactor store-errors.test.js\"\n" + rule + "\n  Opus 5.5 | effort:medium | ctx:0%/1M\n",
			logo + "❯ Remember that the tests need the seed db, then run them.\n\n  Ran 2 shell commands, wrote 1 memory\n" +
				"     > notes-api@0.1.0 test\n\n⏺ Saved it to memory, and the tests pass.\n\n" + box + "❯\n" + rule + "\n"},
			[]string{"❯ Remember that the tests need the seed db, then run them."},
			[]string{"Try \"", "wrote 1 memory", "notes-api@0.1.0 test"},
			"⏺ Saved it to memory"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			k := newCapture(t, "s1", t.TempDir())
			k.pane("w1:p1", tc.harness, "")
			k.raw(tc.texts...)
			k.must(t, Baseline, "w1:p1", "", GapNone)
			for range tc.texts[1:] {
				k.must(t, Change, "w1:p1", "idle", GapNone)
			}
			r := k.rows(t)
			if excerpts, counted := r.prompts(); !slices.Equal(excerpts, tc.prompts) || counted != len(tc.prompts) {
				t.Errorf("user text = %q (%d counted), want %q, each counted", excerpts, counted, tc.prompts)
			}
			if !slices.ContainsFunc(r.obs, func(o store.Observation) bool {
				return o.Sender == store.SenderAssistant && strings.Contains(o.Excerpt, tc.assistant)
			}) {
				t.Errorf("no assistant observation holds %q", tc.assistant)
			}
			var es []store.PromptEntry
			k.tx(t, func(tx *sql.Tx) (err error) {
				es, err = store.PromptEntries(context.Background(), tx)
				return err
			})
			if len(es) != len(tc.prompts) {
				t.Errorf("prompt index = %d rows, want %d", len(es), len(tc.prompts))
			}
			sigs, text := k.signals(t)
			for _, n := range tc.noise {
				for _, e := range es {
					if strings.Contains(e.Text, n) {
						t.Errorf("prompt index row %q holds %q", e.Text, n)
					}
				}
				for _, sg := range sigs {
					if sg.Kind != signal.Failure && strings.Contains(text[sg.Observations[0]], n) {
						t.Errorf("%s signal starts at %q", sg.Kind, text[sg.Observations[0]])
					}
				}
			}
		})
	}
}

// #338: Codex shows a long prompt again as its first line and "…" (testdata/codex-i278-2-
// blocked.txt). In a new turn the cut copy is the last prompt of the read, but it is still a
// copy: one occurrence. A short prompt that ends with "…" is a new send, also when it is the
// start of a known prompt, and a prompt sent again with the same text is a new occurrence.
func TestCapturePromptCut(t *testing.T) {
	const (
		long = "› Let's release version 0.4.0. Steps, in this order: bump the version in package.json, add the CHANGELOG\n" +
			"  section, then commit, then tag v0.4.0."
		cut  = "› Let's release version 0.4.0. Steps, in this order: bump the version in package.json, add the CHANGELOG…"
		wait = "› Let's release…"
		foot = "\n\n  gpt-6.1-sol high · ~/work/notes-api\n"
	)
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.raw("user@host notes-api % codex"+foot,
		"user@host notes-api % codex\n"+long+"\n\n• Working (1s • esc to interrupt)"+foot,
		"user@host notes-api % codex\n"+long+"\n\n• Ran npm version 0.4.0"+foot,
		// Codex goes on in a new turn and draws the prompt again, cut.
		"  >_ OpenAI Codex (v0.160.1)\n"+cut+"\n\n• Ran git tag v0.4.0"+foot,
		"  >_ OpenAI Codex (v0.160.1)\n"+cut+"\n\n• Ran git tag v0.4.0\n\n• Released 0.4.0."+foot,
		"• Released 0.4.0.\n"+wait+"\n\n• Working (1s • esc to interrupt)"+foot,
		"• Released 0.4.0.\n"+wait+"\n\n• OK."+foot,
		"• OK.\n"+wait+"\n\n• Working (1s • esc to interrupt)"+foot)
	k.must(t, Baseline, "w1:p1", "", GapNone)
	for _, st := range []string{"working", "idle", "working", "idle", "working", "idle", "working"} {
		k.must(t, Change, "w1:p1", st, GapNone)
	}
	excerpts, counted := k.rows(t).prompts()
	if !slices.Equal(excerpts, []string{long, wait, wait}) || counted != 3 {
		t.Errorf("user text = %q (%d counted), want the release prompt once and %q two times", excerpts, counted, wait)
	}
}

// #278 review: a prompt in a new turn that is only the start of an earlier prompt is a new
// send, also when it ends with "…" (#338 review). An unknown status in a turn starts no turn, so
// a repaint after it is still a copy.
func TestCapturePromptTurns(t *testing.T) {
	const (
		long  = "› Run the tests and the build, and tell me what fails."
		short = "› Run the tests"
		cut   = "› Run the tests…"
		foot  = "\n\n  GPT-6.1-Sol high · ~/work/notes-api\n"
	)
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.raw("user@host notes-api % codex"+foot,
		"user@host notes-api % codex\n"+long+"\n\n• Working (1s • esc to interrupt)"+foot,
		"user@host notes-api % codex\n"+long+"\n\n• All passed."+foot,
		"• All passed.\n"+short+"\n\n• Working (1s • esc to interrupt)"+foot,
		"• All passed.\n"+short+"\n\n• Working (3s • esc to interrupt)"+foot,
		// Codex draws the screen again in the same turn, after an unknown status.
		"  >_ OpenAI Codex (v0.160.1)\n"+short+"\n\n• Ran npm test"+foot,
		"  >_ OpenAI Codex (v0.160.1)\n"+short+"\n\n• Ran npm test\n\n• All passed."+foot,
		"• All passed.\n"+cut+"\n\n• Working (1s • esc to interrupt)"+foot)
	k.must(t, Baseline, "w1:p1", "", GapNone)
	for _, st := range []string{"working", "idle", "working", "unknown", "working", "idle", "working"} {
		k.must(t, Change, "w1:p1", st, GapNone)
	}
	excerpts, counted := k.rows(t).prompts()
	if !slices.Equal(excerpts, []string{long, short, cut}) || counted != 3 {
		t.Errorf("user text = %q (%d counted), want each prompt once", excerpts, counted)
	}
}

// #281 review: only a Codex approval question with its detail line under it starts an
// assistant block. The same words in a prompt stay user text.
func TestSendersApproval(t *testing.T) {
	lines := []string{"› Use this question in our UI:", "  Would you like to delete the project?",
		"  Never ask it before a preview.", "• Running rm -r build", "› Run it", "  Would you like to run the following command?",
		"  Reason: remove the build folder?"}
	want := []store.Sender{store.SenderUnconfirmed, store.SenderUnconfirmed, store.SenderUnconfirmed, store.SenderAssistant,
		store.SenderUnconfirmed, store.SenderAssistant, store.SenderAssistant}
	if got := senders("codex", lines); !slices.Equal(got, want) {
		t.Errorf("senders = %v, want %v", got, want)
	}
}

// #353: the codex tool output lines and the reply line of M2 eval run 4 (codex 0.160.1), made
// from the lines of its report, through Capture. Codex draws a tall block from its bottom, so
// the end of tool output or of a reply shows under the prompt line, and the • and └ lines of
// the block are above the screen. These lines are no user text: no user text observation and
// no prompt index row holds them, and no signal starts at them. Each prompt counts once.
func TestCaptureRun4ToolLines(t *testing.T) {
	const (
		foot   = "\n\n  gpt-6.1-sol high · ~/brain-eval/notes-api-u\n"
		banner = "user@host notes-api-u % codex -m gpt-6.1-sol\n\n  >_ OpenAI Codex (v0.160.1)\n     ~/brain-eval/notes-api-u\n\n"
		done   = "\n\n• Done.\n"
	)
	turns := []struct {
		prompt string
		under  string // the end of the block under the prompt
	}{
		{"› Build the package and tell me the version.", "    build: dist/ for notes-api 0.2.0"},
		{"› Run the tests and show the timing.", "    # duration_ms 412.71"},
		{"› Run them again.", "    # duration_ms 98.40"},
		{"› Start the server and show me its log.", `    {"level":"info","msg":"listening","port":3000,"time":"2026-10-09T10:12:03Z"}`},
		{"› Show me package.json.", "    \"main\": \"src/server.js\",\n    \"private\": true,"},
		{"› What is in the changelog?", "    - First version: health check, notes CRUD and pagination."},
		{"› Add the error tests for POST /notes.",
			"  Every new endpoint must have error-case tests and a README entry listing its error codes."},
	}
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	texts := []string{banner + foot}
	var prompts, statuses []string
	top := banner
	for _, tc := range turns {
		texts = append(texts, top+tc.prompt+foot, top+tc.prompt+"\n"+tc.under+foot, top+tc.prompt+"\n"+tc.under+done+foot)
		statuses = append(statuses, "working", "working", "idle")
		prompts = append(prompts, tc.prompt)
		top = "• Done.\n\n"
	}
	k.raw(texts...)
	k.must(t, Baseline, "w1:p1", "", GapNone)
	for _, st := range statuses {
		k.must(t, Change, "w1:p1", st, GapNone)
	}
	r := k.rows(t)
	if excerpts, counted := r.prompts(); !slices.Equal(excerpts, prompts) || counted != len(prompts) {
		t.Errorf("user text = %q (%d counted), want %q, each counted", excerpts, counted, prompts)
	}
	var es []store.PromptEntry
	k.tx(t, func(tx *sql.Tx) (err error) {
		es, err = store.PromptEntries(context.Background(), tx)
		return err
	})
	if len(es) != len(prompts) {
		t.Errorf("prompt index = %d rows, want %d", len(es), len(prompts))
	}
	sigs, text := k.signals(t)
	for _, tc := range turns {
		for _, line := range strings.Split(tc.under, "\n") {
			line = strings.TrimSpace(line)
			for _, o := range r.obs {
				if o.Sender == store.SenderUnconfirmed && strings.Contains(o.Excerpt, line) {
					t.Errorf("user text observation %q holds %q", o.Excerpt, line)
				}
			}
			for _, e := range es {
				if strings.Contains(e.Text, line) {
					t.Errorf("prompt index row %q holds %q", e.Text, line)
				}
			}
			for _, sg := range sigs {
				if strings.Contains(text[sg.Observations[0]], line) {
					t.Errorf("%s signal starts at %q", sg.Kind, text[sg.Observations[0]])
				}
			}
		}
	}
}

// #353 review: a new Codex prompt whose first line is the same as an old prompt line, with
// other lines under it, is no known prompt: the line diff takes its first line as old, and its
// other lines are still user text, and they count.
func TestCapturePromptSameFirstLine(t *testing.T) {
	const (
		foot   = "\n\n  gpt-6.1-sol high · ~/brain-eval/notes-api-u\n"
		first  = "› Fix the API\n  Add validation."
		second = "› Fix the API\n  Add authentication."
	)
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.raw("• Earlier."+foot, "• Earlier.\n"+first+foot, "• Earlier.\n"+first+"\n\n• Done."+foot, second+foot)
	k.must(t, Baseline, "w1:p1", "", GapNone)
	for _, st := range []string{"working", "idle", "working"} {
		k.must(t, Change, "w1:p1", st, GapNone)
	}
	excerpts, counted := k.rows(t).prompts()
	if len(excerpts) != 2 || counted != 2 || excerpts[0] != first || !strings.Contains(excerpts[1], "Add authentication.") {
		t.Errorf("user text = %q (%d counted), want the first prompt and the new lines of the second, each counted", excerpts, counted)
	}
}
