package hook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// startAt is start with the hook clock at at, so the receipt has that time.
func startAt(t *testing.T, f *fixture, p *adapter.Profile, env []string, input string, at time.Time) string {
	t.Helper()
	h := f.hook(p, env)
	h.Now = func() time.Time { return at }
	var out bytes.Buffer
	if err := h.Run(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	return briefOf(t, out.String())
}

// approveAt makes a draft of text and approves it at at, as brain remember and an approve in
// review do: the active revision comes from an approved proposal. key "" makes a global item.
func approveAt(t *testing.T, s *store.Store, key, text string, at time.Time) int64 {
	t.Helper()
	r := store.ItemRevision{Text: text, Scope: store.ScopeGlobal, State: store.StateDraft, Author: store.AuthorBrain,
		Origin: store.OriginRemember, Created: at}
	if key != "" {
		r.Scope, r.WorkspaceKey = store.ScopeWorkspace, &key
	}
	var id int64
	write(t, s, func(tx *sql.Tx) error {
		ctx := context.Background()
		var err error
		if id, _, err = store.CreateItem(ctx, tx, store.Item{ClaimID: text, Kind: store.KindFact, Created: at}, r); err != nil {
			return err
		}
		p, err := store.InsertProposal(ctx, tx, store.Proposal{ItemID: &id, BaseRevision: ptr(int64(1)), Text: text,
			Scope: r.Scope, Target: "fact", Diff: "+" + text, ExpectedBenefit: "b", Status: store.ProposalApproved,
			Created: at, Decided: at})
		if err != nil {
			return err
		}
		r.ItemID, r.State, r.Author, r.Origin, r.ProposalID = id, store.StateActive, store.AuthorYou, store.OriginReview, &p
		_, err = store.WriteRevision(ctx, tx, 1, r)
		return err
	})
	return id
}

// failedRun is one shell command of a hook record and its exit status; nil is none.
type failedRun struct {
	argv string
	exit *int
}

// runCommands stores the hook records of runs in the capture session of Claude agent session
// agent in pane w14:p1 of ws_a14, as brained and the tool hook do.
func runCommands(t *testing.T, s *store.Store, agent string, runs ...failedRun) {
	t.Helper()
	write(t, s, func(tx *sql.Tx) error {
		ctx := context.Background()
		key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyAgent, ID: agent}
		session, err := store.OpenSession(ctx, tx, key, "w14:p1", "ws_a14", testNow)
		if err != nil {
			return err
		}
		seg, err := store.StartSegment(ctx, tx, session, "w14:p1", "ws_a14", "visible", testNow)
		if err != nil {
			return err
		}
		for i, r := range runs {
			o, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: fmt.Sprintf("%s/hook/%d", agent, i),
				Source: store.SourceHook, SegmentID: seg, Event: adapter.EventTool, Excerpt: r.argv,
				Status: store.ObservationComplete, Sender: store.SenderTool, Counts: true, At: testNow})
			if err != nil {
				return err
			}
			if err := store.AddCommandRecord(ctx, tx, store.CommandRecord{ObservationID: o, Argv: r.argv, CWD: "/w",
				ExitStatus: r.exit}); err != nil {
				return err
			}
		}
		return nil
	})
}

// handoffPrep is the store of TestHandoffLines: Claude session claude-1 starts in w14 at
// testNow; you approved one item before it and two after its start; its last runs of two
// commands failed. It returns the ids of the two items approved after the start.
func handoffPrep(t *testing.T, f *fixture) (global, workspace int64) {
	t.Helper()
	s := openStore(t, f.mainStore)
	approveAt(t, s, "", "approved before the start", testNow.Add(-time.Minute))
	closeStore(t, s)
	startAt(t, f, claude(t), paneA14(f), startInput("claude-1", "/t/claude-1.jsonl"), testNow)
	s = openStore(t, f.mainStore)
	defer closeStore(t, s)
	global = approveAt(t, s, "", "approved global item", testNow.Add(10*time.Minute))
	workspace = approveAt(t, s, "ws_a14", "approved workspace item", testNow.Add(10*time.Minute))
	runCommands(t, s, "claude-1", failedRun{"go test ./x", ptr(1)}, failedRun{"go test ./y", ptr(1)},
		failedRun{"go test ./y", nil}, failedRun{"make lint", ptr(2)})
	f.gen = build(t, s, f.mainStore)
	return global, workspace
}

