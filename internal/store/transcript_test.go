package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// AC E15-S4, E15-S5: the last session of a workspace is its newest receipt with a transcript
// path and another agent session id, of any harness and any result except "withheld:
// identity"; a receipt of another workspace or
// with no path never counts. brain forget --session clears the paths of one agent session.
func TestLastSession(t *testing.T) {
	_, s := newTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 2, 14, 5, 0, 0, time.UTC)
	str := func(v string) *string { return &v }
	ds := []Delivery{
		{Profile: "claude", AgentSessionID: str("a"), WorkspaceKey: str("ka"), Result: ResultEmitted, TranscriptPath: str("/t/a1")},
		{Profile: "codex", AgentSessionID: str("b"), WorkspaceKey: str("ka"), Result: ResultWithheld,
			WithheldReason: str("inconsistent"), TranscriptPath: str("/t/b")},
		{Profile: "claude", AgentSessionID: str("a"), WorkspaceKey: str("ka"), Result: ResultEmitted, TranscriptPath: str("/t/a2")},
		{Profile: "claude", AgentSessionID: str("c"), WorkspaceKey: str("ka"), Result: ResultEmitted},
		{Profile: "claude", AgentSessionID: str("f"), WorkspaceKey: str("ka"), Result: ResultWithheld,
			WithheldReason: str("identity"), TranscriptPath: str("/t/f")},
		{Profile: "claude", AgentSessionID: str("g"), WorkspaceKey: str("kc"), Result: ResultWithheld,
			WithheldReason: str("forget pending"), TranscriptPath: str("/t/g")},
		{Profile: "claude", AgentSessionID: str("h"), WorkspaceKey: str("kc"), Result: ResultWithheld,
			WithheldReason: str("identity"), TranscriptPath: str("/t/h")},
		{Profile: "claude", AgentSessionID: str("d"), WorkspaceKey: str("kb"), Result: ResultEmitted, TranscriptPath: str("/t/d")},
		{Profile: "claude", AgentSessionID: str("e"), Result: ResultUnmapped, TranscriptPath: str("/t/e")},
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		for _, k := range []string{"ka", "kb", "kc"} {
			if err := InsertWorkspace(ctx, tx, Workspace{Key: k, HerdrSession: "default", HerdrWorkspaceID: "w-" + k,
				Label: k, Status: WorkspaceLive, LastSeen: at}); err != nil {
				return err
			}
		}
		for _, d := range ds {
			d.HerdrSession, d.PaneID, d.At = str("default"), "w1:p1", at
			if _, err := InsertDelivery(ctx, tx, d); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, key, agent string
		want             string // the transcript path; "" for none
	}{
		// The newer receipts of kb, kc and of an unmapped pane never show in ka (S48). A
		// "withheld: identity" receipt never counts (E15-S5).
		{"newest other session of the workspace", "ka", "x", "/t/a2"},
		{"a resumed session skips its own receipts; withheld: inconsistent counts", "ka", "a", "/t/b"},
		{"any harness and result", "ka", "c", "/t/a2"},
		{"withheld: forget pending counts", "kc", "x", "/t/g"},
		{"the only other receipt is withheld: identity", "kc", "g", ""},
		{"the only receipt is of the same session", "kb", "d", ""},
		{"a workspace with no receipt", "kd", "x", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Write(ctx, func(tx *sql.Tx) error {
				d, ok, err := LastSession(ctx, tx, tc.key, tc.agent)
				got := ""
				if ok {
					got = *d.TranscriptPath
				}
				if err != nil || got != tc.want {
					t.Errorf("LastSession(%s, %s) = %q, %v, want %q", tc.key, tc.agent, got, err, tc.want)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	err = s.Write(ctx, func(tx *sql.Tx) error {
		d, _, err := LastSession(ctx, tx, "ka", "x")
		if err != nil {
			return err
		}
		if d.Profile != "claude" || !d.At.Equal(at) {
			t.Errorf("LastSession(ka) = %s at %v, want claude at %v", d.Profile, d.At, at)
		}
		// One indexed query: the hook stays inside its time target (§8.3).
		var plan strings.Builder
		rows, err := tx.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT id, profile, at, transcript_path FROM deliveries
			WHERE workspace_key = ? AND transcript_path IS NOT NULL AND agent_session_id IS NOT ?
			AND NOT (result = ? AND withheld_reason IS 'identity')
			ORDER BY id DESC LIMIT 1`, "ka", "x", ResultWithheld)
		if err != nil {
			return err
		}
		defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				return err
			}
			plan.WriteString(detail + "\n")
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if !strings.Contains(plan.String(), "USING INDEX deliveries_transcript") || strings.Contains(plan.String(), "TEMP B-TREE") {
			t.Errorf("query plan of LastSession:\n%swant the index deliveries_transcript and no sort", plan.String())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// AC E15-S4: forget clears the transcript paths of one agent session in one binding, of any
// harness, and keeps its receipts.
func TestClearTranscriptPaths(t *testing.T) {
	_, s := newTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 2, 14, 5, 0, 0, time.UTC)
	str := func(v string) *string { return &v }
	ds := []Delivery{
		{Profile: "claude", HerdrSession: str("default"), AgentSessionID: str("a"), TranscriptPath: str("/t/a1")},
		{Profile: "claude", HerdrSession: str("default"), AgentSessionID: str("a"), TranscriptPath: str("/t/a2")},
		{Profile: "codex", HerdrSession: str("default"), AgentSessionID: str("a"), TranscriptPath: str("/t/codex")},
		{Profile: "claude", HerdrSession: str("other"), AgentSessionID: str("a"), TranscriptPath: str("/t/other")},
		{Profile: "claude", HerdrSession: str("default"), AgentSessionID: str("b"), TranscriptPath: str("/t/b")},
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		for _, d := range ds {
			d.PaneID, d.Result, d.At = "w1:p1", ResultUnmapped, at
			if _, err := InsertDelivery(ctx, tx, d); err != nil {
				return err
			}
		}
		if n, err := TranscriptPaths(ctx, tx, "default", "a"); err != nil || n != 3 {
			t.Errorf("TranscriptPaths(a) = %d, %v, want 3", n, err)
		}
		if err := ClearTranscriptPaths(ctx, tx, "default", "a"); err != nil {
			return err
		}
		if n, err := TranscriptPaths(ctx, tx, "default", "a"); err != nil || n != 0 {
			t.Errorf("after the clear TranscriptPaths(a) = %d, %v, want 0", n, err)
		}
		for id, want := range map[int64]string{1: "", 2: "", 3: "", 4: "/t/other", 5: "/t/b"} {
			d, err := DeliveryByID(ctx, tx, id)
			if err != nil {
				return err
			}
			got := ""
			if d.TranscriptPath != nil {
				got = *d.TranscriptPath
			}
			if got != want {
				t.Errorf("after the clear receipt %d has transcript path %q, want %q", id, got, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
