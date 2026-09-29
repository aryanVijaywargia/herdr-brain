package brained

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// matchWindow is the time window of the S35 match [NEW]: a pane read can come some minutes
// after the hook, because brained reads a pane only when its agent status changes.
const matchWindow = 5 * time.Minute

// HookText runs the capture filters of §6.3 and §6.7 on text from a hook payload of harness,
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
// matches (§6.4, S35): the same session, a time within matchWindow, the pane sender of the
// hook event, and the same normalized text. The hook record then enriches the pane observation
// and is not a second occurrence. The nearest counterpart in time wins. o.ID is the stored
// row; o.Source is herdr or hook.
func Match(ctx context.Context, tx *sql.Tx, harness string, session int64, o store.Observation) error {
	if o.Status == store.ObservationGap || o.Excerpt == "" {
		return nil
	}
	other := store.SourceHook
	if o.Source == store.SourceHook {
		other = store.SourceHerdr
	}
	cs, err := store.Counterparts(ctx, tx, session, other, o.At, matchWindow)
	if err != nil {
		return err
	}
	for _, c := range cs {
		pane, hook := o, c
		if o.Source == store.SourceHook {
			pane, hook = c, o
		}
		if pane.Sender != paneSender(hook) ||
			!sameText(normalized(harness, pane.Excerpt), normalized(harness, hook.Excerpt)) {
			continue
		}
		return store.Enrich(ctx, tx, pane.ID, hook.ID)
	}
	return nil
}

// paneSender is the sender that a pane read gives the text of hook observation h [NEW]: the
// pane shows a tool call on a line with the assistant mark, as "⏺ Bash(ls)".
func paneSender(h store.Observation) store.Sender {
	if h.Event == adapter.EventTool {
		return store.SenderAssistant
	}
	return h.Sender
}

// normalized is text without the sender marks of harness and without white space, so a
// rewrap or a mark does not change it [NEW].
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

// sameText reports whether normalized pane text and hook text match [NEW]: the pane text holds
// the hook text, as a tool call "Bash(ls)" holds the command, or the pane text is the start
// of the hook text, because the pane read cuts a long text at the excerpt limit.
func sameText(pane, hook string) bool {
	return pane != "" && hook != "" &&
		(strings.Contains(pane, hook) || strings.HasPrefix(hook, pane))
}
