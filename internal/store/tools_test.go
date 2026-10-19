package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/tool"
)

func toolText(name string) string {
	return "name = \"" + name + "\"\ndescription = \"Run the tests of one package\"\ncwd = \"~/src/shop\"\n" +
		"timeout_s = 600\neffects = \"runs tests\"\nread_only = true\n" +
		"steps = [[\"pnpm\", \"install\"], [\"pnpm\", \"test\", \"--filter\", \"{package}\"]]\n\n" +
		"[inputs.package]\ntype = \"string\"\npattern = \"^[a-z]+$\"\n"
}

// toolStore opens a store with the workspaces k1 and k2, and returns it with a function that
// adds a tool item.
func toolStore(t *testing.T) (*Store, func(key *string, state State, text string) (int64, error)) {
	t.Helper()
	s := openM1(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	write(t, s, func(tx *sql.Tx) error {
		for _, k := range []string{"k1", "k2"} {
			if err := InsertWorkspace(ctx, tx, Workspace{Key: k, HerdrSession: "default", HerdrWorkspaceID: k,
				Label: k, Status: WorkspaceLive, LastSeen: now}); err != nil {
				return err
			}
		}
		return nil
	})
	add := func(key *string, state State, text string) (int64, error) {
		r := ItemRevision{Text: text, Scope: ScopeGlobal, WorkspaceKey: key, State: state, Author: AuthorYou,
			Origin: OriginReview, Created: now}
		if key != nil {
			r.Scope = ScopeWorkspace
		}
		var id int64
		err := s.Write(ctx, func(tx *sql.Tx) (err error) {
			id, _, err = CreateItem(ctx, tx, Item{ClaimID: text, Kind: KindTool, Created: now}, r)
			return err
		})
		return id, err
	}
	return s, add
}

func readTool(t *testing.T, s *Store, id int64) Tool {
	t.Helper()
	var tl Tool
	write(t, s, func(tx *sql.Tx) (err error) {
		tl, err = ToolByID(context.Background(), tx, id)
		return err
	})
	return tl
}

// A tool item has a tools row with its contract, the contract hash and the approval of an
// active revision.
func TestToolRow(t *testing.T) {
	s, add := toolStore(t)
	k1 := "k1"
	id, err := add(&k1, StateActive, toolText("test-package"))
	if err != nil {
		t.Fatal(err)
	}
	tl := readTool(t, s, id)
	hash := tool.Hash(toolText("test-package"), "workspace", "k1")
	if tl.Name != "test-package" || tl.Contract != toolText("test-package") || tl.ContractHash != hash ||
		tl.Cwd != "~/src/shop" || tl.TimeoutS != 600 || tl.Effects != "runs tests" || !tl.ReadOnly ||
		tl.ApprovedHash == nil || *tl.ApprovedHash != hash || tl.Approved.IsZero() {
		t.Errorf("tool = %+v", tl)
	}

	draft, err := add(&k1, StateDraft, toolText("lint"))
	if err != nil {
		t.Fatal(err)
	}
	if tl := readTool(t, s, draft); tl.ApprovedHash != nil || !tl.Approved.IsZero() {
		t.Errorf("draft tool approved: %+v", tl)
	}

	// A retire clears the approval.
	ctx := context.Background()
	write(t, s, func(tx *sql.Tx) error {
		cur, err := ItemRevisionByKey(ctx, tx, id, 1)
		if err != nil {
			return err
		}
		cur.State = StateRetired
		_, err = WriteRevision(ctx, tx, 1, cur)
		return err
	})
	if tl := readTool(t, s, id); tl.ApprovedHash != nil {
		t.Errorf("retired tool approved: %+v", tl)
	}
}

// AC: a refused contract is never written, also for an input in a later step.
func TestToolWriteRefusesContract(t *testing.T) {
	s, add := toolStore(t)
	bad := strings.Replace(toolText("test-package"), `"--filter", "{package}"`, `"--filter={package}"`, 1)
	if _, err := add(nil, StateActive, bad); err == nil || !strings.Contains(err.Error(), "step 2") {
		t.Fatalf("CreateItem(bad contract) = %v, want a step 2 refusal", err)
	}
	var n int
	write(t, s, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT (SELECT COUNT(*) FROM items) + (SELECT COUNT(*) FROM tools)`).Scan(&n)
	})
	if n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}
}

// One active tool for each name in each scope: the same name in another scope, or in a
// draft, passes.
func TestToolName(t *testing.T) {
	_, add := toolStore(t)
	k1, k2 := "k1", "k2"
	for _, key := range []*string{&k1, &k2, nil} {
		if _, err := add(key, StateActive, toolText("deploy")); err != nil {
			t.Fatalf("add(%v) = %v", key, err)
		}
	}
	if _, err := add(&k1, StateDraft, toolText("deploy")); err != nil {
		t.Errorf("add(draft) = %v", err)
	}
	if _, err := add(&k1, StateActive, toolText("deploy")); !errors.Is(err, ErrToolName) {
		t.Errorf("add(clash) = %v, want ErrToolName", err)
	}
	if _, err := add(nil, StateActive, toolText("deploy")); !errors.Is(err, ErrToolName) {
		t.Errorf("add(global clash) = %v, want ErrToolName", err)
	}
}

// The contract joins the item text fields: forget clears it from the tools row.
func TestForgetTool(t *testing.T) {
	s, add := toolStore(t)
	id, err := add(nil, StateActive, toolText("deploy"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	write(t, s, func(tx *sql.Tx) error { return EraseItem(ctx, tx, id, time.Now()) })
	var left []Field
	write(t, s, func(tx *sql.Tx) (err error) {
		left, err = ItemTextLeft(ctx, tx, id)
		return err
	})
	if len(left) != 0 {
		t.Errorf("text left = %v", left)
	}
	if tl := readTool(t, s, id); tl.Contract != "" || tl.Name != "" || tl.Cwd != "" || tl.Effects != "" {
		t.Errorf("tool after forget = %+v", tl)
	}
}

// Tools lists every tools row by approval time, then id, with the unapproved ones last. Ready
// holds only for an active tool whose contract hash is the approved one (E17-S5).
func TestTools(t *testing.T) {
	s, add := toolStore(t)
	k1 := "k1"
	first, err := add(&k1, StateActive, toolText("first"))
	if err != nil {
		t.Fatal(err)
	}
	draft, err := add(&k1, StateDraft, toolText("draft"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := add(nil, StateActive, toolText("second"))
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := add(nil, StateActive, toolText("stopped"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	write(t, s, func(tx *sql.Tx) error {
		// A new approval of first, a day later, moves it to the end of the approved tools.
		_, err := WriteRevision(ctx, tx, 1, ItemRevision{ItemID: first, Text: toolText("first"), Scope: ScopeWorkspace,
			WorkspaceKey: &k1, State: StateActive, Author: AuthorYou, Origin: OriginReview,
			Created: time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE tools SET approved_hash = 'other' WHERE id = ?`, stopped)
		return err
	})
	var ts []Tool
	write(t, s, func(tx *sql.Tx) (err error) {
		ts, err = Tools(ctx, tx)
		return err
	})
	var ids []int64
	var ready []bool
	for _, tl := range ts {
		ids = append(ids, tl.ID)
		ready = append(ready, tl.Ready())
	}
	if want := []int64{second, stopped, first, draft}; !slices.Equal(ids, want) {
		t.Errorf("Tools() ids = %v, want %v", ids, want)
	}
	if want := []bool{true, false, true, false}; !slices.Equal(ready, want) {
		t.Errorf("Tools() ready = %v, want %v", ready, want)
	}
	if ts[3].State != StateDraft {
		t.Errorf("Tools()[3].State = %s, want draft", ts[3].State)
	}
}

