package brained

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// hookRecord stores a hook record of pane w1:p1 in binding s1 as `brain hook` does: one
// observation in the open segment, then the S35 match. It returns the observation id.
func (k *capHarness) hookRecord(t *testing.T, event string, sender store.Sender, text string, at time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	k.tx(t, func(tx *sql.Tx) error {
		session, segment, err := store.HookSegment(ctx, tx, "s1", "claude", "w1:p1", "")
		if err != nil || segment == 0 {
			return fmt.Errorf("no open segment: %w", err)
		}
		o := store.Observation{CaptureIdentity: fmt.Sprintf("s1/%d/hook/%s/%s", segment, event, at.Format(time.RFC3339Nano)),
			Source: store.SourceHook, SegmentID: segment, Event: event, Status: store.ObservationComplete,
			Sender: sender, Counts: true, At: at}
		o.Excerpt, o.Truncated, o.BrainOrigin = HookText("claude", text)
		if o.ID, err = store.AddObservation(ctx, tx, o); err != nil {
			return err
		}
		id = o.ID
		return Match(ctx, tx, "claude", session, o)
	})
	return id
}

// paneRow returns the pane observation whose excerpt holds text.
func paneRow(t *testing.T, obs []store.Observation, text string) store.Observation {
	t.Helper()
	for _, o := range obs {
		if o.Source == store.SourceHerdr && strings.Contains(o.Excerpt, text) {
			return o
		}
	}
	t.Fatalf("no pane observation with %q in %+v", text, obs)
	return store.Observation{}
}

