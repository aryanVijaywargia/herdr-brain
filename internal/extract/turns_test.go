package extract

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// tr is one record of a session with hook records (hooked).
type tr struct {
	o    store.Observation
	cmd  *store.CommandRecord
	echo bool // a pane read of the prompt that the hook prompt record above enriches (brained.Match)
}

// asked is the hook prompt record of text.
func asked(text string) tr {
	return tr{o: store.Observation{Source: store.SourceHook, Event: "prompt", Excerpt: text, Sender: store.SenderUnconfirmed}}
}

// echoed is the pane read of a prompt, text with its prompt mark, that the hook prompt record
// above it enriches: it holds the occurrence, and the hook record no longer counts.
func echoed(text string) tr {
	return tr{o: store.Observation{Source: store.SourceHerdr, Event: "working", Excerpt: text,
		Sender: store.SenderUnconfirmed}, echo: true}
}

// replied is the hook stop record of the agent reply text.
func replied(text string) tr {
	return tr{o: store.Observation{Source: store.SourceHook, Event: "stop", Excerpt: text, Sender: store.SenderAssistant}}
}

// ran is the hook tool record of command argv with its command record, exit status 0.
func ran(argv string) tr {
	c := command(argv)
	return tr{o: store.Observation{Source: store.SourceHook, Event: "tool", Excerpt: argv, Sender: store.SenderTool}, cmd: &c}
}

// shown is pane text of sender at agent status event.
func shown(event string, sender store.Sender, text string) tr {
	return tr{o: store.Observation{Source: store.SourceHerdr, Event: event, Excerpt: text, Sender: sender}}
}

// hooked stores an ended session of agent id of harness in ws-A with recs, one second apart
// from start. It returns the session id and the observation ids.
func (f *fixture) hooked(harness, id string, start time.Time, recs ...tr) (int64, []int64) {
	f.t.Helper()
	key := store.SessionKey{HerdrSession: "default", Harness: harness, Kind: store.KeyAgent, ID: id}
	f.panes++
	pane := fmt.Sprintf("w1:p%d", f.panes)
	var sid int64
	var obs []int64
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		if sid, err = store.OpenSession(ctx, tx, key, pane, "ws-A", start); err != nil {
			return err
		}
		seg, err := store.StartSegment(ctx, tx, sid, pane, "ws-A", "recent_unwrapped", start)
		if err != nil {
			return err
		}
		at := start
		var prompt int64
		for i, r := range recs {
			at = at.Add(time.Second)
			o := r.o
			o.CaptureIdentity, o.SegmentID, o.Status, o.Counts, o.At = fmt.Sprintf("%s-%d", id, i), seg,
				store.ObservationComplete, true, at
			oid, err := store.AddObservation(ctx, tx, o)
			if err != nil {
				return err
			}
			obs = append(obs, oid)
			switch {
			case o.Event == "prompt":
				prompt = oid
			case r.echo:
				if err := store.Enrich(ctx, tx, oid, prompt); err != nil {
					return err
				}
			}
			if r.cmd != nil {
				c := *r.cmd
				c.ObservationID = oid
				if err := store.AddCommandRecord(ctx, tx, c); err != nil {
					return err
				}
			}
		}
		return store.EndSession(ctx, tx, key, at)
	})
	return sid, obs
}

