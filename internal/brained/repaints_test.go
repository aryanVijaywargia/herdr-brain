package brained

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The screens of the #349 tests: Codex 0.160.1, made from the lines of the M2 eval run 4
// report, with no real session data.
const (
	repaintFoot   = "\n\n  gpt-6.1-sol high · ~/brain-eval/notes-api-u\n"
	repaintShell  = "user@host notes-api-u % codex -m gpt-6.1-sol\n"
	repaintBanner = "  >_ OpenAI Codex (v0.160.1)\n     ~/brain-eval/notes-api-u\n\n"
	// stats is a feature request as you type it, rows as Codex draws it, and cut as Codex
	// draws it again on one line.
	stats = "Add a GET /stats endpoint that returns the number of notes and the title of the newest note, " +
		"with tests for an empty store and for a store with notes."
	statsRows = "› Add a GET /stats endpoint that returns the number of notes and the title of the newest note, with tests for an\n" +
		"  empty store and for a store with notes."
	statsCut = "› Add a GET /stats endpoint that returns the number of notes and the title of the newest note, with tests for an…"
)

// pane returns r with the observations of pane reads only.
func (r rows) pane() rows {
	r.obs = slices.DeleteFunc(slices.Clone(r.obs), func(o store.Observation) bool { return o.Source != store.SourceHerdr })
	return r
}

// counted returns the counted observations of r whose excerpt holds text.
func (r rows) counted(text string) []store.Observation {
	var obs []store.Observation
	for _, o := range r.obs {
		if o.Counts && strings.Contains(o.Excerpt, text) {
			obs = append(obs, o)
		}
	}
	return obs
}

// #349: Codex shows a sent prompt again as its first line and "…", with a line of the next
// block under it whose mark the read does not show (M2 eval run 4, observations 542 and 1421).
// The cut copy is no new occurrence, in the same turn and in a later one, and the line under it
// is no user text. A real resend of the prompt in a new turn after the repaints counts.
func TestCapturePromptCutBlock(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.raw(repaintShell+repaintFoot,
		repaintShell+statsRows+"\n\n• Working (1s • esc to interrupt)"+repaintFoot,
		// The same turn: the cut copy, and the start of the reply under it.
		repaintBanner+statsCut+"\n  I'll read src/server.js and the tests first."+repaintFoot,
		"• Added GET /stats."+repaintFoot,
		// A new turn: the cut copy again, and a line of tool output under it.
		repaintBanner+statsCut+"\n    > notes-api@0.2.0 test"+repaintFoot,
		"• The tests pass."+repaintFoot,
		// You send the same request again.
		"• The tests pass.\n"+statsRows+"\n\n• Working (1s • esc to interrupt)"+repaintFoot)
	k.must(t, Baseline, "w1:p1", "", GapNone)
	for _, st := range []string{"working", "working", "idle", "working", "idle", "working"} {
		k.must(t, Change, "w1:p1", st, GapNone)
	}
	r := k.rows(t)
	if excerpts, counted := r.prompts(); !slices.Equal(excerpts, []string{statsRows, statsRows}) || counted != 2 {
		t.Errorf("user text = %q (%d counted), want the request two times: the send and the resend", excerpts, counted)
	}
	for _, line := range []string{"I'll read src/server.js", "notes-api@0.2.0 test"} {
		for _, o := range r.obs {
			if o.Sender == store.SenderUnconfirmed && strings.Contains(o.Excerpt, line) {
				t.Errorf("user text observation %q holds %q", o.Excerpt, line)
			}
		}
	}
}

