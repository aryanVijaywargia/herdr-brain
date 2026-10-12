package extract

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// textIn returns each table.column of the store that holds text, in a search of every column.
func (f *fixture) textIn(text string) []string {
	f.t.Helper()
	var found []string
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT m.name, c.name FROM sqlite_master m, pragma_table_info(m.name) c
			WHERE m.type = 'table'`)
		if err != nil {
			return err
		}
		var fields [][2]string
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
			if err := tx.QueryRowContext(ctx, q, text).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				found = append(found, tc[0]+"."+tc[1])
			}
		}
		return nil
	})
	return found
}

// forget erases item as brain forget does in the store (§6.7), and checks that ItemTextLeft
// finds nothing and that no store column holds text.
func (f *fixture) forget(item int64, text string) {
	f.t.Helper()
	if found := f.textIn(text); len(found) == 0 {
		f.t.Fatalf("no copy of %q before the forget", text)
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		if err := store.EraseItem(ctx, tx, item, t0.Add(24*time.Hour)); err != nil {
			return err
		}
		if left, err := store.ItemTextLeft(ctx, tx, item); err != nil || len(left) != 0 {
			f.t.Errorf("ItemTextLeft = %v, %v, want none", left, err)
		}
		return nil
	})
	if found := f.textIn(text); len(found) > 0 {
		f.t.Errorf("after the forget %q is in %v, want nowhere", text, found)
	}
}

// AC 1, 3 and 4 (#327), the restatement path of #312: the extractor restates a brain remember
// item with same_as to its claim, and its claim gets no candidate. Forget of the item removes
// the key text of that claim too.
func TestForgetRestatedRememberItem(t *testing.T) {
	f := newFixture(t)
	a := "ws-A"
	claim, item := f.remembered(&a, logRule)
	f.cli.reply = ruleReply(c2Situation, logRule,
		regexp.MustCompile(`\[(cl_[0-9a-f]+)\] rule: `+regexp.QuoteMeta(logRule)))
	f.session("c2", true, line{"ws-A", logText, t0.Add(20 * time.Minute)})
	f.mustRun()
	if n := f.count(`SELECT COUNT(*) FROM claims WHERE same_as = '` + claim + `' AND rule IS NOT NULL`); n != 1 {
		t.Fatalf("linked claims with a key = %d, want the restatement", n)
	}
	f.forget(item, normal(logRule))
}

// AC 1, 3 and 4 (#327): the restatement of an approved item and the merge of #279 into the
// candidate of an item put the evidence of the linked claim on the candidate of the item.
// Forget of the item removes the key text of the linked claim too.
func TestForgetMergedClaim(t *testing.T) {
	t.Run("restatement", func(t *testing.T) {
		f := newFixture(t)
		f.item3(true)
		f.cli.reply = ruleReply(item3Situation, proposal9Rule, knownItem3)
		f.session("s9", true, line{"ws-A", item3Prompt + " again", t0.Add(time.Hour)})
		f.mustRun()
		if n := f.count(`SELECT COUNT(*) FROM evidence_links WHERE via_claim IS NOT NULL`); n != 1 {
			t.Fatalf("merged evidence = %d, want 1", n)
		}
		f.forget(f.onlyItem(), normal(proposal9Rule))
	})
	t.Run("merge", func(t *testing.T) {
		f := newFixture(t)
		f.cli.reply = logReply(c1Situation, false)
		f.harnessSession("claude", "c1", true, line{"ws-A", logText, t0})
		f.mustRun()
		f.cli.reply = logReply(c2Situation, true)
		f.harnessSession("codex", "c2", true, line{"ws-A", logText, t0.Add(time.Minute)})
		f.mustRun()
		ps := f.proposals()
		if len(ps) != 1 {
			t.Fatalf("proposals = %+v, want the merged one", ps)
		}
		f.write(func(ctx context.Context, tx *sql.Tx) error {
			return store.DecideProposal(ctx, tx, ps[0].ID, store.ProposalApproved, store.SurfaceCLI, t0)
		})
		f.forget(f.onlyItem(), normal(c2Situation))
	})
}

// onlyItem returns the id of the one item of the store.
func (f *fixture) onlyItem() int64 {
	f.t.Helper()
	if n := f.count(`SELECT COUNT(*) FROM items`); n != 1 {
		f.t.Fatalf("items = %d, want 1", n)
	}
	return int64(f.count(`SELECT id FROM items`))
}

// AC 2 (#389, run 7 R7-2): forget of a brain remember item also stops the evidence of the
// extractor claim with same_as to its claim: the tombstone holds its source keys, and its key
// goes, so the same telling after the forget starts a new candidate at count 1, not count 2
// with the old evidence. The run 7 shape: the claim of the telling has its own candidate, as the
// permission flag keeps the link for review.
func TestForgetLinkedEvidence(t *testing.T) {
	f := newFixture(t)
	a := "ws-A"
	claim, item := f.remembered(&a, logRule)
	known := regexp.MustCompile(`\[(cl_[0-9a-f]+)\] rule: ` + regexp.QuoteMeta(logRule))
	reply := func(_ context.Context, prompt string) ([]byte, error) {
		var id any
		if m := known.FindStringSubmatch(prompt); m != nil {
			id = m[1]
		}
		return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"situation": c2Situation,
			"rule": "Log with src/log.js; never use console.log.", "touches_permission": true,
			"evidence_refs": promptRefs(prompt), "same_as": id})}})
	}
	f.cli.reply = reply
	f.session("c2", true, line{"ws-A", logText, t0.Add(20 * time.Minute)})
	f.mustRun()
	if n := f.count(`SELECT COUNT(*) FROM evidence_links l JOIN candidates c ON c.id = l.candidate_id
		JOIN claims k ON k.id = c.claim_id WHERE k.same_as = '` + claim + `'`); n != 1 {
		t.Fatalf("evidence of the linked claim = %d, want 1: the case needs the run 7 shape", n)
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		keys, err := store.ClaimSourceKeys(ctx, tx, claim)
		if err != nil {
			return err
		}
		if !slices.Equal(keys, []string{"c2-0"}) {
			t.Errorf("source keys of the forgotten item = %v, want the evidence of the linked claim", keys)
		}
		if err := store.EraseItem(ctx, tx, item, t0.Add(time.Hour)); err != nil {
			return err
		}
		return store.InsertTombstone(ctx, tx, store.Tombstone{ClaimID: claim, SourceKeys: keys, ForgottenAt: t0.Add(time.Hour)})
	})
	f.session("c3", true, line{"ws-A", logText, t0.Add(2 * time.Hour)})
	f.mustRun()
	if n := f.count(`SELECT COUNT(*) FROM candidates c JOIN evidence_links l ON l.candidate_id = c.id
		WHERE c.count = 1 AND l.capture_identity = 'c3-0'`); n != 1 {
		t.Errorf("candidates of the new telling at count 1 = %d, want 1", n)
	}
	if n := f.count(`SELECT COALESCE(MAX(count), 0) FROM candidates`); n >= 2 {
		t.Errorf("candidate count = %d after the forget, want below 2: the old evidence counts again", n)
	}
}
