package brained

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// matchWindow is the time window of the match of a hook record and a pane observation: a pane
// read can come some minutes after the hook, because brained reads a pane only when its agent
// status changes.
const matchWindow = 5 * time.Minute

// HookText runs the capture filters on text from a hook payload of harness,
// as on pane text: it drops Brain blocks, the CLI scaffolding and a compaction summary, puts
// [secret] in place of each secret, and cuts the result to the excerpt limit. brain says that
// the text held a Brain block, so the text after it is Brain-origin.
func HookText(harness, text string) (excerpt string, truncated, brain bool) {
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimRight(l, " \t\r"); strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	idx := make([]int, len(lines))
	for i := range idx {
		idx[i] = i
	}
	// One sender for the whole text: the hook event gives it.
	r := read{lines: lines, senders: make([]store.Sender, len(lines)), clean: redact(harness, lines)}
	ps, _ := parts(r, idx, nil)
	var kept []string
	for _, p := range ps {
		kept = append(kept, p.lines...)
		brain = brain || p.brain
	}
	excerpt, truncated = filter(strings.Join(kept, "\n"))
	return excerpt, truncated, brain
}

// Match links observation o of session to its counterpart from the other source, when one
// matches: the same session, a time within matchWindow, the pane sender of the
// hook event, and the same normalized text. The hook record then enriches the pane observation
// and is not a second occurrence. The nearest counterpart in time wins. o.ID is the stored
// row; o.Source is herdr or hook, or transcript for a record of the rollout of a launched Codex
// session, which matches as a hook record does (E24-S5). A pane record matches hook records
// only: brained reads a rollout after the pane reads.
func Match(ctx context.Context, tx *sql.Tx, harness string, session int64, o store.Observation) error {
	c, err := counterpart(ctx, tx, harness, session, o)
	if err != nil || c == 0 {
		return err
	}
	if o.Source != store.SourceHerdr {
		return store.Enrich(ctx, tx, c, o.ID)
	}
	return store.Enrich(ctx, tx, o.ID, c)
}

// counterpart returns the id of the observation from the other source that Match links to
// observation o of session, or 0 when none matches.
func counterpart(ctx context.Context, tx *sql.Tx, harness string, session int64, o store.Observation) (int64, error) {
	if o.Status == store.ObservationGap || o.Excerpt == "" {
		return 0, nil
	}
	other := store.SourceHook
	if o.Source != store.SourceHerdr {
		other = store.SourceHerdr
	}
	cs, err := store.Counterparts(ctx, tx, session, other, o.At, matchWindow)
	if err != nil {
		return 0, err
	}
	for _, c := range cs {
		pane, hook := o, c
		if o.Source != store.SourceHerdr {
			pane, hook = c, o
		}
		if pane.Sender == paneSender(hook) &&
			sameText(normalized(harness, pane.Excerpt), normalized(harness, hook.Excerpt)) {
			return c.ID, nil
		}
	}
	return 0, nil
}

// paneSender is the sender that a pane read gives the text of hook observation h: the
// pane shows a tool call on a line with the assistant mark, as "⏺ Bash(ls)".
func paneSender(h store.Observation) store.Sender {
	if h.Event == adapter.EventTool {
		return store.SenderAssistant
	}
	return h.Sender
}

// normalized is text without the sender marks of harness and without white space, so a
// rewrap or a mark does not change it.
func normalized(harness, text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		for _, m := range marks[harness] {
			if strings.HasPrefix(l, m.prefix) {
				lines[i] = l[len(m.prefix):]
				break
			}
		}
	}
	return squash(strings.Join(lines, "\n"))
}

// sameText reports whether normalized pane text and hook text match: the pane text holds
// the hook text, as a tool call "Bash(ls)" holds the command, or the pane text is the start
// of the hook text, because the pane read cuts a long text at the excerpt limit.
func sameText(pane, hook string) bool {
	return pane != "" && hook != "" &&
		(strings.Contains(pane, hook) || strings.HasPrefix(hook, pane))
}