// The limit texts of the M2 eval run 6 (2026-10-10, codex 0.160.1, claude 2.1.296): the codex
// error lines under a prompt, which capture keeps in the prompt read, and the claude limit line
// and dialog [run6 screens §A, §B].
const (
	codexLine429 = "■ exceeded retry limit, last status: 429 Too Many Requests"
	codexLine503 = "■ unexpected status 503 Service Unavailable: All managed Codex accounts are temporarily " +
		"unavailable for this runtime\nrequest., url: http://127.0.0.1:64305/responses"
	claudeLimit  = "  ⎿  You've hit your weekly limit · resets Oct 12 at 9:30am (UTC)"
	claudeDialog = "▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔\n   What do you want to do?\n   ❯ 1. Stop and wait for limit to reset\n" +
		"     2. Wait here, then continue automatically at Oct 12 at 9:30am\n     3. Upgrade your plan\n" +
		"   Enter to confirm · Esc to cancel"
	// The run 6 tasks H44a and H45a, and a claude task, with the paths changed.
	pinTask   = "PATCH /notes/:id should accept pinned true or false, and GET /notes should list pinned notes first. Add tests."
	trimTask  = "Trim titles on POST, PUT and PATCH before you validate them. Add tests for titles with spaces."
	jsonTask  = "When a POST /notes body is not valid JSON, return 400 with an error field. Add tests for both cases and run them."
	commitAsk = "Commit this work with a clear message."
)

// AC #377 (2) and #379 (2), with the run 6 shapes: a session with no agent work, only your
// prompts and limit or error text, makes no batch call: the codex sessions with only 429 and 503
// answers, a task that repeats an earlier one, and the claude session with only the weekly limit
// line and dialog, whose "Stop" made a correction signal. Their batches are empty with no call,
// and the watermark moves. A session with the same prompts and agent work still makes its call.
func TestNoWorkNoCall(t *testing.T) {
	f := newFixture(t)
	f.hooked("codex", "done", t0, asked(pinTask), echoed("› "+pinTask), ran("npm test"), replied("Pinned notes come first."))
	f.mustRun()
	calls := f.cli.calls()
	codex, _ := f.hooked("codex", "s29", t0.Add(time.Minute), asked(pinTask), echoed("› "+pinTask+"\n"+codexLine429),
		asked(commitAsk), echoed("› "+commitAsk+"\n"+codexLine503))
	claude, _ := f.hooked("claude", "s2", t0.Add(2*time.Minute), asked(jsonTask), echoed("❯ "+jsonTask),
		shown("blocked", store.SenderTool, claudeLimit), shown("blocked", store.SenderUnconfirmed, claudeDialog))
	f.mustRun()
	if n := f.cli.calls() - calls; n != 0 {
		t.Errorf("calls of the sessions with no agent work = %d, want 0", n)
	}
	for _, sid := range []int64{codex, claude} {
		var found bool
		for _, b := range f.batches() {
			if b.SessionID != sid {
				continue
			}
			found = true
			if b.Status != store.BatchEmpty || b.ModelCalls != 0 || f.watermark(sid, "ws-A") != b.LastObservation {
				t.Errorf("batch of session %d = %+v, want empty with no call, and the watermark at its end", sid, b)
			}
		}
		if !found {
			t.Errorf("session %d has no batch, want an empty one", sid)
		}
	}

	f.hooked("claude", "work", t0.Add(3*time.Minute), asked(jsonTask), echoed("❯ "+jsonTask),
		shown("blocked", store.SenderUnconfirmed, claudeDialog), ran("npm test"), replied("Invalid JSON gets 400."))
	f.mustRun()
	if n := f.cli.calls() - calls; n != 1 {
		t.Errorf("calls after a session with work = %d, want 1", n)
	}
}

// evidenceOf returns the candidates that hold one of obs as evidence.
func (f *fixture) evidenceOf(obs ...int64) int {
	f.t.Helper()
	ids := make([]string, len(obs))
	for i, o := range obs {
		ids[i] = fmt.Sprint(o)
	}
	return f.count(`SELECT COUNT(DISTINCT candidate_id) FROM evidence_links WHERE observation_id IN (` +
		strings.Join(ids, ",") + `)`)
}

