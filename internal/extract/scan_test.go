package extract

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// scanned stores a session of agent id that the history scan imported, in workspace key
// ws ("" for the global pool): one segment with no pane, the texts as records of source
// transcript with sender unconfirmed, and its extraction watermark at its end. Each text is a
// turn of its own. It returns the finding observations of the texts.
func (f *fixture) scanned(id, ws string, texts ...string) []store.FindingObs {
	f.t.Helper()
	recs := make([]rec, len(texts))
	for i, t := range texts {
		recs[i] = rec{o: store.Observation{Event: "prompt", Excerpt: t, Sender: store.SenderUnconfirmed}}
	}
	return f.scannedRecs(id, ws, recs...)
}

// scannedRecs is scanned with records recs: each record of sender unconfirmed starts a turn.
func (f *fixture) scannedRecs(id, ws string, recs ...rec) []store.FindingObs {
	f.t.Helper()
	key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyAgent, ID: id}
	f.panes++
	at := t0.Add(time.Duration(f.panes) * time.Minute)
	var obs []store.FindingObs
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		sid, err := store.OpenSession(ctx, tx, key, "", ws, at)
		if err != nil {
			return err
		}
		seg, err := store.StartSegment(ctx, tx, sid, "", ws, "recent_unwrapped", at)
		if err != nil {
			return err
		}
		var turn int64
		for i, r := range recs {
			o := r.o
			o.CaptureIdentity, o.Source, o.SegmentID, o.Status, o.Counts, o.At = fmt.Sprintf("%s-%d", id, i), "transcript",
				seg, store.ObservationComplete, true, at.Add(time.Duration(i)*time.Second)
			oid, err := store.AddObservation(ctx, tx, o)
			if err != nil {
				return err
			}
			if r.cmd != nil {
				c := *r.cmd
				c.ObservationID = oid
				if err := store.AddCommandRecord(ctx, tx, c); err != nil {
					return err
				}
			}
			if o.Sender == store.SenderUnconfirmed {
				turn = oid
			}
			obs = append(obs, store.FindingObs{Observation: oid, Identity: o.CaptureIdentity, Turn: turn})
		}
		last := obs[len(obs)-1]
		if err := store.MoveWatermark(ctx, tx, store.Batch{SessionID: sid, WorkspaceKey: ws,
			LastObservation: last.Observation, LastCapture: last.Identity}); err != nil {
			return err
		}
		return store.EndSession(ctx, tx, key, at.Add(time.Minute))
	})
	return obs
}

// scanRow stores the history scan of the store with findings fs in its extracting state: the
// read ended, and the scan batches wait. Each turn of the findings has 1,000 tokens.
func (f *fixture) scanRow(fs ...store.Finding) {
	f.t.Helper()
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		if err := store.RequestScan(ctx, tx, t0); err != nil {
			return err
		}
		s, _, err := store.ClaimScan(ctx, tx, "default", t0)
		if err != nil {
			return err
		}
		var turns []store.ScanTurn
		for _, x := range fs {
			for _, o := range x.Obs {
				turns = append(turns, store.ScanTurn{ID: o.Turn, Tokens: 1000, Known: true})
			}
		}
		if err := store.AddScanTurns(ctx, tx, s.ID, turns); err != nil {
			return err
		}
		if err := store.SetScanFindings(ctx, tx, s.ID, fs); err != nil {
			return err
		}
		return store.SetScanStatus(ctx, tx, s.ID, store.ScanExtracting, t0)
	})
}