// #349: the pane read at the send fails, so the prompt hook record counts alone, and the
// baseline after it no longer shows the prompt (M2 eval run 4, request X7). A one-line cut copy
// 2 minutes later and a full repaint at idle 8 minutes later are no new occurrence: the hook
// record is the one occurrence of the send.
func TestCapturePromptReadFailed(t *testing.T) {
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.raw(repaintShell+repaintFoot, readFails,
		"• Explored\n  └ Read src/server.js"+repaintFoot,
		repaintBanner+statsCut+"\n\n• Ran npm test"+repaintFoot,
		repaintBanner+statsRows+"\n\n• Ran npm test\n\n• Added GET /stats."+repaintFoot)
	k.must(t, Baseline, "w1:p1", "", GapNone)
	hook := k.hookRecord(t, "prompt", store.SenderUnconfirmed, stats, k.clock)
	if err := k.step(t, Change, "w1:p1", "working", GapNone); err == nil {
		t.Fatal("change with a failed read: no error")
	}
	k.must(t, Change, "w1:p1", "working", GapNone) // the baseline after the failed read
	k.clock = k.clock.Add(2 * time.Minute)
	k.must(t, Change, "w1:p1", "working", GapNone)
	k.clock = k.clock.Add(6 * time.Minute)
	k.must(t, Change, "w1:p1", "idle", GapNone)

	r := k.rows(t)
	if got := r.counted("GET /stats endpoint"); len(got) != 1 || got[0].ID != hook {
		t.Errorf("counted observations of the request = %+v, want only the hook record %d", got, hook)
	}
	_, gaps := r.text()
	if !slices.Contains(gaps, gapFailed) {
		t.Errorf("gaps = %q, want the failed read", gaps)
	}
}

// #349: at a later idle read Codex shows the quoted question of an earlier answer again, with
// the answer under it, and the question view is no longer in the last reads (M2 eval run 4,
// observation 620). The quote is Codex text, and the answer is no new occurrence: the hook
// record of the answer holds it, and the first read of the answer is its occurrence.
func TestCaptureQuoteRepaint(t *testing.T) {
	const (
		question = "`data/db.json` is missing from this workspace. Where is the real database file?"
		answer   = "Create it from the seed with ./scripts/reset-db.sh, then measure."
		prompt   = "› Measure the size of the local db."
		asked    = repaintBanner + prompt + "\n\n• I need the database file.\n\n"
		view     = "  " + question + "\n\n  Type your answer\n\n  enter submit   ⌃] skip   ⇧→ main prompt\n"
		reply    = "› > " + question + "\n\n  " + answer + "\n\n• Ran ./scripts/reset-db.sh"
	)
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", codexRun2)
	k.raw(repaintShell+repaintFoot,
		repaintBanner+prompt+repaintFoot,
		asked+view+repaintFoot,
		asked+reply+repaintFoot,
		"• Ran ./scripts/reset-db.sh\n\n• The db holds 12 notes."+repaintFoot,
		// Codex draws the whole session again at idle.
		asked+reply+"\n\n• The db holds 12 notes."+repaintFoot)
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.must(t, Change, "w1:p1", "working", GapNone)
	k.must(t, Change, "w1:p1", "blocked", GapNone)
	k.hookRecord(t, "prompt", store.SenderUnconfirmed, answer, k.clock)
	k.must(t, Change, "w1:p1", "working", GapNone)
	k.must(t, Change, "w1:p1", "idle", GapNone)
	k.clock = k.clock.Add(10 * time.Minute)
	k.must(t, Change, "w1:p1", "idle", GapNone)

	r := k.rows(t)
	if excerpts, counted := r.pane().prompts(); !slices.Equal(excerpts, []string{prompt, "  " + answer}) || counted != 2 {
		t.Errorf("user text = %q (%d counted), want the prompt and the answer once each", excerpts, counted)
	}
	if got := r.counted("reset-db.sh, then measure"); len(got) != 1 || got[0].Source != store.SourceHerdr || got[0].EnrichedBy == 0 {
		t.Errorf("counted observations of the answer = %+v, want the first pane read, enriched by its hook record", got)
	}
}