// ToolTexts gives the tools that a pane of a workspace sees, active or pending: the current
// text of each active or disputed tool item and the text of each pending proposal of a tool
// item, global or of that workspace. A pending promotion of a tool of another workspace is
// global. A retired tool, a draft with no pending proposal, a decided proposal and a tool of
// another workspace are not in it (#337, #339).
func TestToolTexts(t *testing.T) {
	s, add := toolStore(t)
	k1, k2 := "k1", "k2"
	ids := map[string]int64{}
	for _, it := range []struct {
		name  string
		key   *string
		state State
	}{{"global", nil, StateActive}, {"active", &k1, StateActive}, {"disputed", &k1, StateDisputed},
		{"retired", &k1, StateRetired}, {"other", &k2, StateActive}, {"pending", &k1, StateDraft},
		{"rejected", &k1, StateDraft}, {"quiet", &k1, StateDraft}, {"promoted", &k2, StateActive}} {
		id, err := add(it.key, it.state, toolText(it.name))
		if err != nil {
			t.Fatal(err)
		}
		ids[it.name] = id
	}
	ctx := context.Background()
	write(t, s, func(tx *sql.Tx) error {
		for name, p := range map[string]Proposal{"pending": {Status: ProposalPending, Scope: ScopeWorkspace},
			"rejected": {Status: ProposalRejected, Scope: ScopeWorkspace}, "active": {Status: ProposalPending, Scope: ScopeWorkspace},
			"promoted": {Status: ProposalPending, Scope: ScopeGlobal}} {
			id, base := ids[name], int64(1)
			p.ItemID, p.BaseRevision, p.Text, p.Target, p.Created = &id, &base, toolText(name+"-next"), "k1", time.Now()
			if _, err := InsertProposal(ctx, tx, p); err != nil {
				return err
			}
		}
		return nil
	})
	names := func(key string) []string {
		t.Helper()
		var got []string
		write(t, s, func(tx *sql.Tx) error {
			ts, err := ToolTexts(ctx, tx, key)
			for _, tt := range ts {
				c, err := tool.Parse(tt.Text)
				if err != nil || tt.ClaimID != toolText(strings.TrimSuffix(c.Name, "-next")) {
					t.Errorf("tool text %+v: %v; want a tool text with the claim of its item", tt, err)
				}
				got = append(got, c.Name)
			}
			return err
		})
		slices.Sort(got)
		return got
	}
	if got, want := names("k1"), []string{"active", "active-next", "disputed", "global", "pending-next", "promoted-next"}; !slices.Equal(got, want) {
		t.Errorf("ToolTexts(k1) = %q, want %q", got, want)
	}
	if got, want := names(""), []string{"global", "promoted-next"}; !slices.Equal(got, want) {
		t.Errorf("ToolTexts(global) = %q, want %q", got, want)
	}
}
