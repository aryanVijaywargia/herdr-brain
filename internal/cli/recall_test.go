package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// recallItem adds an item with text in scope key ("" for global) in state, and returns its id.
// A draft gets a proposal with status p.
func recallItem(t *testing.T, f *fixture, key, text string, state store.State, p store.ProposalStatus) int64 {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var id int64
	write(t, s, func(tx *sql.Tx) (err error) {
		ctx := context.Background()
		r := store.ItemRevision{Text: text, Scope: store.ScopeGlobal, State: state, Author: store.AuthorYou,
			Origin: store.OriginRemember, Created: testNow}
		if key != "" {
			r.Scope, r.WorkspaceKey = store.ScopeWorkspace, &key
		}
		if id, _, err = store.CreateItem(ctx, tx, store.Item{ClaimID: fmt.Sprintf("cl_%s", text), Kind: store.KindFact,
			Created: testNow}, r); err != nil || state != store.StateDraft {
			return err
		}
		_, err = store.InsertProposal(ctx, tx, store.Proposal{ItemID: &id, Text: text, Scope: r.Scope, Target: "t",
			Diff: "+ " + text, Status: p, Created: testNow})
		return err
	})
	return id
}

// recall runs brain recall from pane paneID of workspace ws, and returns its output.
func (f *fixture) recall(t *testing.T, paneID, ws string, words ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, warn bytes.Buffer
	e := f.env(f.sock, paneID, ws, &out)
	e.Stderr = &warn
	err = Recall(context.Background(), e, words)
	return out.String(), warn.String(), err
}

// AC E20-S2 #266 1 and 2: items match by the words in their text, best match first; only the
// global items and the items of the pane's workspace show, with no draft, pending, rejected or
// retired item.
func TestRecallMatch(t *testing.T) {
	f := newFixture(t)
	add := func(key, text string, state store.State, p store.ProposalStatus) int64 {
		return recallItem(t, f, key, text, state, p)
	}
	one := add("", "Use pnpm for packages", store.StateActive, "")
	two := add("ws_a14", "run pnpm test before a push", store.StateActive, "")
	three := add("ws_a14", "Packaging lives in the vendor folder", store.StateActive, "")
	add("ws_a14", "docker builds are slow", store.StateActive, "")
	add("ws_a15", "pnpm packages of the blog", store.StateActive, "")            // another workspace
	add("ws_a14", "pnpm packages pending", store.StateDraft, store.ProposalPending) // pending
	add("ws_a14", "pnpm packages rejected", store.StateDraft, store.ProposalRejected)
	add("", "pnpm packages retired", store.StateRetired, "")

	out, warn, err := f.recall(t, "w14:p1", "w14", "PNPM", "pack")
	want := fmt.Sprintf("Brain items for \"pnpm pack\", best match first:\n- [%d] Use pnpm for packages\n"+
		"- [%d] run pnpm test before a push\n- [%d] Packaging lives in the vendor folder\n", one, two, three)
	if err != nil || out != want {
		t.Errorf("Recall(pnpm pack) = %q, %v, want %q", out, err, want)
	}
	// The fixture has no receipt for the pane, so nothing records the recall, and one line says so.
	if !strings.Contains(warn, "not recorded") || strings.Count(warn, "\n") != 1 {
		t.Errorf("Recall() warning = %q, want one line that the recall is not recorded", warn)
	}

	if out, _, err := f.recall(t, "w14:p1", "w14", "kubernetes"); err != nil || out != "no Brain item matches \"kubernetes\"\n" {
		t.Errorf("Recall(kubernetes) = %q, %v, want no match", out, err)
	}
	// A pane of an unmapped workspace gets the global part only, as its brief (§8.3 step 4).
	want = fmt.Sprintf("Brain items for \"pnpm\", best match first:\n- [%d] Use pnpm for packages\n", one)
	if out, _, err := f.recall(t, "w16:p1", "w16", "pnpm"); err != nil || out != want {
		t.Errorf("Recall(pnpm) in w16 = %q, %v, want %q", out, err, want)
	}
	if _, _, err := f.recall(t, "w14:p1", "w14", " ", "--"); err == nil || !strings.Contains(err.Error(), recallUsage) {
		t.Errorf("Recall(no words) = %v, want the usage", err)
	}
}

// AC E20-S2 #266 1: at most 10 items show; equal matches go in item id order.
func TestRecallCap(t *testing.T) {
	f := newFixture(t)
	var ids []int64
	for i := range 12 {
		ids = append(ids, recallItem(t, f, "ws_a14", fmt.Sprintf("lint rule %d", i), store.StateActive, ""))
	}
	best := recallItem(t, f, "ws_a14", "lint and format rule", store.StateActive, "")
	out, _, err := f.recall(t, "w14:p1", "w14", "lint", "format")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")[1:]
	if len(lines) != maxRecall {
		t.Fatalf("Recall() printed %d items, want %d:\n%s", len(lines), maxRecall, out)
	}
	want := append([]int64{best}, ids[:maxRecall-1]...)
	for i, l := range lines {
		if !strings.HasPrefix(l, fmt.Sprintf("- [%d] ", want[i])) {
			t.Errorf("item %d = %q, want item %d", i, l, want[i])
		}
	}
}