// AC #273 1, 2 and 4: the brief of the next session names, below the last session line, the
// items approved since that session started and the commands whose last run failed in it,
// newest first. The receipt's brief hash covers the lines; the built output holds none.
func TestHandoffLines(t *testing.T) {
	f := newFixture(t)
	global, workspace := handoffPrep(t, f)
	got := startAt(t, f, codex(t), paneA14(f), startInput("codex-1", "/t/codex-1.jsonl"), testNow.Add(time.Hour))
	want := lastLine("/t/claude-1.jsonl") +
		fmt.Sprintf("Approved since that session started: [%d], [%d]\n", global, workspace) +
		"Failed in that session (last run): make lint (exit 2)\n" +
		"Failed in that session (last run): go test ./x (exit 1)\n<!-- brain:end -->"
	if !strings.HasSuffix(got, want) {
		t.Errorf("codex brief =\n%s\nwant it to end with\n%s", got, want)
	}
	ds := deliveries(t, f.mainStore)
	sum := sha256.Sum256([]byte(got))
	if d := ds[len(ds)-1]; d.BriefHash == nil || *d.BriefHash != hex.EncodeToString(sum[:]) {
		t.Errorf("receipt = %s, want the hash of the printed brief with the handoff lines", show(d))
	}
	for _, name := range []string{"global.md", "workspaces/ws_a14.md", "manifest.json"} {
		b, err := os.ReadFile(filepath.Join(f.mainStore, outDir, fmt.Sprintf("gen-%d", f.gen), name))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("Approved since")) || bytes.Contains(b, []byte("Failed in that session")) {
			t.Errorf("%s holds a handoff line:\n%s", name, b)
		}
	}
}

// AC #273 4 and 6: the handoff lines show only under the last session line, so a start with no
// last session line gets none (S48); after brain forget --session of the last session, its
// failed commands never show, and the session before it is the last one.
func TestNoHandoffLines(t *testing.T) {
	forget := func(t *testing.T, f *fixture) {
		t.Helper()
		s := openStore(t, f.mainStore)
		defer closeStore(t, s)
		// As brain forget --session claude-1 does (internal/cli forget.go).
		write(t, s, func(tx *sql.Tx) error {
			ctx := context.Background()
			ss, err := store.SessionsByID(ctx, tx, "default", "claude-1")
			if err != nil {
				return err
			}
			for _, sn := range ss {
				if err := store.ForgetSession(ctx, tx, sn.ID); err != nil {
					return err
				}
			}
			return store.ClearTranscriptPaths(ctx, tx, "default", "claude-1")
		})
	}
	pending := func(t *testing.T, f *fixture) {
		t.Helper()
		s := openStore(t, f.mainStore)
		defer closeStore(t, s)
		write(t, s, func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE outputs SET holds_forgotten_text = 1`)
			return err
		})
	}
	claude0 := func(t *testing.T, f *fixture) {
		t.Helper()
		startAt(t, f, claude(t), paneA14(f), startInput("claude-0", "/t/claude-0.jsonl"), testNow.Add(-time.Hour))
	}
	tests := []struct {
		name          string
		env           func(f *fixture) []string
		before, after func(t *testing.T, f *fixture) // before and after handoffPrep
		want          string                         // the line of the last session; "" for none
		silent        bool                           // the hook prints nothing
	}{
		{name: "unmapped workspace", env: func(f *fixture) []string { return environ(f.mainSock, "", "w15:p1", "w15") }},
		{name: "workspace part withheld", env: func(f *fixture) []string { return environ(f.mainSock, "", "w16:p1", "w16") }},
		// S79: B's w14 is another workspace than A's w14.
		{name: "session of another workspace", env: func(f *fixture) []string { return environ(f.workSock, "work", "w14:p1", "w14") }},
		{name: "forget pending", env: paneA14, after: pending, silent: true},
		{name: "forgotten session", env: paneA14, after: forget},
		{name: "forgotten session after an earlier one", env: paneA14, before: claude0, after: forget,
			want: "Last session in this workspace: claude, 2026-09-28 09:30, transcript /t/claude-0.jsonl\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.before != nil {
				tt.before(t, f)
			}
			handoffPrep(t, f)
			if tt.after != nil {
				tt.after(t, f)
			}
			got := startAt(t, f, codex(t), tt.env(f), startInput("codex-1", "/t/codex-1.jsonl"), testNow.Add(time.Hour))
			if tt.silent != (got == "") {
				t.Fatalf("brief =\n%s\nwant printed %v", got, !tt.silent)
			}
			if strings.Contains(got, "Failed in that session") {
				t.Errorf("brief =\n%s\nwant no failed line", got)
			}
			if tt.want == "" && (strings.Contains(got, "Last session") || strings.Contains(got, "Approved since")) {
				t.Errorf("brief =\n%s\nwant no last session line and no approved line", got)
			}
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Errorf("brief =\n%s\nwant the line\n%s", got, tt.want)
			}
		})
	}
}