// AC #381 with the run 6 shapes (R6-7): a task that you send again after the agent could not run
// it (429 and 503 answers) is one task: no correction, no rule and no proposal, and the
// extractor never sees it as a signal. Each claim of the extractor takes every ref of its prompt
// (claimOf), as the run 6 correction claim did.
func TestTaskSentAgain(t *testing.T) {
	f := newFixture(t)
	start := t0
	session := func(id string, recs ...tr) []int64 {
		start = start.Add(time.Minute)
		_, obs := f.hooked("codex", id, start, recs...)
		return obs
	}
	var tasks []int64
	add := func(obs []int64, at ...int) {
		for _, i := range at {
			tasks = append(tasks, obs[i])
		}
	}
	add(session("s29", asked(pinTask), echoed("› "+pinTask+"\n"+codexLine429)), 0, 1)
	add(session("s30", asked(trimTask), echoed("› "+trimTask+"\n"+codexLine429)), 0, 1)
	add(session("s31", asked(pinTask), echoed("› "+pinTask+"\n"+codexLine503)), 0, 1)
	add(session("s32", asked(pinTask), echoed("› "+pinTask+"\n"+codexLine503)), 0, 1)
	add(session("s33", asked(pinTask), echoed("› "+pinTask), ran("npm test"), replied("Pinned notes come first."),
		asked(trimTask), echoed("› "+trimTask), ran("npm test"), replied("Titles are trimmed.")), 0, 1, 4, 5)
	f.mustRun()
	if n := len(f.proposals()); n != 0 {
		t.Errorf("proposals = %d, want 0", n)
	}
	if n := f.evidenceOf(tasks...); n != 0 {
		t.Errorf("candidates with a task as evidence = %d, want 0", n)
	}
	for _, p := range f.cli.prompts {
		if strings.Contains(p, "PATCH /notes/:id") || strings.Contains(p, "Trim titles") {
			t.Errorf("an extractor prompt holds a task: %q", p)
		}
	}
}

// AC #381: the earlier session made a batch (a correction with agent work), and then the agent
// answered your task with an error; the same task in a later session, which the agent runs, is
// one telling: neither occurrence is evidence, and no proposal comes. A real correction still
// counts: a task that you send again after the agent ran it, and a correction with a marker
// after the agent's work, reach the correction gate of 2 and make a proposal.
func TestTaskAgainAfterError(t *testing.T) {
	t.Run("after an error answer", func(t *testing.T) {
		f := newFixture(t)
		_, s1 := f.hooked("claude", "s1", t0, asked("set up the project"), echoed("❯ set up the project"),
			ran("npm install"), replied("Installed."), asked(pnpm), echoed("❯ "+pnpm), ran("pnpm install"),
			replied("Switched to pnpm."), asked(jsonTask), echoed("❯ "+jsonTask),
			replied("API Error: 500 {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"Internal server error\"}}"))
		f.mustRun()
		if bs := f.batches(); len(bs) != 1 || bs[0].Status != store.BatchCommitted || f.cli.calls() != 1 {
			t.Fatalf("batches of the first session = %+v (%d calls), want one committed batch", bs, f.cli.calls())
		}
		_, s2 := f.hooked("claude", "s2", t0.Add(time.Minute), asked(jsonTask), echoed("❯ "+jsonTask), ran("npm test"),
			replied("Invalid JSON gets 400."))
		f.mustRun()
		if n := f.evidenceOf(s1[8], s1[9], s2[0], s2[1]); n != 0 {
			t.Errorf("candidates with the task as evidence = %d, want 0", n)
		}
		for _, p := range f.proposals() {
			t.Errorf("proposal %+v, want none: the pnpm correction has one telling", p)
		}
	})

	for name, again := range map[string]string{"task again after the answer": jsonTask, "correction": pnpm} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.hooked("claude", "s1", t0, asked(again), echoed("❯ "+again), ran("npm test"), replied("Done."))
			f.hooked("claude", "s2", t0.Add(time.Minute), asked(again), echoed("❯ "+again), ran("npm test"), replied("Done."))
			f.mustRun()
			cs := slices.DeleteFunc(f.candidates(), func(c store.Candidate) bool { return c.Kind != store.CandidateCorrection })
			if len(cs) != 1 || cs[0].Count != 2 || len(f.proposals()) != 1 {
				t.Errorf("corrections = %+v, %d proposals, want one of count 2 with its proposal", cs, len(f.proposals()))
			}
		})
	}
}

