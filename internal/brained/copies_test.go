package brained

import (
	"context"
	"database/sql"
	"slices"
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

// #278, S40: a prompt sent again in a new turn is a second occurrence, also when the first
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

// #281: the approval dialog, the approval echo line and the diff lines of a Codex task (made
// from session S1 of the M2 eval run) and the folder trust dialog and the diff lines at the top
// of a Claude screen (session S2) are no user text: they make no user text observation and no
// signal. The question of the approval dialog is agent text, and it still makes the blocked
// approval signal (§7.1).
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

// #278 review: a prompt in a new turn that is only the start of an earlier prompt is a new
// send. An unknown status in a turn starts no turn, so a repaint after it is still a copy.
func TestCapturePromptTurns(t *testing.T) {
	const (
		long  = "› Run the tests and the build, and tell me what fails."
		short = "› Run the tests"
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
		"  >_ OpenAI Codex (v0.160.1)\n"+short+"\n\n• Ran npm test"+foot)
	k.must(t, Baseline, "w1:p1", "", GapNone)
	for _, st := range []string{"working", "idle", "working", "unknown", "working"} {
		k.must(t, Change, "w1:p1", st, GapNone)
	}
	excerpts, counted := k.rows(t).prompts()
	if !slices.Equal(excerpts, []string{long, short}) || counted != 2 {
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
