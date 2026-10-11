package brained

import (
	"slices"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// #379 (M2 eval run 6, R6-5, claude 2.1.296): the claude usage limit line under a prompt and
// the limit dialog are screen text: capture stores neither as user text nor as tool output, and
// the prompt stays one counted user text. The dialog's "Stop" makes no correction signal.
func TestCaptureLimitScreen(t *testing.T) {
	const (
		prompt = "❯ When a POST /notes body is not valid JSON, return 400 with an error field. Add tests and run them."
		limit  = "  ⎿  You've hit your weekly limit · resets Oct 12 at 9:30am (UTC)\n"
		dialog = "\n▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔\n   What do you want to do?\n" +
			"   ❯ 1. Stop and wait for limit to reset\n     2. Wait here, then continue automatically at Oct 12 at 9:30am\n" +
			"     3. Upgrade your plan\n   Enter to confirm · Esc to cancel\n"
		box = "\n─────────────\n❯ \n─────────────\n  ⏵⏵ auto mode on (shift+tab to cycle)\n"
	)
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "claude", "")
	k.raw("⏺ Earlier.\n"+box, "⏺ Earlier.\n\n"+prompt+"\n\n✻ Thinking… (1s)\n"+box,
		"⏺ Earlier.\n\n"+prompt+"\n"+limit+dialog)
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.must(t, Change, "w1:p1", "working", GapNone)
	k.must(t, Change, "w1:p1", "blocked", GapNone)
	if excerpts, counted := k.rows(t).prompts(); !slices.Equal(excerpts, []string{prompt}) || counted != 1 {
		t.Errorf("user text = %q (%d counted), want only %q, counted", excerpts, counted, prompt)
	}
	for _, o := range k.rows(t).obs {
		if o.Sender == store.SenderTool || strings.Contains(o.Excerpt, "limit") && !strings.HasPrefix(o.Excerpt, "❯") {
			t.Errorf("observation %q (%s) holds the limit screen", o.Excerpt, o.Sender)
		}
	}
}

// #385 (M2 eval run 6, codex 0.160.1): the codex limit warning, the 429 and 503 error lines and
// the footer limit lines are screen text: capture stores none of them, and the prompts stay
// counted user text. A reply and tool output with "429" and "rate limit" in them stay.
func TestCaptureCodexLimitScreen(t *testing.T) {
	const (
		prompt = "› Retry a request that gets 429, and warn when the rate limit is near."
		again  = "› Run the tests again."
		work   = "• Ran npm test\n  └ not ok 4 - retry after 429 Too Many Requests\n\n" +
			"• The client now retries a 429 with a backoff and warns at the rate limit.\n"
		warn  = "⚠ Heads up, you have less than 5% of your 5h limit left. Run /status for a breakdown.\n"
		retry = "■ exceeded retry limit, last status: 429 Too Many Requests\n"
		down  = "■ unexpected status 503 Service Unavailable: All managed Codex accounts are temporarily\n" +
			"unavailable for this runtime request., url: http://127.0.0.1:41873/responses\n"
		foot = "\n› Ask Codex to do anything\n\n  ⚠ 5h limit: 6% left · /status\n" +
			"  main · gpt-6.1-sol high · Context 61% left · 258K window\n"
	)
	k := newCapture(t, "s1", t.TempDir())
	k.pane("w1:p1", "codex", "")
	k.raw(foot, prompt+"\n\n"+work+"\n"+warn+foot,
		prompt+"\n\n"+work+"\n"+warn+"\n"+again+"\n\n"+retry+"\n"+again+"\n\n"+down+foot)
	k.must(t, Baseline, "w1:p1", "", GapNone)
	k.must(t, Change, "w1:p1", "idle", GapNone)
	k.must(t, Change, "w1:p1", "idle", GapNone)
	r := k.rows(t)
	if excerpts, counted := r.prompts(); !slices.Equal(excerpts, []string{prompt, again, again}) || counted != 3 {
		t.Errorf("user text = %q (%d counted), want %q, each counted", excerpts, counted, []string{prompt, again, again})
	}
	var kept []string
	for _, o := range r.obs {
		for _, noise := range []string{"Heads up", "exceeded retry", "unexpected status", "unavailable for", "5h limit"} {
			if strings.Contains(o.Excerpt, noise) {
				t.Errorf("observation %q (%s) holds the limit screen", o.Excerpt, o.Sender)
			}
		}
		kept = append(kept, o.Excerpt)
	}
	all := strings.Join(kept, "\n")
	for _, want := range []string{"retry after 429 Too Many Requests", "retries a 429 with a backoff and warns at the rate limit"} {
		if !strings.Contains(all, want) {
			t.Errorf("no observation holds %q; got\n%s", want, all)
		}
	}
}