// AC #377, #381: the turns of a session with hook prompt records. A turn with a reply, a tool
// call or tool output is run; a turn with only the prompt, its pane read and errors of the CLI is
// not. Such a turn gives no signal when its finished session has no work at all, or when a later
// prompt repeats it; else it stays, as a last correction that got only an error. The text
// before the first prompt joins the first turn, and a pane read that its hook prompt enriches
// joins that prompt's turn. The last turn of a session that is not finished is not known yet. A
// session with no hook prompt record (agy) has no turns.
func TestUnrun(t *testing.T) {
	in := func(id, session int64, source, event string, sender store.Sender, text string) store.SignalInput {
		return store.SignalInput{Observation: store.Observation{ID: id, Source: source, Event: event, Sender: sender,
			Excerpt: text, Status: store.ObservationComplete}, SessionID: session}
	}
	ins := []store.SignalInput{
		in(1, 1, store.SourceHerdr, "idle", store.SenderUnconfirmed, "OpenAI Codex"),
		in(2, 1, store.SourceHook, "prompt", store.SenderUnconfirmed, pinTask),
		in(3, 1, store.SourceHerdr, "working", store.SenderUnconfirmed, "› "+pinTask+"\n"+codexLine429),
		in(4, 1, store.SourceHook, "prompt", store.SenderUnconfirmed, trimTask),
		in(5, 1, store.SourceHerdr, "working", store.SenderAssistant, "• Ran npm test"),
		in(6, 1, store.SourceHook, "prompt", store.SenderUnconfirmed, pnpm),
		in(7, 1, store.SourceHook, "stop", store.SenderAssistant, "API Error: Rate limit reached"),
		in(8, 2, store.SourceHook, "prompt", store.SenderUnconfirmed, jsonTask),
		in(9, 2, store.SourceHerdr, "blocked", store.SenderTool, claudeLimit),
		in(10, 2, store.SourceHook, "prompt", store.SenderUnconfirmed, commitAsk),
		in(11, 3, store.SourceHerdr, "working", store.SenderUnconfirmed, pinTask),
		in(13, 4, store.SourceHook, "prompt", store.SenderUnconfirmed, jsonTask),
		in(14, 4, store.SourceHerdr, "working", store.SenderUnconfirmed, "❯ "+jsonTask),
		in(15, 4, store.SourceHerdr, "blocked", store.SenderTool, claudeLimit),
	}
	// The pane read of a prompt that comes before its hook record joins the turn of that record.
	echo := in(12, 1, store.SourceHerdr, "working", store.SenderUnconfirmed, "› "+pnpm)
	echo.EnrichedBy = 6
	ins = slices.Insert(ins, 5, echo)
	got := unrun(ins, map[int64]bool{1: true, 3: true, 4: true}, map[int64]bool{3: true, 8: true})
	want := map[int64]bool{1: true, 2: true, 3: true, 8: true, 9: true, 13: true, 14: true, 15: true}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("unrun() = %v, want %v", got, want)
	}
}

// AC #381 (2) (Sol review): a correction after the agent's work that the agent did not run, as
// it got only an API error or the session ended with no answer, is still a correction: it makes
// a batch call, and the claim holds it as evidence.
func TestLastCorrectionKept(t *testing.T) {
	for name, after := range map[string][]tr{
		"error answer": {replied("API Error: Rate limit reached")},
		"no answer":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			recs := append([]tr{asked("set up the project"), echoed("❯ set up the project"), ran("npm install"),
				replied("Installed with npm."), asked(pnpm), echoed("❯ " + pnpm)}, after...)
			_, obs := f.hooked("claude", "s1", t0, recs...)
			f.mustRun()
			if f.cli.calls() != 1 || f.evidenceOf(obs[5]) != 1 {
				t.Errorf("%d calls, %d candidates with the correction, want 1 call and its candidate", f.cli.calls(),
					f.evidenceOf(obs[5]))
			}
		})
	}
}