// sessionOf returns the session id of observation o.
func (f *fixture) sessionOf(o store.FindingObs) int64 {
	f.t.Helper()
	var sid int64
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT g.session_id FROM segments g JOIN observations o ON o.segment_id = g.id
			WHERE o.id = ?`, o.Observation).Scan(&sid)
	})
	return sid
}

// finding is finding id of kind in scope, with the observations of obs.
func finding(id int, kind store.CandidateKind, scope string, obs ...[]store.FindingObs) store.Finding {
	return store.Finding{ID: id, Kind: kind, Scope: scope, Obs: slices.Concat(obs...)}
}

// scan returns the scan row of the store and its progress by scope.
func (f *fixture) scan(scopes ...string) (store.Scan, map[string]int) {
	f.t.Helper()
	var s store.Scan
	p := map[string]int{}
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		if s, _, err = store.HistoryScan(ctx, tx); err != nil {
			return err
		}
		for _, scope := range scopes {
			if p[scope], err = store.ScanProgress(ctx, tx, s.ID, scope); err != nil {
				return err
			}
		}
		return nil
	})
	return s, p
}

// scanBatches returns the scan batches of the store, by id.
func (f *fixture) scanBatches() []store.Batch {
	f.t.Helper()
	return slices.DeleteFunc(f.batches(), func(b store.Batch) bool { return !b.Scan() })
}

// promptWith returns the prompt of the fake CLI that holds text, or "".
func (f *fixture) promptWith(text string) string {
	f.t.Helper()
	for _, p := range f.cli.prompts {
		if strings.Contains(p, text) {
			return p
		}
	}
	return ""
}

const (
	tabs  = "no, indent with tabs here, not spaces"
	short = "answer in short sentences please"
	vet   = "always run go vet before you commit"
)

// AC: the findings of one scope go to the extractor in finding order, up to the input byte cap,
// with the extractor input of that scope: the workspace and approved global context, or the global
// context alone. Each batch is a fixed run of findings that moves the scope progress at its
// commit. A second pass adds nothing, and the scan is done when every batch is committed.
func TestScanBatches(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	wsA, wsB := "ws-A", "ws-B"
	f.activeItem(nil, "global brief rule")
	f.activeItem(&wsA, "workspace A brief rule")
	f.activeItem(&wsB, "workspace B brief rule")
	a1, a2 := f.scanned("a1", "ws-A", pnpm, vet), f.scanned("a2", "ws-A", pnpm)
	a3, a4 := f.scanned("a3", "ws-A", tabs), f.scanned("a4", "ws-A", tabs)
	g1, g2 := f.scanned("g1", "", short), f.scanned("g2", "", short)
	f.scanRow(
		finding(1, store.CandidateCorrection, "ws-A", a1[:1], a2),
		finding(2, store.CandidateLesson, "ws-A", a1[1:]),
		finding(3, store.CandidateCorrection, "ws-A", a3, a4),
		finding(4, store.CandidateCorrection, "", g1, g2))
	// Findings 1 and 2 fit in one call; finding 3 goes into the next.
	f.caps = Caps{Calls: 40, Proposals: 15, InputBytes: 2*len(pnpm) + len(vet)}
	f.mustRun()

	if f.cli.calls() != 3 {
		t.Fatalf("calls = %d, want 3: two batches of ws-A and one of the global pool", f.cli.calls())
	}
	first, second, global := f.promptWith(vet), f.promptWith(tabs), f.promptWith(short)
	switch {
	case first == "" || !strings.Contains(first, pnpm) || strings.Contains(first, tabs):
		t.Errorf("first ws-A prompt =\n%s\nwant findings 1 and 2 only", first)
	case strings.Contains(second, pnpm) || strings.Contains(second, short):
		t.Errorf("second ws-A prompt =\n%s\nwant finding 3 only", second)
	}
	scopeInput(t, "workspace", first, []string{"global brief rule", "workspace A brief rule", scanHelp},
		[]string{"workspace B brief rule", globalHelp})
	scopeInput(t, "global pool", global, []string{"global brief rule", scanHelp, globalHelp},
		[]string{"workspace A brief rule", "workspace B brief rule", pnpm})
	var got []findingRange
	for _, b := range f.scanBatches() {
		if b.Status != store.BatchEmpty || b.ScanID == 0 || b.ModelCalls != 1 {
			t.Errorf("scan batch %+v, want an empty batch with one call", b)
		}
		got = append(got, findingRange{b.WorkspaceKey, b.FirstFinding, b.LastFinding})
	}
	slices.SortFunc(got, func(a, b findingRange) int { return a.first - b.first })
	if want := []findingRange{{"ws-A", 1, 2}, {"ws-A", 3, 3}, {"", 4, 4}}; !slices.Equal(got, want) {
		t.Errorf("scan batches = %+v, want %+v", got, want)
	}
	s, progress := f.scan("ws-A", "")
	if progress["ws-A"] != 3 || progress[""] != 4 {
		t.Errorf("progress = %v, want ws-A 3 and global 4", progress)
	}
	if s.Status != store.ScanExtracting {
		t.Errorf("scan status = %s, want extracting until the next pass", s.Status)
	}

	f.mustRun()
	if s, _ := f.scan(); f.cli.calls() != 3 || s.Status != store.ScanDone {
		t.Errorf("after a second pass: calls = %d, scan %s; want 3 calls and done", f.cli.calls(), s.Status)
	}
}

// findingRange is the scope and the finding range of a scan batch.
type findingRange struct {
	scope       string
	first, last int
}

// scopeInput checks that the prompt of a scan batch of the scope name holds each of has and none
// of not.
func scopeInput(t *testing.T, name, prompt string, has, not []string) {
	t.Helper()
	for _, s := range has {
		if !strings.Contains(prompt, s) {
			t.Errorf("%s prompt does not hold %q:\n%s", name, s, prompt)
		}
	}
	for _, s := range not {
		if strings.Contains(prompt, s) {
			t.Errorf("%s prompt holds %q:\n%s", name, s, prompt)
		}
	}
}

// AC: a scan batch commits as a live batch does (G3, S69): its claims make candidates and
// proposals in its scope. A forget during the call, or a scope progress that moved, discards the
// result, and the progress stays; a later pass extracts the kept evidence.
func TestScanBatchCommit(t *testing.T) {
	for name, tc := range map[string]struct {
		during func(f *fixture, sid int64)
		reason string
	}{
		"commits": {},
		"forget": {func(f *fixture, sid int64) {
			f.write(func(ctx context.Context, tx *sql.Tx) error { return store.ForgetSession(ctx, tx, sid) })
		}, "a forget removed input of the batch"},
		"progress moved": {func(f *fixture, _ int64) {
			f.write(func(ctx context.Context, tx *sql.Tx) error {
				_, err := store.MoveScanProgress(ctx, tx, 1, "ws-A", 0, 1)
				return err
			})
		}, "the watermark moved"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			a1, a2 := f.scanned("a1", "ws-A", pnpm), f.scanned("a2", "ws-A", pnpm)
			f.scanRow(finding(1, store.CandidateCorrection, "ws-A", a1, a2))
			sid := f.sessionOf(a2[0])
			f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
				if tc.during != nil {
					tc.during(f, sid)
				}
				return claimOf("use pnpm")(ctx, prompt)
			}
			f.mustRun()
			bs := f.scanBatches()
			if len(bs) != 1 {
				t.Fatalf("scan batches = %+v, want one", bs)
			}
			_, progress := f.scan("ws-A")
			if tc.reason != "" {
				if bs[0].Status != store.BatchDiscarded || bs[0].Reason != tc.reason {
					t.Errorf("batch = %+v, want discarded: %s", bs[0], tc.reason)
				}
				if n := f.count(`SELECT COUNT(*) FROM proposals`); n != 0 {
					t.Errorf("proposals = %d, want 0", n)
				}
				return
			}
			if bs[0].Status != store.BatchCommitted || progress["ws-A"] != 1 {
				t.Errorf("batch %+v, progress %v: want committed and progress 1", bs[0], progress)
			}
			ps := f.proposals()
			if len(ps) != 1 || ps[0].Scope != store.ScopeWorkspace || ps[0].Target != "workspace w1 (ws-A)" {
				t.Errorf("proposals = %+v, want one in ws-A", ps)
			}
		})
	}
}

// startScan starts scan batch b with store.StartScanBatch.
func (f *fixture) startScan(b store.Batch) (store.Batch, bool) {
	f.t.Helper()
	var got store.Batch
	var ok bool
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		got, ok, err = store.StartScanBatch(ctx, tx, b, f.now)
		return err
	})
	return got, ok
}

// batch returns the batch with row id.
func (f *fixture) batch(id int64) store.Batch {
	f.t.Helper()
	var b store.Batch
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		b, err = store.BatchByID(ctx, tx, id)
		return err
	})
	return b
}

// AC: one scan batch is active at a time. An active scan batch of another binding runs there,
// so this pass waits. An active scan batch of a binding at the start of its pass was left by a
// crash: that pass discards it, also when the binding has no scan batch of its own left.
func TestScanBatchActive(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	a1, a2 := f.scanned("a1", "ws-A", pnpm), f.scanned("a2", "ws-A", pnpm)
	f.scanRow(finding(1, store.CandidateCorrection, "ws-A", a1, a2))
	var other int64
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		other, err = store.OpenSession(ctx, tx, store.SessionKey{HerdrSession: "work", Harness: "claude",
			Kind: store.KeyAgent, ID: "x"}, "", "ws-B", t0)
		return err
	})
	running, ok := f.startScan(store.Batch{ScanID: 1, SessionID: other, WorkspaceKey: "ws-B", FirstCapture: "scan:9",
		LastCapture: "scan:9", FirstFinding: 9, LastFinding: 9})
	if !ok {
		t.Fatal("StartScanBatch of the other binding = false, want true")
	}
	f.mustRun()
	if f.cli.calls() != 0 || len(f.scanBatches()) != 1 {
		t.Fatalf("calls = %d, batches %+v: want no call while the batch of binding work runs", f.cli.calls(), f.scanBatches())
	}
	if ws := f.waiting(); len(ws) != 1 || ws[0].WorkspaceKey != "ws-A" || ws[0].Batches != 1 {
		t.Errorf("waiting = %+v, want the scan batch of ws-A", ws)
	}

	// The brained of work crashed, and a forget took its findings: its next pass has no scan batch
	// of its own, and it discards the crashed one.
	work := f.pass()
	work.HerdrSession = "work"
	if err := work.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b := f.batch(running.ID); b.Status != store.BatchDiscarded || b.Reason != "a crash stopped it" {
		t.Errorf("batch of work = %+v, want it discarded after the crash", b)
	}
	if f.cli.calls() != 0 {
		t.Fatalf("calls of the pass of work = %d, want 0", f.cli.calls())
	}
}

// AC: a scan batch of this binding that a crash left active is discarded at the next pass, which
// starts its own batch. A done batch adds nothing at a retry.
func TestScanBatchCrash(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	a1, a2 := f.scanned("a1", "ws-A", pnpm), f.scanned("a2", "ws-A", pnpm)
	f.scanRow(finding(1, store.CandidateCorrection, "ws-A", a1, a2))
	crashed, ok := f.startScan(store.Batch{ScanID: 1, SessionID: f.sessionOf(a1[0]), WorkspaceKey: "ws-A",
		FirstCapture: "scan:7", LastCapture: "scan:7", FirstFinding: 7, LastFinding: 7})
	if !ok {
		t.Fatal("StartScanBatch of this binding = false, want true")
	}
	f.mustRun()
	if f.cli.calls() != 1 {
		t.Fatalf("calls = %d, want 1", f.cli.calls())
	}
	var done store.Batch
	for _, b := range f.scanBatches() {
		switch b.ID {
		case crashed.ID:
			if b.Status != store.BatchDiscarded {
				t.Errorf("crashed batch = %+v, want discarded", b)
			}
		default:
			done = b
		}
	}
	if again, ok := f.startScan(done); !ok || again.ID != done.ID || again.ModelCalls != 1 {
		t.Errorf("StartScanBatch of a done batch = %+v, %v; want it as it is, with no call", again, ok)
	}
	if n := f.count(`SELECT COUNT(*) FROM extraction_calls`); n != 2 {
		t.Errorf("extractor calls = %d, want 2", n)
	}
}

// AC (S93): live batches go first; scan batches use what is left of the daily caps, and one scan
// batch can go over the remaining proposal room after its call. The rest waits, and the waiting
// and cap lines show it.
func TestScanCaps(t *testing.T) {
	f := newFixture(t)
	f.caps = Caps{Calls: 2, Proposals: 2, InputBytes: 2*len(tabs) + 2*len(short)}
	f.session("live", true, line{"ws-B", pnpm, t0}, line{"ws-B", pnpm, t0.Add(time.Second)})
	a1, a2 := f.scanned("a1", "ws-A", tabs, short), f.scanned("a2", "ws-A", tabs, short)
	a3, a4 := f.scanned("a3", "ws-A", vet), f.scanned("a4", "ws-A", vet)
	f.scanRow(
		finding(1, store.CandidateCorrection, "ws-A", a1[:1], a2[:1]),
		finding(2, store.CandidateCorrection, "ws-A", a1[1:], a2[1:]),
		finding(3, store.CandidateCorrection, "ws-A", a3, a4))
	f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
		refs := promptRefs(prompt)
		var claims []any
		for i := 0; i+1 < len(refs); i += 2 {
			claims = append(claims, replyOf(map[string]any{"rule": fmt.Sprintf("rule %d", i), "evidence_refs": refs[i : i+2]}))
		}
		return claudeReply(map[string]any{"claims": claims})
	}
	// The live batch gives one proposal, so one proposal of room is left; the first scan batch,
	// findings 1 and 2, gives two.
	f.mustRun()
	if f.cli.calls() != 2 || !strings.Contains(f.cli.prompts[0], pnpm) {
		t.Fatalf("calls = %d, first prompt %q: want the live batch first, then one scan batch", f.cli.calls(), f.cli.prompts[0])
	}
	if n := len(f.proposals()); n != 3 {
		t.Errorf("proposals = %d, want 3: the scan batch went over the room of 1 after its call", n)
	}
	ws := f.waiting()
	if len(ws) != 1 || ws[0].WorkspaceKey != "ws-A" || ws[0].Batches != 1 || !strings.Contains(ws[0].Why, "daily cap") {
		t.Errorf("waiting = %+v, want the last scan batch of ws-A behind the daily cap", ws)
	}
	f.now = f.now.Add(25 * time.Hour)
	f.mustRun()
	if s, p := f.scan("ws-A"); f.cli.calls() != 3 || p["ws-A"] != 3 {
		t.Errorf("the next day: calls = %d, progress %v, scan %s; want the rest extracted", f.cli.calls(), p, s.Status)
	}
}

// AC (S93): the daily caps cover the store, so live batches go first across bindings: a scan
// batch calls only while the caps keep room for each live batch of another binding that may call.
func TestScanCapsOtherBinding(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	f.caps = Caps{Calls: 1, Proposals: 15, InputBytes: 32 << 10}
	a1, a2 := f.scanned("a1", "ws-A", pnpm), f.scanned("a2", "ws-A", pnpm)
	f.scanRow(finding(1, store.CandidateCorrection, "ws-A", a1, a2))
	key := store.SessionKey{HerdrSession: "work", Harness: "claude", Kind: store.KeyAgent, ID: "live"}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		sid, err := store.OpenSession(ctx, tx, key, "w2:p1", "ws-B", t0)
		if err != nil {
			return err
		}
		seg, err := store.StartSegment(ctx, tx, sid, "w2:p1", "ws-B", "recent_unwrapped", t0)
		if err != nil {
			return err
		}
		if _, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: "live-0", Source: store.SourceHerdr,
			SegmentID: seg, Event: "working", Excerpt: tabs, Status: store.ObservationComplete, Sender: store.SenderYou,
			Counts: true, At: t0}); err != nil {
			return err
		}
		return store.EndSession(ctx, tx, key, t0)
	})
	f.mustRun()
	if f.cli.calls() != 0 {
		t.Fatalf("calls = %d, want 0: the live batch of binding work keeps the last call", f.cli.calls())
	}
	work := f.pass()
	work.HerdrSession = "work"
	if err := work.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.cli.calls() != 1 || !strings.Contains(f.cli.prompts[0], tabs) {
		t.Fatalf("calls = %d, want the live batch of work", f.cli.calls())
	}
	f.now = f.now.Add(25 * time.Hour)
	f.mustRun()
	if f.cli.calls() != 2 || len(f.scanBatches()) != 1 {
		t.Errorf("the next day: calls = %d, scan batches %+v; want the scan batch", f.cli.calls(), f.scanBatches())
	}
}

// AC (S95): a scan batch keeps only items, skills and tools. A claim of any other kind, as
// an automation, is dropped and counted in the brained log line with no text; it makes no
// response invalid. The prompt says that a repeat that fits a hook becomes an item or a tool.
func TestScanKinds(t *testing.T) {
	f := newFixture(t)
	a1, a2 := f.scanned("a1", "ws-A", pnpm), f.scanned("a2", "ws-A", pnpm)
	f.scanRow(finding(1, store.CandidateCorrection, "ws-A", a1, a2))
	automation := replyOf(map[string]any{"kind": "automation", "rule": "run the formatter after each edit"})
	f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
		automation["evidence_refs"] = promptRefs(prompt)
		return claudeReply(map[string]any{"claims": []any{automation}})
	}
	var notes []string
	p := f.pass()
	p.Note = func(line string) { notes = append(notes, line) }
	if err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.cli.prompts[0], `A repeat that fits a hook best, such as "run the formatter after each edit", becomes a lesson or a tool`) {
		t.Errorf("prompt does not say that a hook repeat becomes an item or a tool:\n%s", f.cli.prompts[0])
	}
	bs := f.scanBatches()
	if len(bs) != 1 || bs[0].Status != store.BatchEmpty {
		t.Fatalf("scan batches = %+v, want one empty batch", bs)
	}
	want := []string{fmt.Sprintf("batch %d: dropped 1 of 1 claims: claim 1: a scan batch keeps no claim of this kind", bs[0].ID)}
	if !slices.Equal(notes, want) {
		t.Errorf("notes = %q, want %q", notes, want)
	}
	if n := f.count(`SELECT COUNT(*) FROM proposals`); n != 0 {
		t.Errorf("proposals = %d, want 0", n)
	}

	// A live batch keeps its rule: a kind out of the schema makes the response invalid.
	f.session("live", true, line{"ws-B", tabs, t0})
	f.mustRun()
	if bs := f.batches(); bs[len(bs)-1].Scan() || bs[len(bs)-1].Status != store.BatchInvalid {
		t.Errorf("live batch = %+v, want invalid", bs[len(bs)-1])
	}
}

// AC (S90): the global pool keeps only an item claim that the extractor marks global. A skill, a
// tool and a claim about one project are dropped and counted. The tool claim of the test fails
// its own check first (its commands are not in the evidence), which drops it too.
func TestScanGlobalPool(t *testing.T) {
	f := newFixture(t)
	g1, g2 := f.scanned("g1", "", short), f.scanned("g2", "", short)
	f.scanRow(finding(1, store.CandidateCorrection, "", g1, g2))
	f.cli.reply = func(ctx context.Context, prompt string) ([]byte, error) {
		refs := promptRefs(prompt)
		claims := []any{replyOf(map[string]any{"kind": "preference", "situation": "answering", "rule": "answer short",
			"scope_guess": "global", "evidence_refs": refs})}
		claims = append(claims, replyOf(map[string]any{"rule": "in this repo use pnpm", "evidence_refs": refs}))
		for _, reply := range []func(context.Context, string) ([]byte, error){skillReply(nil), toolReply(nil)} {
			out, _ := reply(ctx, prompt)
			var res struct {
				StructuredOutput struct {
					Claims []any `json:"claims"`
				} `json:"structured_output"`
			}
			if err := json.Unmarshal(out, &res); err != nil {
				return nil, err
			}
			claims = append(claims, res.StructuredOutput.Claims...)
		}
		return claudeReply(map[string]any{"claims": claims})
	}
	var notes []string
	p := f.pass()
	p.Note = func(line string) { notes = append(notes, line) }
	if err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	ps := f.proposals()
	if len(ps) != 1 || ps[0].Scope != store.ScopeGlobal || ps[0].Target != "global" || !strings.Contains(ps[0].Text, "answer short") {
		t.Fatalf("proposals = %+v, want the one global item", ps)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "dropped 3 of 4 claims") ||
		!strings.Contains(notes[0], "claim 2: the global pool keeps no claim about one project") ||
		!strings.Contains(notes[0], "claim 3: the global pool keeps no skill or tool") ||
		!strings.Contains(notes[0], "claim 4: ") {
		t.Errorf("notes = %q, want 3 claims dropped: the project claim, the skill and the tool", notes)
	}
	if s, p := f.scan(""); p[""] != 1 || s.Status != store.ScanExtracting {
		t.Errorf("scan %s, progress %v: want progress 1", s.Status, p)
	}
}

// AC: the exact tool path treats scanned workspace sessions as live ones: an exact chain in
// 3 scanned sessions gets a tool draft with no model call.
func TestScanExactTool(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	prompt := rec{o: store.Observation{Event: "prompt", Excerpt: "check the code", Sender: store.SenderUnconfirmed}}
	var chain []store.FindingObs
	for i := range 3 {
		obs := f.scannedRecs(fmt.Sprintf("s%d", i), "ws-A", prompt, codexRec("go vet ./..."), codexRec("go test ./..."))
		chain = append(chain, obs[1:]...)
	}
	f.scanRow(finding(1, store.CandidateCommandChain, "ws-A", chain))
	f.mustRun()
	if ps := f.toolProposals(); len(ps) != 1 || !strings.Contains(ps[0].Text, `"go", "vet"`) {
		t.Fatalf("tool drafts = %+v, want one of the exact chain", ps)
	}
	if f.cli.calls() != 0 || len(f.scanBatches()) != 0 {
		t.Errorf("calls = %d, scan batches %+v; want none: the exact chain needs no scan batch", f.cli.calls(), f.scanBatches())
	}
	if s, _ := f.scan(); s.Status != store.ScanDone {
		t.Errorf("scan status = %s, want done", s.Status)
	}
}

// AC (S93, S75): with the extractor choice none, no call is made and the scan batches wait;
// with extraction off for a workspace, its scope makes 0 calls while the global pool goes on.
func TestScanSwitches(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	a1, a2 := f.scanned("a1", "ws-A", pnpm), f.scanned("a2", "ws-A", pnpm)
	g1, g2 := f.scanned("g1", "", short), f.scanned("g2", "", short)
	f.scanRow(finding(1, store.CandidateCorrection, "ws-A", a1, a2), finding(2, store.CandidateCorrection, "", g1, g2))
	f.write(func(ctx context.Context, tx *sql.Tx) error { return store.SetExtractorCLI(ctx, tx, "", "", t0) })
	f.mustRun()
	if f.cli.calls() != 0 {
		t.Fatalf("calls with no extractor = %d, want 0", f.cli.calls())
	}
	ws := f.waiting()
	if len(ws) != 2 || !ws[0].NoCLI || !ws[1].NoCLI {
		t.Errorf("waiting = %+v, want both scopes waiting for an extractor choice", ws)
	}

	f.write(func(ctx context.Context, tx *sql.Tx) error {
		if err := store.SetExtractorCLI(ctx, tx, store.ExtractorClaude, "", t0); err != nil {
			return err
		}
		return store.SetExtraction(ctx, tx, "ws-A", false, t0)
	})
	f.mustRun()
	if f.calls("ws-A") != 0 || f.calls("") != 1 {
		t.Errorf("calls: ws-A %d, global %d; want 0 and 1", f.calls("ws-A"), f.calls(""))
	}
}

// AC (S86): a scan proposal opens no review pane, as no agent pane asked for it: a lesson
// proposal of a scan batch reserves no suggestion.
func TestScanLessonNoPane(t *testing.T) {
	f := newFixture(t)
	a1 := f.scanned("a1", "ws-A", vet)
	f.scanRow(finding(1, store.CandidateLesson, "ws-A", a1))
	f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
		return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"kind": "lesson", "situation": "committing",
			"rule": "run go vet first", "evidence_refs": promptRefs(prompt)})}})
	}
	var suggested int
	p := f.pass()
	p.Suggest = func(context.Context, Suggestion) error { suggested++; return nil }
	if err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.proposals()); n != 1 {
		t.Fatalf("proposals = %d, want the lesson proposal", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM suggestions`); suggested != 0 || n != 0 {
		t.Errorf("suggestions: %d started, %d reserved; want none", suggested, n)
	}
}

// AC #413: the help text of a global pool batch says where its sessions come from, that a rule
// of one project is dropped, and that only a global claim that is no skill or tool comes back.
func TestGlobalHelpText(t *testing.T) {
	for _, want := range []string{
		"project folders that have no herdr workspace",
		"drops a rule about one project's code, files, commands, tools or release steps",
		"holds in every project", "scope_guess global", "no skill or tool",
	} {
		if !strings.Contains(globalHelp, want) {
			t.Errorf("globalHelp = %q, want %q", globalHelp, want)
		}
	}
	if strings.Contains(globalHelp, "belong to no project") {
		t.Errorf("globalHelp = %q, want no \"belong to no project\"", globalHelp)
	}
}