func hookRow(t *testing.T, obs []store.Observation, id int64) store.Observation {
	t.Helper()
	for _, o := range obs {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("no observation %d", id)
	return store.Observation{}
}

// S35: a hook record that matches a pane observation (the session, the time window and the
// normalized text) enriches it and is not a second occurrence. The order of the two does not
// matter. A hook record with other text, or outside the window, stays a separate occurrence.
func TestCaptureS35(t *testing.T) {
	t.Run("hook after pane", func(t *testing.T) {
		k := newCapture(t, "s1", t.TempDir())
		k.pane("w1:p1", "claude", "")
		k.reads(t, "w1:p1", "claude-s40-0-base.txt", "claude-s40-1-idle.txt")
		k.must(t, Baseline, "w1:p1", "", GapNone)
		k.must(t, Change, "w1:p1", "idle", GapNone)
		stop := k.hookRecord(t, "stop", store.SenderAssistant, "ready", k.clock.Add(-2*time.Second))
		other := k.hookRecord(t, "stop", store.SenderAssistant, "a reply that no pane shows", k.clock)
		late := k.hookRecord(t, "prompt", store.SenderUnconfirmed, "Reply with only the word ready.",
			k.clock.Add(matchWindow+time.Minute))
		obs := k.rows(t).obs
		reply := paneRow(t, obs, "⏺ ready")
		if reply.EnrichedBy != stop || hookRow(t, obs, stop).Counts {
			t.Errorf("pane reply %+v, hook %+v: want the reply enriched by the hook, and the hook not counted",
				reply, hookRow(t, obs, stop))
		}
		for _, id := range []int64{other, late} {
			if o := hookRow(t, obs, id); !o.Counts {
				t.Errorf("hook %+v does not count, want a separate occurrence", o)
			}
			for _, o := range obs {
				if o.EnrichedBy == id {
					t.Errorf("hook %d enriches %+v, want no match", id, o)
				}
			}
		}
	})
	t.Run("hook before pane", func(t *testing.T) {
		k := newCapture(t, "s1", t.TempDir())
		k.pane("w1:p1", "claude", "")
		k.reads(t, "w1:p1", "claude-s40-0-base.txt", "claude-s40-1-idle.txt")
		k.must(t, Baseline, "w1:p1", "", GapNone)
		// The prompt holds the word ready, but a prompt is not an assistant reply.
		prompt := k.hookRecord(t, "prompt", store.SenderUnconfirmed, "Reply with only the word ready.", k.clock)
		stop := k.hookRecord(t, "stop", store.SenderAssistant, "ready", k.clock)
		k.must(t, Change, "w1:p1", "idle", GapNone)
		obs := k.rows(t).obs
		row := paneRow(t, obs, "⏺ ready")
		if row.EnrichedBy != stop || hookRow(t, obs, stop).Counts || !hookRow(t, obs, prompt).Counts {
			t.Errorf("pane reply %+v, stop %+v, prompt %+v: want the reply enriched by the stop only",
				row, hookRow(t, obs, stop), hookRow(t, obs, prompt))
		}
		n := 0
		for _, o := range obs {
			if o.EnrichedBy != 0 {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d enriched rows in %+v, want 1: a hook enriches one pane row", n, obs)
		}
	})
	t.Run("same correction", func(t *testing.T) {
		k := newCapture(t, "s1", t.TempDir())
		k.pane("w1:p1", "claude", "")
		k.reads(t, "w1:p1", "claude-s40-1-idle.txt")
		// The next read is the idle fixture with a new prompt (a correction) and its reply
		// above the input box.
		idle := k.f.reads["w1:p1"][0]
		box := strings.LastIndex(idle[:strings.LastIndex(idle, "\n❯")], "\n") + 1
		correction := "No, use tabs in hi.txt, not spaces."
		k.f.reads["w1:p1"] = append(k.f.reads["w1:p1"],
			idle[:box]+"❯ "+correction+"\n\n⏺ Done. hi.txt uses tabs.\n\n"+idle[box:])
		k.must(t, Baseline, "w1:p1", "", GapNone)
		prompt := k.hookRecord(t, "prompt", store.SenderUnconfirmed, correction, k.clock)
		k.must(t, Change, "w1:p1", "idle", GapNone)
		stop := k.hookRecord(t, "stop", store.SenderAssistant, "Done. hi.txt uses tabs.", k.clock)
		obs := k.rows(t).obs
		row := paneRow(t, obs, "❯ "+correction)
		if row.Sender != store.SenderUnconfirmed || row.EnrichedBy != prompt || hookRow(t, obs, prompt).Counts {
			t.Errorf("pane prompt %+v, hook %+v: want the prompt enriched by the hook, and the hook not counted",
				row, hookRow(t, obs, prompt))
		}
		if reply := paneRow(t, obs, "⏺ Done. hi.txt uses tabs."); reply.EnrichedBy != stop || hookRow(t, obs, stop).Counts {
			t.Errorf("pane reply %+v, hook %+v: want the reply enriched by the stop", reply, hookRow(t, obs, stop))
		}
		n := 0
		for _, o := range obs {
			if o.Counts && strings.Contains(o.Excerpt, correction) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d counted rows hold the correction in %+v, want 1 occurrence", n, obs)
		}
	})
}

// A tool hook matches the tool call line of the pane; a cut pane text matches the start of the
// hook text.
func TestSameText(t *testing.T) {
	for _, tc := range []struct {
		pane, hook string
		want       bool
	}{
		{"Bash(lsnosuchfile-zq)", "lsnosuchfile-zq", true},
		{"ROW1ROW2", "ROW1ROW2ROW3", true},
		{"ready", "Replywithonlythewordready.", false},
		{"", "", false},
	} {
		if got := sameText(tc.pane, tc.hook); got != tc.want {
			t.Errorf("sameText(%q, %q) = %v, want %v", tc.pane, tc.hook, got, tc.want)
		}
	}
}

// HookText runs the capture filters on hook text: a Brain block and a secret do not stay.
func TestHookText(t *testing.T) {
	key := "sk-" + "ant-api03-" + strings.Repeat("xY9_", 8)
	if got, _, _ := HookText("claude", "use the key "+key+" here"); strings.Contains(got, key) || !strings.Contains(got, "[secret]") {
		t.Errorf("HookText with a secret = %q, want [secret] in place of the key", got)
	}
	long := strings.Repeat("word ", 200)
	if got, truncated, _ := HookText("claude", long); !truncated || len([]rune(got)) > 500 {
		t.Errorf("HookText of %d runes = %d runes, truncated %v, want ≤ 500 and truncated", len(long), len([]rune(got)), truncated)
	}
	if got, _, _ := HookText("claude", "\n  line one  \n\n line two\n"); got != "  line one\n line two" {
		t.Errorf("HookText = %q, want the blank lines dropped", got)
	}
}
