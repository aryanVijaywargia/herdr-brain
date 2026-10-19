package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/fingerprint"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// clock returns a clock that moves one minute on each read, so the cleanup of a later call
// finds every old generation past the hook time cap.
func clock() func() time.Time {
	t := testNow
	return func() time.Time {
		t = t.Add(time.Minute)
		return t
	}
}

// forget runs brain forget from pane w14:p1 with the clock of the fixture's calls.
func (f *fixture) forget(t *testing.T, now func() time.Time, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &out)
	e.Now = now
	err := Forget(context.Background(), e, args)
	return out.String(), err
}

func (f *fixture) mustForget(t *testing.T, now func() time.Time, args ...string) string {
	t.Helper()
	out, err := f.forget(t, now, args...)
	if err != nil {
		t.Fatalf("Forget(%q) = %v", args, err)
	}
	return out
}

// approved remembers text in workspace w14, approves it, and returns the item id.
func (f *fixture) approved(t *testing.T, text string) int64 {
	t.Helper()
	p := f.remember(t, text)
	f.mustReview(t, fmt.Sprint(p), "--approve")
	return *f.rows(t).proposal(t, p).ItemID
}

// scan returns every store field (table.column) and every file in the out folder that holds
// marker: all columns of all tables, not only the listed text fields.
func (f *fixture) scan(t *testing.T, marker string) []string {
	t.Helper()
	ctx := context.Background()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var found []string
	write(t, s, func(tx *sql.Tx) error {
		var fields [][2]string
		rows, err := tx.QueryContext(ctx, `SELECT m.name, c.name FROM sqlite_master m, pragma_table_info(m.name) c
			WHERE m.type = 'table'`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var tc [2]string
			if err := rows.Scan(&tc[0], &tc[1]); err != nil {
				return err
			}
			fields = append(fields, tc)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, tc := range fields {
			var n int
			q := fmt.Sprintf(`SELECT COUNT(*) FROM "%s" WHERE instr(CAST("%s" AS TEXT), ?) > 0`, tc[0], tc[1])
			if err := tx.QueryRowContext(ctx, q, marker).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				found = append(found, tc[0]+"."+tc[1])
			}
		}
		return nil
	})
	err := filepath.WalkDir(filepath.Join(f.store, outDir), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err == nil && bytes.Contains(b, []byte(marker)) {
			found = append(found, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// forgetPending reads the mark that makes the hook withhold the brief (E7).
func (f *fixture) forgetPending(t *testing.T) bool {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var pending bool
	write(t, s, func(tx *sql.Tx) (err error) {
		pending, err = store.ForgetPending(context.Background(), tx)
		return err
	})
	return pending
}

// AC (item part): a unique marker in two revisions, a proposal and the applied edits of
// two builds. After forget and its rebuild, no store field and no output file holds it; the
// item keeps its ids and a tombstone, and forget reports done.
func TestForgetScan(t *testing.T) {
	const marker = "marker-s47 use pnpm"
	f := newFixture(t)
	id := f.approved(t, marker)
	if _, err := f.pin(t, fmt.Sprint(id)); err != nil { // one more revision and build that hold the marker
		t.Fatalf("Pin() = %v", err)
	}
	if found := f.scan(t, marker); len(found) == 0 {
		t.Fatal("no copy of the marker before forget")
	}

	out := f.mustForget(t, clock(), fmt.Sprint(id))
	if !strings.HasSuffix(out, fmt.Sprintf("forget item %d: done\n", id)) {
		t.Errorf("Forget() printed %q, want done", out)
	}
	if found := f.scan(t, marker); len(found) > 0 {
		t.Errorf("after forget the marker is in %v, want nowhere", found)
	}
	if f.forgetPending(t) {
		t.Error("ForgetPending() = true after done, want false: the hook would withhold")
	}
	if b := f.brief(t); !strings.Contains(b, liveText) {
		t.Errorf("brief = %q, want the other items still delivered", b)
	}

	ctx := context.Background()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	write(t, s, func(tx *sql.Tx) error {
		it, err := store.ItemByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if it.State != store.StateRetired {
			t.Errorf("item = %+v, want retired", it)
		}
		ts, err := store.TombstonesByClaim(ctx, tx, it.ClaimID)
		if err != nil {
			return err
		}
		if len(ts) != 1 {
			t.Errorf("tombstones of claim %s = %v, want one", it.ClaimID, ts)
		}
		// The audit rows keep their metadata; the forget build logged no old bytes.
		edits, err := store.AppliedEditsWithItem(ctx, tx, id)
		if err != nil {
			return err
		}
		if len(edits) < 2 {
			t.Errorf("applied edits of item %d = %v, want the approve and forget builds", id, edits)
		}
		for _, e := range edits {
			ae, err := store.AppliedEditByID(ctx, tx, e)
			if err != nil {
				return err
			}
			if ae.File == "" || ae.ContentHash == "" || ae.Generation == 0 {
				t.Errorf("applied edit %d = %+v, want its file, generation and hash", e, ae)
			}
		}
		return nil
	})
}

// AC (audit rule with E6): while the forget is open, the build logs a metadata-only row for a
// file that held the item: no old bytes, but its file, generation, hash and revisions.
func TestForgetBuildLogsNoOldBytes(t *testing.T) {
	f := newFixture(t)
	id := f.approved(t, "marker-audit")
	before := f.rows(t).newest
	f.mustForget(t, clock(), fmt.Sprint(id))

	ctx := context.Background()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	write(t, s, func(tx *sql.Tx) error {
		ids, err := store.AppliedEditsWithItem(ctx, tx, id)
		if err != nil {
			return err
		}
		var forgetRow *store.AppliedEdit
		for _, e := range ids {
			ae, err := store.AppliedEditByID(ctx, tx, e)
			if err != nil {
				return err
			}
			if ae.Generation > before {
				forgetRow = &ae
			}
		}
		switch {
		case forgetRow == nil:
			t.Errorf("no applied edit of the forget build (generation > %d)", before)
		case forgetRow.ContentBefore != nil || forgetRow.File != "workspaces/ws_a14.md" || forgetRow.ContentHash == "" ||
			len(forgetRow.RevisionsBefore) == 0:
			t.Errorf("forget build row = %+v, want workspaces/ws_a14.md with its hash and revisions and no old bytes", *forgetRow)
		}
		return nil
	})
}

// AC: a hand edit blocks the compile, so forget reports pending, names the conflict, and
// the hook withholds the brief.
func TestForgetPendingOnHandEdit(t *testing.T) {
	f := newFixture(t)
	id := f.approved(t, "marker-s77")
	f.handEdit(t, f.rows(t).newest)

	out := f.mustForget(t, clock(), fmt.Sprint(id))
	for _, want := range []string{"pending", "conflict: output generation", "brain repair --confirm"} {
		if !strings.Contains(out, want) {
			t.Errorf("Forget() printed %q, want %q", out, want)
		}
	}
	if strings.Contains(out, "done") {
		t.Errorf("Forget() printed %q, want no done", out)
	}
	if !f.forgetPending(t) {
		t.Error("ForgetPending() = false, want true: the hook must withhold the brief")
	}
	// The store part is done already; only the output holds the text.
	if found := f.scan(t, "marker-s77"); len(found) == 0 || slicesAnyStore(found) {
		t.Errorf("marker in %v, want only in output files", found)
	}
}

// slicesAnyStore reports whether one of found is a store field, not a file path.
func slicesAnyStore(found []string) bool {
	for _, s := range found {
		if !filepath.IsAbs(s) {
			return true
		}
	}
	return false
}

// AC: the same scan after a blocked compile and a later repair: brain repair --confirm
// replaces the hand-edited generation, and a second forget reports done, with no copy left.
func TestForgetAfterBlockedCompile(t *testing.T) {
	const marker = "marker-blocked"
	f := newFixture(t)
	id := f.approved(t, marker)
	f.handEdit(t, f.rows(t).newest)
	now := clock()
	if out := f.mustForget(t, now, fmt.Sprint(id)); !strings.Contains(out, "pending") {
		t.Fatalf("Forget() printed %q, want pending", out)
	}

	var buf bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &buf)
	e.Now = now
	if err := Repair(context.Background(), e, []string{"--confirm"}); err != nil {
		t.Fatalf("Repair(--confirm) = %v", err)
	}
	out := f.mustForget(t, now, fmt.Sprint(id))
	if !strings.HasSuffix(out, fmt.Sprintf("forget item %d: done\n", id)) {
		t.Errorf("second Forget() printed %q, want done", out)
	}
	if found := f.scan(t, marker); len(found) > 0 {
		t.Errorf("after repair the marker is in %v, want nowhere", found)
	}
	if f.forgetPending(t) {
		t.Error("ForgetPending() = true after done, want false")
	}
}

// AC: a copy of the same claim in another scope is listed first and kept, and it is still
// delivered; the selected item's revisions and derivatives are gone, and forget reports done.
func TestForgetKeepsCopies(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := openStore(t, f.store)
	ids := map[string]int64{}
	write(t, s, func(tx *sql.Tx) error {
		for _, key := range []string{"", "ws_a14", "ws_a15"} {
			r := store.ItemRevision{Text: "dup " + key, Scope: store.ScopeGlobal, State: store.StateActive,
				Author: store.AuthorYou, Origin: store.OriginRemember, Created: testNow}
			if key != "" {
				r.Scope, r.WorkspaceKey = store.ScopeWorkspace, ptr(key)
			}
			id, _, err := store.CreateItem(ctx, tx, store.Item{ClaimID: "c-dup", Kind: store.KindFact, Created: testNow}, r)
			if err != nil {
				return err
			}
			ids[key] = id
		}
		return nil
	})
	if _, err := output.Build(ctx, s, filepath.Join(f.store, outDir), nil, now); err != nil {
		t.Fatalf("Build() = %v", err)
	}
	closeStore(t, s)

	out := f.mustForget(t, clock(), fmt.Sprint(ids["ws_a14"]))
	want := fmt.Sprintf("kept: item %d, a copy of the same claim (global)\n"+
		"kept: item %d, a copy of the same claim (another workspace)\n"+
		"item %d: its text is removed from the store\n"+
		"forget item %d: done\n", ids[""], ids["ws_a15"], ids["ws_a14"], ids["ws_a14"])
	if out != want {
		t.Errorf("Forget() printed\n%s\nwant\n%s", out, want)
	}
	if found := f.scan(t, "dup ws_a14"); len(found) > 0 {
		t.Errorf("the forgotten copy is in %v, want nowhere", found)
	}
	for _, key := range []string{"", "ws_a15"} {
		if found := f.scan(t, "dup "+key); len(found) == 0 {
			t.Errorf("the copy in %q is gone, want it kept", key)
		}
	}
	b, err := output.Read(filepath.Join(f.store, outDir), "ws_a15")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.Global, "dup ") || !strings.Contains(b.Workspace, "dup ws_a15") {
		t.Errorf("brief of ws_a15 = %q + %q, want both copies still delivered", b.Global, b.Workspace)
	}
}

// The E4 scope check: an item of another workspace is not found, with no text. A forgotten
// item cannot come back with undo.
func TestForgetScopeAndUndo(t *testing.T) {
	f := newFixture(t)
	other := f.addActive(t, ptr("ws_a15"), "blog secret")
	out, err := f.forget(t, clock(), fmt.Sprint(other))
	if !errors.Is(err, errNotFound) || strings.Contains(out+err.Error(), "blog secret") {
		t.Errorf("Forget(other workspace) = %q, %v; want not found with no text", out, err)
	}
	if found := f.scan(t, "blog secret"); len(found) == 0 {
		t.Error("the item of the other workspace is gone, want it kept")
	}

	id := f.approved(t, "gone soon")
	f.mustForget(t, clock(), fmt.Sprint(id))
	if _, err := f.undo(t, fmt.Sprint(id)); err == nil || !strings.Contains(err.Error(), "forgotten") {
		t.Errorf("Undo(forgotten) = %v, want refused", err)
	}
	if _, err := f.forget(t, clock(), "x"); err == nil {
		t.Error("Forget(x) = nil, want a usage error")
	}
}

// A kept conflict keeps forget pending even when its manifest does not name the item: the
// changed bytes may hold anything (review E4-S6). A repair then lets forget report done.
func TestForgetPendingOnConflictWithoutItem(t *testing.T) {
	const marker = "marker-unnamed"
	f := newFixture(t)
	id := f.approved(t, marker)
	if _, err := f.undo(t, fmt.Sprint(id)); err != nil { // retires the item: no output names it
		t.Fatalf("Undo() = %v", err)
	}
	gen := f.rows(t).newest
	if err := os.WriteFile(filepath.Join(f.genDir(gen), "global.md"), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}

	now := clock()
	if out := f.mustForget(t, now, fmt.Sprint(id)); !strings.Contains(out, "pending") || strings.Contains(out, "done") {
		t.Errorf("Forget() printed %q, want pending", out)
	}
	if !f.forgetPending(t) {
		t.Error("ForgetPending() = false, want true: the hook must withhold the brief")
	}

	var buf bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &buf)
	e.Now = now
	if err := Repair(context.Background(), e, []string{"--confirm"}); err != nil {
		t.Fatalf("Repair(--confirm) = %v", err)
	}
	if out := f.mustForget(t, now, fmt.Sprint(id)); !strings.HasSuffix(out, "done\n") {
		t.Errorf("Forget() after repair printed %q, want done", out)
	}
	if found := f.scan(t, marker); len(found) > 0 {
		t.Errorf("after repair the marker is in %v, want nowhere", found)
	}
}

// onWrite is a writer that calls fn with the text of each write before it keeps it.
type onWrite struct {
	bytes.Buffer
	fn func(p string)
}

func (w *onWrite) Write(p []byte) (int, error) {
	w.fn(string(p))
	return w.Buffer.Write(p)
}

// AC E11-S6: brain forget --session lists the evidence of the session while it still exists,
// then deletes it: no store field holds the text of the session after. The same id in another
// herdr session and the items stay.
func TestForgetSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const marker = "zq-forget-session-marker"
	s := openStore(t, f.store)
	var claim string
	write(t, s, func(tx *sql.Tx) (err error) {
		claim, err = store.ClaimID(ctx, tx, store.ClaimKey{Situation: "go build", Rule: "run go mod tidy first"}, testNow)
		if err != nil {
			return err
		}
		cand := store.Candidate{ClaimID: claim, Scope: store.ScopeWorkspace, WorkspaceKey: "ws_a14", Kind: store.CandidateFailure}
		for i, herdrSession := range []string{"default", "other"} {
			key := store.SessionKey{HerdrSession: herdrSession, Harness: "claude", Kind: store.KeyCapture, ID: "cap-1"}
			id, err := store.OpenSession(ctx, tx, key, "w14:p1", "ws_a14", testNow)
			if err != nil {
				return err
			}
			if err := store.EnrichSession(ctx, tx, key, "agent-1"); err != nil {
				return err
			}
			seg, err := store.StartSegment(ctx, tx, id, "w14:p1", "ws_a14", "visible", testNow)
			if err != nil {
				return err
			}
			for j, src := range []string{store.SourceHerdr, store.SourceHook} {
				o, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: fmt.Sprint(i, j), Source: src,
					SegmentID: seg, Event: "tool", Excerpt: herdrSession + " " + marker, Status: store.ObservationComplete,
					Sender: store.SenderTool, Counts: true, At: testNow})
				if err != nil {
					return err
				}
				if _, err := store.Attach(ctx, tx, cand, o, store.DefaultGates(), signal.Supports); err != nil {
					return err
				}
				if src == store.SourceHook {
					if err := store.AddCommandRecord(ctx, tx, store.CommandRecord{ObservationID: o,
						Argv: "echo " + herdrSession + " " + marker, CWD: "/w"}); err != nil {
						return err
					}
				}
			}
			// A start receipt of the agent session in each herdr session (E15-S4).
			if _, err := store.InsertDelivery(ctx, tx, store.Delivery{Profile: "claude", HerdrSession: &herdrSession,
				PaneID: "w14:p1", AgentSessionID: ptr("agent-1"), Result: store.ResultUnmapped, At: testNow,
				TranscriptPath: ptr("/t/" + herdrSession + ".jsonl")}); err != nil {
				return err
			}
		}
		return nil
	})
	closeStore(t, s)

	if _, err := f.forget(t, clock(), "--session"); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("forget --session with no id = %v, want the usage", err)
	}
	if _, err := f.forget(t, clock(), "--session", "nosuch"); err == nil {
		t.Error("forget of an unknown session = nil, want an error")
	}
	var listed []string
	links := -1
	out := &onWrite{fn: func(p string) {
		if strings.HasPrefix(p, "session ") {
			listed = f.scan(t, "default "+marker) // AC: the list shows before anything is deleted
		}
		if strings.HasPrefix(p, "candidate ") {
			links = sessionLinks(t, f, 1) // AC: the candidate list shows before the delete
		}
	}}
	e := f.env(f.sock, "w14:p1", "w14", &bytes.Buffer{})
	e.Stdout = out
	if err := Forget(ctx, e, []string{"--session", "agent-1"}); err != nil {
		t.Fatalf("forget --session = %v", err)
	}
	want := "session 1: claude in pane w14:p1, capture id cap-1, agent session \"agent-1\": 2 observations, 1 command records, 2 evidence links, 0 prompt index rows, 0 outcomes\n" +
		"agent session \"agent-1\": 1 receipts with a transcript path: the paths are cleared, the receipts stay\n" +
		"candidate 1: workspace failure of claim " + claim + ", 4 occurrences in 2 sessions: its links to this evidence go, and it is counted again\n" +
		"1 candidates use this evidence; no item uses capture evidence yet: nothing else to keep or forget\n" +
		"forget session agent-1: done; its text is deleted, and new text is captured as usual\n"
	if out.String() != want {
		t.Errorf("forget --session printed\n%s\nwant\n%s", out.String(), want)
	}
	if len(listed) != 2 {
		t.Errorf("at the list the text was in %v, want observations and command records", listed)
	}
	if links != 2 {
		t.Errorf("at the candidate list the session had %d evidence links, want 2", links)
	}
	// AC: no evidence link of the session stays, and the candidate keeps the other session's.
	if got := sessionLinks(t, f, 1); got != 0 {
		t.Errorf("after the forget the session has %d evidence links, want 0", got)
	}
	s = openStore(t, f.store)
	write(t, s, func(tx *sql.Tx) error {
		c, err := store.CandidateByID(ctx, tx, 1)
		if err == nil && (c.Count != 2 || c.Sessions != 1) {
			t.Errorf("after the forget the candidate is %+v, want 2 occurrences in 1 session", c)
		}
		return err
	})
	closeStore(t, s)
	// AC: a scan of every table finds no text of the session.
	if got := f.scan(t, "default "+marker); len(got) != 0 {
		t.Errorf("after the forget %v hold the text of the session", got)
	}
	if got := f.scan(t, "other "+marker); len(got) != 2 {
		t.Errorf("the other herdr session's text is in %v, want observations and command records", got)
	}
	if got := f.scan(t, liveText); len(got) == 0 {
		t.Error("the forget of a session removed an item")
	}
	// AC E15-S4: the forget clears the transcript path of the session's receipt and keeps the
	// receipt; the other herdr session keeps its path.
	s = openStore(t, f.store)
	defer closeStore(t, s)
	write(t, s, func(tx *sql.Tx) error {
		for id, want := range map[int64]string{1: "", 2: "/t/other.jsonl"} {
			d, err := store.DeliveryByID(ctx, tx, id)
			if err != nil {
				return err
			}
			got := ""
			if d.TranscriptPath != nil {
				got = *d.TranscriptPath
			}
			if got != want {
				t.Errorf("after the forget receipt %d has transcript path %q, want %q", id, got, want)
			}
		}
		return nil
	})
}

