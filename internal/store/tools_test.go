package store

import (
	"context"
	"database/sql"
	"errors"
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

// One active tool for each name in each scope (§10.1): the same name in another scope, or in a
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