// AC E20-S2 #266 2: a recalled item has the same marks as in the brief: exception, conflict
// and dispute.
func TestRecallMarks(t *testing.T) {
	f := newFixture(t)
	global := recallItem(t, f, "", "use npm for scripts", store.StateActive, "")
	other := recallItem(t, f, "ws_a14", "use yarn for scripts", store.StateActive, "")
	s := openStore(t, f.store)
	var exc, disputed int64
	write(t, s, func(tx *sql.Tx) (err error) {
		ctx := context.Background()
		key, one := "ws_a14", int64(1)
		exc, _, err = store.CreateItem(ctx, tx, store.Item{ClaimID: "cl_exc", Kind: store.KindFact, Created: testNow},
			store.ItemRevision{Text: "use pnpm for scripts here", Scope: store.ScopeWorkspace, WorkspaceKey: &key,
				State: store.StateActive, Author: store.AuthorYou, Origin: store.OriginRemember, Created: testNow,
				ExceptionToItem: &global, ExceptionToRevision: &one, ConflictsWith: []int64{other}})
		if err != nil {
			return err
		}
		disputed, _, err = store.CreateItem(ctx, tx, store.Item{ClaimID: "cl_disp", Kind: store.KindFact, Created: testNow},
			store.ItemRevision{Text: "scripts live in package.json", Scope: store.ScopeWorkspace, WorkspaceKey: &key,
				State: store.StateActive, Author: store.AuthorYou, Origin: store.OriginRemember, Created: testNow})
		if err != nil {
			return err
		}
		_, err = store.Dispute(ctx, tx, store.Proposal{ItemID: &disputed, BaseRevision: &one, Text: "t",
			Scope: store.ScopeWorkspace, Target: key, Diff: "- package.json abc\n+ (deleted)",
			Dispute: []string{"package.json"}, Status: store.ProposalPending, Created: testNow})
		return err
	})
	closeStore(t, s)
	out, _, err := f.recall(t, "w14:p1", "w14", "scripts")
	for _, want := range []string{
		fmt.Sprintf("- [%d] use pnpm for scripts here (exception to global item %d) (conflicts with %d; ask the user)\n", exc, global, other),
		fmt.Sprintf("- [%d] scripts live in package.json (disputed: package.json deleted on 2026-09-28)\n", disputed),
	} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("Recall(scripts) = %q, %v, want the line %q", out, err, want)
		}
	}
}

// recalls reads the recalls rows as "delivery: item_revisions".
func (f *fixture) recalls(t *testing.T) []string {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var rs []string
	write(t, s, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(context.Background(), `SELECT delivery_id, item_revisions FROM recalls ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close() //nolint:errcheck // read-only
		for rows.Next() {
			var d int64
			var revs string
			if err := rows.Scan(&d, &revs); err != nil {
				return err
			}
			rs = append(rs, fmt.Sprintf("%d: %s", d, revs))
		}
		return rows.Err()
	})
	return rs
}

// AC E20-S2 #266 3: a recall records the item revisions that it printed on the newest receipt
// of the pane's agent session. When herdr shows another agent session in the pane than the
// receipts name, the recall is not recorded.
func TestRecallRecord(t *testing.T) {
	f := newFixture(t)
	item := recallItem(t, f, "ws_a14", "use pnpm", store.StateActive, "")
	s := openStore(t, f.store)
	var receipts []int64
	write(t, s, func(tx *sql.Tx) error {
		for _, d := range []struct{ pane, agent string }{{"w14:p1", "a"}, {"w14:p1", "a"}, {"w14:p2", "a"}} {
			session, agent := "default", d.agent
			id, err := store.InsertDelivery(context.Background(), tx, store.Delivery{Profile: "claude", HerdrSession: &session,
				PaneID: d.pane, AgentSessionID: &agent, ItemRevisions: []store.RevisionRef{}, Result: store.ResultEmitted, At: testNow})
			receipts = append(receipts, id)
			if err != nil {
				return err
			}
		}
		return nil
	})
	closeStore(t, s)
	p := f.panes["w14:p1"]
	p.AgentSession = &herdr.AgentSession{Agent: "claude", Kind: "id", Value: "a"}
	f.panes["w14:p1"] = p
	if _, warn, err := f.recall(t, "w14:p1", "w14", "pnpm"); err != nil || warn != "" {
		t.Fatalf("Recall() = %q, %v, want a recorded recall", warn, err)
	}
	refs, _ := json.Marshal([]store.RevisionRef{{Item: 1, Revision: 1}, {Item: item, Revision: 1}})
	// A recall with no match records nothing.
	if _, _, err := f.recall(t, "w14:p1", "w14", "nothing matches this"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.recall(t, "w14:p1", "w14", "pnpm", "live"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		fmt.Sprintf("%d: [{\"item\":%d,\"revision\":1}]", receipts[1], item),
		fmt.Sprintf("%d: %s", receipts[1], refs),
	}
	if got := f.recalls(t); !slices.Equal(got, want) {
		t.Errorf("recalls = %v, want %v", got, want)
	}

	p.AgentSession.Value = "b"
	if _, warn, err := f.recall(t, "w14:p1", "w14", "pnpm"); err != nil || !strings.Contains(warn, "not recorded") {
		t.Errorf("Recall() with another agent = %q, %v, want a not recorded warning", warn, err)
	}
	if got := f.recalls(t); len(got) != 2 {
		t.Errorf("recalls = %v, want no new row", got)
	}
}

// AC E20-S2 #266 5: outside herdr, brain recall reads nothing and fails with one line.
func TestRecallOutsideHerdr(t *testing.T) {
	f := newFixture(t)
	recallItem(t, f, "", "use pnpm", store.StateActive, "")
	var out bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &out)
	e.Environ = []string{"HOME=" + f.store}
	err := Recall(context.Background(), e, []string{"pnpm"})
	var ns *noStoreError
	if !errors.As(err, &ns) || strings.Contains(err.Error(), "\n") || out.Len() != 0 {
		t.Errorf("Recall() outside herdr = %q, %v, want one error line and no output", out.String(), err)
	}
}