// AC (index part): brain forget --session lists the proposals that used the prompt index
// rows of the session while the rows still exist, then deletes their text and hashes. The
// proposal stays.
func TestForgetSessionPromptIndex(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const marker = "zq-prompt-index-marker"
	prompt := "run make Foo " + marker
	hash := fingerprint.Prompt(prompt)
	var proposal int64
	s := openStore(t, f.store)
	write(t, s, func(tx *sql.Tx) error {
		claim, err := store.ClaimID(ctx, tx, store.ClaimKey{Situation: "make", Rule: "run make Foo"}, testNow)
		if err != nil {
			return err
		}
		key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyCapture, ID: "cap-1"}
		id, err := store.OpenSession(ctx, tx, key, "w14:p1", "ws_a14", testNow)
		if err != nil {
			return err
		}
		seg, err := store.StartSegment(ctx, tx, id, "w14:p1", "ws_a14", "visible", testNow)
		if err != nil {
			return err
		}
		o, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: "p1", Source: store.SourceHook,
			SegmentID: seg, Event: "prompt", Excerpt: prompt, Status: store.ObservationComplete,
			Sender: store.SenderUnconfirmed, Counts: true, At: testNow})
		if err != nil {
			return err
		}
		if err := store.IndexPrompt(ctx, tx, o, hash, prompt); err != nil {
			return err
		}
		cand, err := store.Attach(ctx, tx, store.Candidate{ClaimID: claim, Scope: store.ScopeWorkspace,
			WorkspaceKey: "ws_a14", Kind: store.CandidateCorrection}, o, store.DefaultGates(), signal.Supports)
		if err != nil {
			return err
		}
		ws := "ws_a14"
		item, err := store.InsertItem(ctx, tx, store.Item{ClaimID: claim, CurrentRevision: 1, Kind: store.KindPreference,
			Scope: store.ScopeWorkspace, WorkspaceKey: &ws, State: store.StateDraft, Created: testNow})
		if err != nil {
			return err
		}
		base := int64(1)
		proposal, err = store.ProposeCandidate(ctx, tx, cand, store.Proposal{ItemID: &item, BaseRevision: &base,
			Text: "make: run make Foo", Scope: store.ScopeWorkspace, Target: "workspace", Diff: "+ make: run make Foo",
			Status: store.ProposalPending, Created: testNow})
		return err
	})
	closeStore(t, s)

	rows := -1
	out := &onWrite{fn: func(p string) {
		if strings.HasPrefix(p, "proposal ") {
			rows = len(f.scan(t, marker)) // the proposal list shows before the delete
		}
	}}
	e := f.env(f.sock, "w14:p1", "w14", &bytes.Buffer{})
	e.Stdout = out
	if err := Forget(ctx, e, []string{"--session", "cap-1"}); err != nil {
		t.Fatalf("forget --session = %v", err)
	}
	for _, want := range []string{"1 prompt index rows, 0 outcomes\n", fmt.Sprintf("\nproposal %d used its prompts", proposal)} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("forget --session printed\n%s\nwant %q", out.String(), want)
		}
	}
	if rows < 2 {
		t.Errorf("at the proposal list the prompt was in %d fields, want the observation and the prompt index", rows)
	}
	if got := f.scan(t, marker); len(got) != 0 {
		t.Errorf("after the forget %v hold the prompt", got)
	}
	s = openStore(t, f.store)
	defer closeStore(t, s)
	write(t, s, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM prompt_index WHERE hash = ?`, hash).Scan(&n); err != nil || n != 0 {
			t.Errorf("prompt index rows with the hash = %d, %v, want 0", n, err)
		}
		p, err := store.ProposalByID(ctx, tx, proposal)
		if err == nil && p.Status != store.ProposalPending {
			t.Errorf("proposal after the forget = %+v, want it pending", p)
		}
		return err
	})
}

// AC E15-S4: brain forget --session clears the transcript path of an agent session that has a
// start receipt and no capture session, and the last session lookup of the next start finds
// nothing. An id with neither is still an error.
func TestForgetSessionReceiptOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := openStore(t, f.store)
	write(t, s, func(tx *sql.Tx) error {
		_, err := store.InsertDelivery(ctx, tx, store.Delivery{Profile: "claude", HerdrSession: ptr("default"), PaneID: "w14:p1",
			AgentSessionID: ptr("agent-1"), WorkspaceKey: ptr("ws_a14"), Result: store.ResultEmitted, At: testNow,
			TranscriptPath: ptr("/t/agent-1.jsonl")})
		return err
	})
	closeStore(t, s)
	var out bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &bytes.Buffer{})
	e.Stdout = &out
	if err := Forget(ctx, e, []string{"--session", "agent-1"}); err != nil {
		t.Fatalf("forget --session = %v", err)
	}
	if want := "agent session \"agent-1\": 1 receipts with a transcript path: the paths are cleared, the receipts stay\n"; !strings.HasPrefix(out.String(), want) {
		t.Errorf("forget --session printed\n%s\nwant it to start with\n%s", out.String(), want)
	}
	s = openStore(t, f.store)
	write(t, s, func(tx *sql.Tx) error {
		d, ok, err := store.LastSession(ctx, tx, "ws_a14", "agent-2")
		if ok {
			t.Errorf("after the forget the next start shows %s", *d.TranscriptPath)
		}
		return err
	})
	closeStore(t, s)
	if err := Forget(ctx, e, []string{"--session", "agent-1"}); err == nil {
		t.Error("a second forget of a session with no capture session and no path = nil, want an error")
	}
}

// sessionLinks is the number of evidence links of capture session id.
func sessionLinks(t *testing.T, f *fixture, id int64) int {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var n int
	write(t, s, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM evidence_links WHERE session_id = ?`, id).Scan(&n)
	})
	return n
}

// AC #412: the list line of a history scan session, which has no pane, says no pane.
func TestForgetScanSession(t *testing.T) {
	f := newFixture(t)
	sessions := scanRow(t, f.store, store.ScanExtracting, map[string]store.ScanCounts{})
	out := f.mustForget(t, clock(), "--session", "c1")
	want := fmt.Sprintf("session %d: claude, agent id c1, ", sessions[0])
	if !strings.Contains(out, want) || strings.Contains(out, "in pane") {
		t.Errorf("forget --session printed\n%s\nwant %q and no pane", out, want)
	}
}
