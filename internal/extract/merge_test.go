package extract

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// AC output (§9.5): checkMerge drops a group that fails its own check, says why with no text,
// and keeps the other groups. A result that does not match the schema is invalid.
func TestCheckMerge(t *testing.T) {
	items := []store.MergeItem{{ID: 1, Text: "use pnpm in this repo"}, {ID: 2, Text: "install with pnpm here"},
		{ID: 3, Text: "pnpm, not npm"}, {ID: 4, Text: "run the tests with go test"}}
	good := map[string]any{"item_ids": []int64{2, 1}, "keep": 1, "text": "use pnpm", "reason": "same rule"}
	with := func(k string, v any) map[string]any {
		g := map[string]any{}
		for gk, gv := range good {
			g[gk] = gv
		}
		g[k] = v
		return g
	}
	for name, tc := range map[string]struct {
		group    map[string]any
		rejected [][]int64
		why      string
	}{
		"one id":         {group: with("item_ids", []int64{1, 1}), why: "fewer than 2 items"},
		"unknown id":     {group: with("item_ids", []int64{1, 9}), why: "item 9 is not in the input"},
		"keep not in it": {group: with("keep", 3), why: "keep 3 is not an item of the group"},
		"empty text":     {group: with("text", "  "), why: "no text"},
		"four lines":     {group: with("text", "a\nb\nc\nd"), why: "more than 3 lines"},
		"not shorter":    {group: with("text", strings.Repeat("x", 43)), why: "not shorter than the old texts together"},
		"rejected":       {group: good, rejected: [][]int64{{1, 2}}, why: "you rejected the merge"},
	} {
		t.Run(name, func(t *testing.T) {
			other := map[string]any{"item_ids": []int64{3, 4}, "keep": 4, "text": "pnpm; go test", "reason": "r"}
			out, err := json.Marshal(map[string]any{"groups": []any{tc.group, other}})
			if err != nil {
				t.Fatal(err)
			}
			groups, dropped, err := checkMerge(out, items, tc.rejected)
			if err != nil || len(groups) != 1 || groups[0].keep != 4 || len(dropped) != 1 || !strings.Contains(dropped[0], tc.why) {
				t.Errorf("checkMerge = %+v, %q, %v; want the other group and %q", groups, dropped, err, tc.why)
			}
		})
	}
	t.Run("an id in two groups", func(t *testing.T) {
		out, err := json.Marshal(map[string]any{"groups": []any{good, with("item_ids", []int64{2, 3}),
			map[string]any{"item_ids": []int64{4, 3}, "keep": 4, "text": "x", "reason": "r"}}})
		if err != nil {
			t.Fatal(err)
		}
		groups, dropped, err := checkMerge(out, items, nil)
		if err != nil || len(groups) != 0 || len(dropped) != 3 {
			t.Errorf("checkMerge = %+v, %q, %v; want all 3 groups dropped", groups, dropped, err)
		}
	})
	for name, out := range map[string]string{
		"no groups":     `{}`,
		"unknown field": `{"groups": [], "x": 1}`,
		"missing field": `{"groups": [{"item_ids": [1, 2], "keep": 1, "text": "x"}]}`,
		"wrong type":    `{"groups": [{"item_ids": ["1"], "keep": 1, "text": "x", "reason": "r"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := checkMerge(json.RawMessage(out), items, nil); !errors.Is(err, ErrInvalid) {
				t.Errorf("checkMerge(%s) = %v, want ErrInvalid", out, err)
			}
		})
	}
}

// mergeFixture is a fixture with three items of ws-A that may merge.
func mergeFixture(t *testing.T) (*fixture, []int64) {
	t.Helper()
	f := newFixture(t)
	var ids []int64
	ws := "ws-A"
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		for i, text := range []string{"use pnpm in this repo", "install with pnpm here", "pnpm, not npm"} {
			id, _, err := store.CreateItem(ctx, tx, store.Item{ClaimID: "cl_m" + string(rune('a'+i)), Kind: store.KindFact, Created: t0},
				store.ItemRevision{Text: text, Scope: store.ScopeWorkspace, WorkspaceKey: &ws, State: store.StateActive,
					Author: store.AuthorYou, Origin: store.OriginReview, Created: t0})
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	return f, ids
}

// AC proposal and caps (§9.5, §7.3): the merge call has the merge schema and counts as one
// call; its proposal counts as one proposal. A call that the CLI refuses at a usage limit does
// not count (#377), and stores no hash, so the next pass calls again.
func TestMergeCaps(t *testing.T) {
	f, ids := mergeFixture(t)
	f.cli.reply = func(context.Context, string) ([]byte, error) {
		return claudeReply(map[string]any{"groups": []any{map[string]any{"item_ids": ids, "keep": ids[0],
			"text": "use pnpm", "reason": "same rule"}}})
	}
	part := MergePart{Key: "ws-A", SHA256: "h1"}
	if err := f.pass().Merge(context.Background(), part); err != nil {
		t.Fatal(err)
	}
	if argv := f.cli.cmds[0].Argv; !slices.Contains(argv, mergeSchema) || slices.Contains(argv, claimSchema) {
		t.Errorf("merge call argv %q, want the merge schema", argv)
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		d, err := f.pass().Day(ctx, tx)
		if err == nil && (d.Calls != 1 || d.Proposals != 1) {
			t.Errorf("day = %d calls, %d proposals; want 1 and 1", d.Calls, d.Proposals)
		}
		return err
	})

	f, _ = mergeFixture(t)
	f.cli.reply = func(context.Context, string) ([]byte, error) {
		return nil, &ExitError{Err: errors.New("exit status 1"), Stderr: []byte("ERROR: 429 Too Many Requests")}
	}
	for range 2 {
		if err := f.pass().Merge(context.Background(), part); !errors.Is(err, ErrLimit) {
			t.Fatalf("Merge at a limit = %v, want ErrLimit", err)
		}
	}
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		d, err := f.pass().Day(ctx, tx)
		if err == nil && (d.Calls != 0 || f.cli.calls() != 2) {
			t.Errorf("after 2 calls at a limit: %d counted, %d made; want 0 counted and 2 made", d.Calls, f.cli.calls())
		}
		return err
	})
}
