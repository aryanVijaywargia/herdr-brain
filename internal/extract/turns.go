package extract

import (
	"regexp"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// promptEvent is the event of a hook prompt record (adapter.EventPrompt).
const promptEvent = "prompt"

// agentError matches a line of agent reply or tool output that is only an error of the CLI
// [NEW, M2 eval run 6, 2026-10-10]: claude "You've hit your weekly limit · resets ..." and an
// "API Error: ..." line, and codex "exceeded retry limit, last status: 429 Too Many Requests"
// and "unexpected status 503 Service Unavailable: ...". The sender marks come off first.
var agentError = regexp.MustCompile(`^(?:You(?:'|’)ve hit your .*limit.*|Claude (?:AI )?usage limit reached.*|` +
	`API Error\b.*|exceeded retry limit, last status: \d+.*|unexpected status \d{3}\b.*)$`)

// lineMarks are the sender marks and the error mark of the pane lines of claude and codex.
const lineMarks = "⏺⎿■•└ \t "

// errorOnly reports whether text holds no line but errors of the CLI (agentError).
func errorOnly(text string) bool {
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(strings.TrimLeft(l, lineMarks)); l != "" && !agentError.MatchString(l) {
			return false
		}
	}
	return true
}

// works reports whether observation in shows work of the agent: a command record, or agent reply
// or tool output that is not Brain's own text and not only an error of the CLI (#377).
func works(in store.SignalInput) bool {
	if in.Command != nil {
		return true
	}
	return (in.Sender == store.SenderAssistant || in.Sender == store.SenderTool) && !in.BrainOrigin &&
		in.Status != store.ObservationGap && !errorOnly(in.Excerpt)
}

// unrun returns the observations of the turns that the agent did not run and that give no
// signal (#377, #379, #381). A turn that the agent did not run has no work: the agent did not
// run its prompt, as at a usage limit or an error, and its text is only the prompt and the error.
// Such a turn gives no signal when its finished session has no agent work at all, so the session
// makes no call, or when a later prompt repeats it (resent): you sent the task again, so the
// later telling is the one telling. Else it stays, as a last correction that the agent did not
// answer is still a correction. ins are the observations in time order (store.SignalInputs),
// done holds the finished sessions and resent the observations that a later prompt repeats.
// As in the command chains of the signal rules, a turn is the part of a session from one hook
// prompt record to the next; the text before the first prompt joins the first turn, and a pane
// read that a hook prompt enriches joins the turn of that prompt. A turn is known only when a
// later prompt ended it or its session is finished. Only a session with hook prompt records has
// turns: the CLI also records the replies and the commands there. A CLI with no prompt hook
// (agy) shows no reply mark, so its text stays as it is. A git observation is Brain's own line,
// not text of a turn.
func unrun(ins []store.SignalInput, done, resent map[int64]bool) map[int64]bool {
	prompts := map[int64]int{} // the turn of each hook prompt record
	count := map[int64]int{}   // the hook prompts of each session
	for _, in := range ins {
		if in.Source == store.SourceHook && in.Event == promptEvent {
			prompts[in.ID] = count[in.SessionID]
			count[in.SessionID]++
		}
	}
	type turn struct {
		session int64
		n       int
	}
	of := map[int64]int{}
	work, again := map[turn]bool{}, map[turn]bool{}
	worked := map[int64]bool{} // the sessions with agent work
	seen := map[int64]int{}    // the hook prompts of each session so far
	for _, in := range ins {
		if p, ok := prompts[in.ID]; ok {
			seen[in.SessionID] = p + 1
		}
		t := max(seen[in.SessionID]-1, 0)
		if p, ok := prompts[in.EnrichedBy]; ok && in.EnrichedBy != 0 {
			t = p
		}
		of[in.ID] = t
		k := turn{in.SessionID, t}
		work[k] = work[k] || works(in)
		again[k] = again[k] || resent[in.ID]
		worked[in.SessionID] = worked[in.SessionID] || works(in)
	}
	skip := map[int64]bool{}
	for _, in := range ins {
		t, last := of[in.ID], count[in.SessionID]-1
		k := turn{in.SessionID, t}
		known := t < last || done[in.SessionID]
		idle := !worked[in.SessionID] && done[in.SessionID]
		if last >= 0 && in.Source != store.SourceGit && !work[k] && known && (idle || again[k]) {
			skip[in.ID] = true
		}
	}
	return skip
}

// repeated returns the observations that a later prompt repeats: the earlier occurrences of the
// repeated prompt signals of sigs.
func repeated(sigs []signal.Signal) map[int64]bool {
	ids := map[int64]bool{}
	for _, s := range sigs {
		if s.Kind != signal.RepeatedPrompt {
			continue
		}
		for _, id := range s.Observations[1:] {
			ids[id] = true
		}
	}
	return ids
}
