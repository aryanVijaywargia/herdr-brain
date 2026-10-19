package store

import (
	"context"
	"database/sql"
	"testing"
)

// AC #428 (Sol round 2): migration 37 stores the refusal of a failed call as data, and maps the
// reasons of an older Brain once: the bare "claude: exit status 1" of a Brain before #428 is a
// sign-in refusal, the #377 limit text is a limit refusal, and any other reason or status is
// none. Later code reads only the column.
func TestRefusalMigration(t *testing.T) {
	dir := t.TempDir()
	e := &evidenceStore{Store: openTest(t, dir, migrationsUpTo(t, 36)), sessions: map[string]int64{}}
	e.observe(t, "claude", "a1", "ws-a", SenderUnconfirmed, testNow)
	sid := queryInt(t, e.Store, `SELECT MIN(id) FROM sessions`)
	cases := []struct {
		status BatchStatus
		reason string
		want   Refusal
	}{
		{BatchFailed, "claude: exit status 1", RefusalSignIn},
		{BatchFailed, "codex: exit status 127", RefusalSignIn},
		{BatchFailed, "claude is at a usage or rate limit", RefusalLimit},
		{BatchFailed, "codex is at a usage or rate limit", RefusalLimit},
		{BatchFailed, "claude: exit status 1: API Error: Request timed out", RefusalNone},
		{BatchFailed, "claude: exit status 1000", RefusalNone},
		{BatchFailed, "claude reported an error", RefusalNone},
		{BatchInvalid, "claude: exit status 1", RefusalNone},
	}
	ctx := context.Background()
	if err := e.Write(ctx, func(tx *sql.Tx) error {
		for i, c := range cases {
			if _, err := tx.ExecContext(ctx, `INSERT INTO extraction_batches (session_id, workspace_key, first_capture,
				last_capture, start_watermark, last_observation, status, model_calls, reason, at)
				VALUES (?, 'ws-a', ?, ?, 0, 0, ?, 3, ?, ?)`, sid, i, i, c.status, c.reason, timeArg(testNow)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO change_requests (source, source_id, revision, workspace_key,
				status, model_calls, reason, created, at) VALUES ('proposal', ?, 1, 'ws-a', ?, 3, ?, ?, ?)`,
				i+1, c.status, c.reason, timeArg(testNow), timeArg(testNow)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	closeTest(t, e.Store)
	s := openTest(t, dir, migrationsUpTo(t, 37))
	defer closeTest(t, s)
	var bs []Batch
	var rs []ChangeRequest
	if err := s.Write(ctx, func(tx *sql.Tx) (err error) {
		if bs, err = Batches(ctx, tx); err != nil {
			return err
		}
		rs, err = ChangeRequests(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(bs) != len(cases) || len(rs) != len(cases) {
		t.Fatalf("batches %d, requests %d, want %d each", len(bs), len(rs), len(cases))
	}
	for i, c := range cases {
		if bs[i].Refusal != c.want || rs[i].Refusal != c.want {
			t.Errorf("%s %q: batch refusal %q, request refusal %q, want %q", c.status, c.reason, bs[i].Refusal,
				rs[i].Refusal, c.want)
		}
	}
}