// #349: with hook records, a cut copy and a full repaint of a sent prompt are no new
// occurrence, and the same prompt sent again in a new turn is: its own hook record enriches its
// pane read, also when the hook comes after the read. A resend with no hook record of its own
// still counts (#349 review): a hook can fail, and an old hook record does not hold the new
// send. Each send counts once.
func TestCapturePromptResendHooks(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook int // the resend has a hook record: 1 before its read, 2 after it; 0 none
	}{{"hook before the read", 1}, {"hook after the read", 2}, {"no hook of its own", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			k := newCapture(t, "s1", t.TempDir())
			k.pane("w1:p1", "codex", codexRun2)
			k.raw(repaintShell+repaintFoot,
				repaintShell+statsRows+"\n\n• Working (1s • esc to interrupt)"+repaintFoot,
				repaintBanner+statsCut+"\n  I'll read src/server.js and the tests first."+repaintFoot,
				repaintBanner+statsRows+"\n\n• Added GET /stats."+repaintFoot,
				"• Added GET /stats."+repaintFoot,
				"• Added GET /stats.\n"+statsRows+"\n\n• Working (1s • esc to interrupt)"+repaintFoot)
			k.must(t, Baseline, "w1:p1", "", GapNone)
			first := k.hookRecord(t, "prompt", store.SenderUnconfirmed, stats, k.clock)
			for _, st := range []string{"working", "working", "idle", "idle"} {
				k.must(t, Change, "w1:p1", st, GapNone)
			}
			k.clock = k.clock.Add(6 * time.Minute)
			var again int64
			if tc.hook == 1 {
				again = k.hookRecord(t, "prompt", store.SenderUnconfirmed, stats, k.clock)
			}
			k.must(t, Change, "w1:p1", "working", GapNone)
			if tc.hook == 2 {
				again = k.hookRecord(t, "prompt", store.SenderUnconfirmed, stats, k.clock)
			}

			got := k.rows(t).counted("GET /stats endpoint")
			if len(got) != 2 || got[0].Source != store.SourceHerdr || got[0].EnrichedBy != first ||
				got[1].Source != store.SourceHerdr || got[1].EnrichedBy != again {
				t.Errorf("counted observations of the request = %+v, want the two pane reads, enriched by hooks %d and %d",
					got, first, again)
			}
		})
	}
}

// #349 review: a literal "…" at the end of the first line of a new prompt of more lines, after
// a known prompt with that first line, is a new send, not a cut copy. Claude takes only a cut
// copy of one line. Codex takes the new prompt as the first read of its own hook record.
func TestCapturePromptEllipsis(t *testing.T) {
	for _, tc := range []struct {
		harness, mark, reply string
		hooks                bool
	}{
		{"claude", "❯ ", "⏺ ", false},
		{"codex", "› ", "• ", true},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			first := tc.mark + "Run the tests\n  and the build."
			next := tc.mark + "Run the tests…\n  Only the API package this time."
			k := newCapture(t, "s1", t.TempDir())
			k.pane("w1:p1", tc.harness, codexRun2)
			k.raw(tc.mark+"\n", first+"\n", first+"\n\n"+tc.reply+"Done.\n", tc.reply+"Done.\n",
				tc.reply+"Done.\n"+next+"\n")
			k.must(t, Baseline, "w1:p1", "", GapNone)
			if tc.hooks {
				k.hookRecord(t, "prompt", store.SenderUnconfirmed, "Run the tests\nand the build.", k.clock)
			}
			for _, st := range []string{"working", "idle", "idle"} {
				k.must(t, Change, "w1:p1", st, GapNone)
			}
			if tc.hooks {
				k.hookRecord(t, "prompt", store.SenderUnconfirmed, "Run the tests…\nOnly the API package this time.", k.clock)
			}
			k.must(t, Change, "w1:p1", "working", GapNone)
			if excerpts, counted := k.rows(t).pane().prompts(); !slices.Equal(excerpts, []string{first, next}) || counted != 2 {
				t.Errorf("user text = %q (%d counted), want both prompts, each counted", excerpts, counted)
			}
		})
	}
}
