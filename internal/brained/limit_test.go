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
