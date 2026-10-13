package brained_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/cli"
	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// merger is the fake extractor CLI of the merge pass: no model call. It counts its calls,
// checks that each call has the merge schema, and answers with groups.
type merger struct {
	t      *testing.T
	calls  int
	prompt string
	groups []map[string]any
}

func (m *merger) Run(_ context.Context, c extract.Command) ([]byte, error) {
	m.calls++
	m.prompt = c.Argv[len(c.Argv)-1]
	if i := slices.Index(c.Argv, "--json-schema"); i < 0 || !strings.Contains(c.Argv[i+1], `"item_ids"`) {
		m.t.Errorf("merge call argv %q has no merge schema", c.Argv)
	}
	groups := append([]map[string]any{}, m.groups...)
	return json.Marshal(map[string]any{"is_error": false, "structured_output": map[string]any{"groups": groups}})
}

// group is one group of a merge result.
func group(keep int64, text string, ids ...int64) map[string]any {
	return map[string]any{"item_ids": ids, "keep": keep, "text": text, "reason": "they all say to use pnpm"}
}

// dupes are the three items of the merge cases that say the same thing, and their claim keys.
var dupes = []struct {
	text string
	key  store.ClaimKey
}{
	{"Use pnpm, not npm, to install packages in this repo.", store.ClaimKey{Situation: "installing packages", Rule: "use pnpm"}},
	{"Install packages with pnpm; npm breaks the lockfile.", store.ClaimKey{Situation: "adding a dependency", Rule: "install with pnpm"}},
	{"In this repo, pnpm install is the way to add a package, npm is not.", store.ClaimKey{Situation: "new package", Rule: "pnpm install"}},
}

// merged is the merged text of dupes in the merge cases.
const merged = "Use pnpm, not npm, to install packages."

// mergeCase is the pipeline of the merge cases: workspace w1 (ws_1) has the three items of
// dupes, each with its claim, and fillers; the extractor CLI is claude, faked by m.
type mergeCase struct {
	pipeline
	ids    []int64 // the items of dupes
	claims []string
	m      *merger
	n      int // items made so far
}

// newMergeCase makes the merge case with fillers items besides dupes, and builds the output.
func newMergeCase(t *testing.T, fillers int) *mergeCase {
	t.Helper()
	c := &mergeCase{pipeline: pipeline{brained.NewPipeline(t)}, m: &merger{t: t}}
	c.tx(t, func(ctx context.Context, tx *sql.Tx) error {
		return store.SetExtractor(ctx, tx, store.ExtractorSetting{On: true, CLI: store.ExtractorClaude, Changed: pipelineNow})
	})
	for _, d := range dupes {
		id, claim := c.item(t, d.text, &d.key)
		c.ids, c.claims = append(c.ids, id), append(c.claims, claim)
	}
	for range fillers {
		c.filler(t)
	}
	c.build(t)
	return c
}

// item makes an active item of ws_1 with text and, when key is not nil, the claim of key; else
// the claim has no row, as the claim of brain remember. It returns the item and its claim.
func (c *mergeCase) item(t *testing.T, text string, key *store.ClaimKey) (id int64, claim string) {
	t.Helper()
	c.n++
	ws := "ws_1"
	c.tx(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		claim = fmt.Sprintf("cl_r%02d", c.n)
		if key != nil {
			if claim, err = store.ClaimID(ctx, tx, *key, pipelineNow); err != nil {
				return err
			}
		}
		id, _, err = store.CreateItem(ctx, tx, store.Item{ClaimID: claim, Kind: store.KindFact, Created: pipelineNow},
			store.ItemRevision{Text: text, Scope: store.ScopeWorkspace, WorkspaceKey: &ws, State: store.StateActive,
				Author: store.AuthorYou, Origin: store.OriginReview, Created: pipelineNow})
		return err
	})
	return id, claim
}

// filler makes an item of about 200 characters that says something of its own.
func (c *mergeCase) filler(t *testing.T) int64 {
	t.Helper()
	id, _ := c.item(t, fmt.Sprintf("Rule %02d of this repo: %s", c.n, strings.Repeat("keep it so ", 17)), nil)
	return id
}

// build builds the output of the store, as approve does.
func (c *mergeCase) build(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, c.Store(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	if _, err := output.Build(ctx, st, filepath.Join(c.Store(), "out"), nil, func() time.Time { return pipelineNow }); err != nil {
		t.Fatalf("Build() = %v", err)
	}
}

// size returns the size of the part of ws_1 in the current generation and its budget.
func (c *mergeCase) size(t *testing.T) (size, limit int) {
	t.Helper()
	ps, err := output.Parts(filepath.Join(c.Store(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Key == "ws_1" {
			size, limit = brief.WorkspaceSize(p.Text)
			return size, limit
		}
	}
	t.Fatal("no part of ws_1")
	return 0, 0
}

// pass runs the merge pass at pipelineNow.
func (c *mergeCase) pass(t *testing.T) {
	t.Helper()
	if err := c.MergePass(c.m, pipelineNow); err != nil {
		t.Fatalf("merge pass = %v", err)
	}
}

// merges returns the merge proposals with status s.
func (c *mergeCase) merges(t *testing.T, s store.ProposalStatus) []store.Proposal {
	t.Helper()
	var ps []store.Proposal
	c.tx(t, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM proposals WHERE status = ? AND merge != '[]' ORDER BY id`, s)
		if err != nil {
			return err
		}
		defer rows.Close() //nolint:errcheck // read only
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		for _, id := range ids {
			p, err := store.ProposalByID(ctx, tx, id)
			if err != nil {
				return err
			}
			ps = append(ps, p)
		}
		return rows.Err()
	})
	return ps
}

// one runs the pass and returns its one pending merge proposal.
func (c *mergeCase) one(t *testing.T) store.Proposal {
	t.Helper()
	c.pass(t)
	ps := c.merges(t, store.ProposalPending)
	if c.m.calls != 1 || len(ps) != 1 {
		t.Fatalf("%d calls, merge proposals %+v; want 1 call and 1 proposal", c.m.calls, ps)
	}
	return ps[0]
}

// out runs a brain command in pane w1:p1 and returns its output.
func (c *mergeCase) out(t *testing.T, cmd func(context.Context, cli.Env, []string) error, args ...string) string {
	t.Helper()
	e := c.env(nil)
	var b bytes.Buffer
	e.Stdout = &b
	if err := cmd(context.Background(), e, args); err != nil {
		t.Fatalf("brain %q = %v", args, err)
	}
	return b.String()
}

// texts returns the state and text of the current revision of each item of ids.
func (c *mergeCase) texts(t *testing.T, ids []int64) []string {
	t.Helper()
	var out []string
	c.tx(t, func(ctx context.Context, tx *sql.Tx) error {
		for _, id := range ids {
			it, err := store.ItemByID(ctx, tx, id)
			if err != nil {
				return err
			}
			r, err := store.ItemRevisionByKey(ctx, tx, id, it.CurrentRevision)
			if err != nil {
				return err
			}
			out = append(out, fmt.Sprintf("%s: %s", it.State, r.Text))
		}
		return nil
	})
	return out
}

// claimOf returns the claim id that key gets now.
func (c *mergeCase) claimOf(t *testing.T, key store.ClaimKey) string {
	t.Helper()
	var id string
	c.tx(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		id, err = store.ClaimID(ctx, tx, key, pipelineNow)
		return err
	})
	return id
}

// S96: a workspace part over 80% with three items that say the same thing gives one proposal on
// the kept item, with its base revisions, shown with the items it merges, each old text with its
// id and the characters saved. Approve leaves one active item and two retired items, and the part
// gets smaller. A later claim with the key of a retired item gets the claim of the kept item, which
// holds it in the workspace.
func TestS96MergePass(t *testing.T) {
	c := newMergeCase(t, 21)
	before, limit := c.size(t)
	if before*100 < limit*80 {
		t.Fatalf("part = %d of %d, want at least 80%%", before, limit)
	}
	c.m.groups = []map[string]any{group(c.ids[0], merged, c.ids...)}
	pr := c.one(t)
	for _, d := range dupes {
		if !strings.Contains(c.m.prompt, d.text) {
			t.Errorf("merge prompt has no %q", d.text)
		}
	}
	want := []store.RevisionRef{{Item: c.ids[0], Revision: 1}, {Item: c.ids[1], Revision: 1}, {Item: c.ids[2], Revision: 1}}
	if *pr.ItemID != c.ids[0] || *pr.BaseRevision != 1 || pr.Text != merged || !slices.Equal(pr.Merge, want) {
		t.Errorf("proposal = item %d revision %d text %q merge %v; want item %d revision 1 text %q merge %v",
			*pr.ItemID, *pr.BaseRevision, pr.Text, pr.Merge, c.ids[0], merged, want)
	}

	show := c.out(t, cli.Review, fmt.Sprint(pr.ID))
	saved := len(dupes[0].text) + len(dupes[1].text) + len(dupes[2].text) - len(merged)
	for _, w := range []string{fmt.Sprintf("merges items %d, %d and %d into item %d", c.ids[0], c.ids[1], c.ids[2], c.ids[0]),
		"text:     " + merged, fmt.Sprintf("- item %d: %s", c.ids[1], dupes[1].text), fmt.Sprintf("saves:    %d characters", saved)} {
		if !strings.Contains(show, w) {
			t.Errorf("brain review %d =\n%s\nwant %q", pr.ID, show, w)
		}
	}
	if list := c.out(t, cli.Review); !strings.Contains(list, fmt.Sprintf("merges items %d, %d and %d into item %d: %s",
		c.ids[0], c.ids[1], c.ids[2], c.ids[0], merged)) {
		t.Errorf("brain review =\n%s\nwant the merge line", list)
	}

	c.brain(t, cli.Review, fmt.Sprint(pr.ID), "--approve")
	wantTexts := []string{"active: " + merged, "retired: " + dupes[1].text, "retired: " + dupes[2].text}
	if got := c.texts(t, c.ids); !slices.Equal(got, wantTexts) {
		t.Errorf("items = %q, want %q", got, wantTexts)
	}
	if after, _ := c.size(t); after >= before {
		t.Errorf("part after the approval = %d, want less than %d", after, before)
	}
	for _, d := range dupes[1:] {
		if got := c.claimOf(t, d.key); got != c.claims[0] {
			t.Errorf("claim of %q = %s, want %s of the kept item", d.key.Rule, got, c.claims[0])
		}
	}
	later := c.claimOf(t, dupes[2].key)
	c.tx(t, func(ctx context.Context, tx *sql.Tx) error {
		held, err := store.ClaimHeld(ctx, tx, later, "ws_1")
		if err == nil && !held {
			t.Error("the claim of a retired item is not held by the kept item")
		}
		return err
	})
}

// S97: undo of the kept item on the merge revision restores the three items in one undo, and
// removes the links of the merge; a second undo does nothing. After a reject, the same group
// does not come back when the part changes.
func TestS97MergeUndoReject(t *testing.T) {
	t.Run("undo", func(t *testing.T) {
		c := newMergeCase(t, 21)
		c.m.groups = []map[string]any{group(c.ids[0], merged, c.ids...)}
		pr := c.one(t)
		c.brain(t, cli.Review, fmt.Sprint(pr.ID), "--approve")
		c.out(t, cli.Undo, fmt.Sprint(c.ids[0]))
		want := []string{"active: " + dupes[0].text, "active: " + dupes[1].text, "active: " + dupes[2].text}
		if got := c.texts(t, c.ids); !slices.Equal(got, want) {
			t.Errorf("items after undo = %q, want %q", got, want)
		}
		for i, d := range dupes {
			if got := c.claimOf(t, d.key); got != c.claims[i] {
				t.Errorf("claim of %q after undo = %s, want its own %s", d.key.Rule, got, c.claims[i])
			}
		}
		if out := c.out(t, cli.Undo, fmt.Sprint(c.ids[0])); !strings.Contains(out, "undone already; nothing to do") {
			t.Errorf("second undo = %q, want nothing to do", out)
		}
		if got := c.texts(t, c.ids); !slices.Equal(got, want) {
			t.Errorf("items after a second undo = %q, want %q", got, want)
		}
	})
	t.Run("reject", func(t *testing.T) {
		c := newMergeCase(t, 21)
		c.m.groups = []map[string]any{group(c.ids[0], merged, c.ids[2], c.ids[1], c.ids[0])}
		pr := c.one(t)
		c.brain(t, cli.Review, fmt.Sprint(pr.ID), "--reject")
		c.filler(t)
		c.build(t)
		c.pass(t)
		if c.m.calls != 2 {
			t.Fatalf("%d calls, want a second call for the changed part", c.m.calls)
		}
		if ps := c.merges(t, store.ProposalPending); len(ps) != 0 {
			t.Errorf("merge proposals after the reject = %+v, want none", ps)
		}
	})
}

// S98: no merge call for a part under 80%, an unchanged part, a part with a waiting merge
// proposal, learning off, extraction off for the workspace, or full caps. A group with an
// unknown id or a text of 4 lines is dropped, and the other groups stay.
func TestS98MergeNoCall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fillers int
		setup   func(t *testing.T, c *mergeCase)
		calls   int
	}{
		{name: "under 80%", fillers: 10},
		{name: "unchanged", fillers: 21, calls: 1, setup: func(t *testing.T, c *mergeCase) {
			c.pass(t) // no group: the hash of the part is stored
		}},
		{name: "waiting merge proposal", fillers: 21, calls: 1, setup: func(t *testing.T, c *mergeCase) {
			c.m.groups = []map[string]any{group(c.ids[0], merged, c.ids...)}
			c.one(t)
			c.filler(t)
			c.build(t)
		}},
		{name: "learning off", fillers: 21, setup: func(t *testing.T, c *mergeCase) { c.brain(t, cli.Learn, "off") }},
		{name: "extraction off", fillers: 21, setup: func(t *testing.T, c *mergeCase) {
			c.brain(t, cli.Extract, "off", "--workspace", "w1")
		}},
		{name: "proposal cap full", fillers: 21, setup: func(t *testing.T, c *mergeCase) {
			c.tx(t, func(ctx context.Context, tx *sql.Tx) error {
				for range extract.DefaultCaps().Proposals {
					if _, err := store.InsertProposal(ctx, tx, store.Proposal{ItemID: &c.ids[0], BaseRevision: new(int64), Text: "x",
						Scope: store.ScopeWorkspace, Merge: []store.RevisionRef{{Item: c.ids[0]}}, Status: store.ProposalRejected,
						Created: pipelineNow.Add(-time.Hour)}); err != nil {
						return err
					}
				}
				return nil
			})
		}},
		{name: "caps full", fillers: 21, setup: func(t *testing.T, c *mergeCase) {
			c.tx(t, func(ctx context.Context, tx *sql.Tx) error {
				for range extract.DefaultCaps().Calls {
					if _, err := store.RecordMergeCall(ctx, tx, "ws_2", pipelineNow.Add(-time.Hour)); err != nil {
						return err
					}
				}
				return nil
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newMergeCase(t, tc.fillers)
			if tc.setup != nil {
				tc.setup(t, c)
			}
			c.pass(t)
			if c.m.calls != tc.calls {
				t.Errorf("merge calls = %d, want %d", c.m.calls, tc.calls)
			}
		})
	}
	t.Run("dropped groups", func(t *testing.T) {
		c := newMergeCase(t, 21)
		a, b := c.filler(t), c.filler(t)
		c.build(t)
		c.m.groups = []map[string]any{
			group(c.ids[0], merged, c.ids[0], c.ids[1]),
			group(c.ids[2], merged, c.ids[2], 9999),
			group(a, "Keep it so.\nKeep it so.\nKeep it so.\nKeep it so.", a, b),
		}
		c.pass(t)
		ps := c.merges(t, store.ProposalPending)
		if len(ps) != 1 || *ps[0].ItemID != c.ids[0] || len(ps[0].Merge) != 2 {
			t.Errorf("merge proposals = %+v, want one of items %d and %d", ps, c.ids[0], c.ids[1])
		}
	})
}

// A forget of an item in a waiting merge proposal removes that proposal from the queue, and
// erases its text (§9.5 forget, §6.7).
func TestMergeForget(t *testing.T) {
	c := newMergeCase(t, 21)
	c.m.groups = []map[string]any{group(c.ids[0], merged, c.ids...)}
	pr := c.one(t)
	c.out(t, cli.Forget, fmt.Sprint(c.ids[1]))
	c.tx(t, func(ctx context.Context, tx *sql.Tx) error {
		p, err := store.ProposalByID(ctx, tx, pr.ID)
		if err == nil && (p.Status != store.ProposalSuperseded || p.Text != "" || p.Diff != "") {
			t.Errorf("merge proposal after the forget = %s, text %q, diff %q; want superseded with no text", p.Status, p.Text, p.Diff)
		}
		return err
	})
}

// AC approve (§9.5): approve writes the kept item pinned when an item of the group was pinned,
// and takes an edit. It needs --confirm when a claim of the group touches a permission. A changed
// base revision of any item fails with the existing message, and a merge takes no scope change.
func TestMergeApprove(t *testing.T) {
	review := func(c *mergeCase, args ...string) error {
		return cli.Review(context.Background(), c.env(nil), args)
	}
	t.Run("pinned and edited", func(t *testing.T) {
		c := newMergeCase(t, 21)
		c.brain(t, cli.Pin, fmt.Sprint(c.ids[2]))
		c.build(t)
		c.m.groups = []map[string]any{group(c.ids[0], merged, c.ids...)}
		pr := c.one(t)
		c.brain(t, cli.Review, fmt.Sprint(pr.ID), "--approve", "--edit", "Use pnpm.")
		c.tx(t, func(ctx context.Context, tx *sql.Tx) error {
			it, err := store.ItemByID(ctx, tx, c.ids[0])
			if err == nil && !it.Pinned {
				t.Error("the kept item is not pinned, want pinned as item 3 of the group was")
			}
			return err
		})
		if got := c.texts(t, c.ids[:1]); got[0] != "active: Use pnpm." {
			t.Errorf("kept item = %q, want the edit", got[0])
		}
	})
	t.Run("permission", func(t *testing.T) {
		c := newMergeCase(t, 21)
		c.tx(t, func(ctx context.Context, tx *sql.Tx) error {
			return store.MarkClaim(ctx, tx, c.claims[1], "fact", true)
		})
		c.m.groups = []map[string]any{group(c.ids[0], merged, c.ids...)}
		pr := c.one(t)
		if !pr.PermissionWarning {
			t.Error("merge proposal has no permission warning, want one for the claim of item 2")
		}
		if err := review(c, fmt.Sprint(pr.ID), "--approve"); err == nil || !strings.Contains(err.Error(), "--confirm") {
			t.Errorf("approve without --confirm = %v, want the confirm error", err)
		}
		c.brain(t, cli.Review, fmt.Sprint(pr.ID), "--approve", "--confirm")
	})
	t.Run("changed base and scope", func(t *testing.T) {
		c := newMergeCase(t, 21)
		c.m.groups = []map[string]any{group(c.ids[0], merged, c.ids...)}
		pr := c.one(t)
		if err := review(c, fmt.Sprint(pr.ID), "--approve", "--scope", "global"); err == nil || !strings.Contains(err.Error(), "without --scope") {
			t.Errorf("approve --scope global = %v, want a refusal", err)
		}
		c.brain(t, cli.Pin, fmt.Sprint(c.ids[1]))
		err := review(c, fmt.Sprint(pr.ID), "--approve")
		want := fmt.Sprintf("item %d changed after proposal %d was made", c.ids[1], pr.ID)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("approve after a change of item %d = %v, want %q", c.ids[1], err, want)
		}
		wantTexts := []string{"active: " + dupes[0].text, "active: " + dupes[1].text, "active: " + dupes[2].text}
		if got := c.texts(t, c.ids); !slices.Equal(got, wantTexts) {
			t.Errorf("items after the refused approve = %q, want %q", got, wantTexts)
		}
	})
}

// AC trigger (§9.5): the global part takes a merge call when it is at least 80% of its budget,
// with its own items only.
func TestMergeGlobalPart(t *testing.T) {
	c := newMergeCase(t, 0)
	var global []int64
	c.tx(t, func(ctx context.Context, tx *sql.Tx) error {
		for i := range 16 {
			id, _, err := store.CreateItem(ctx, tx, store.Item{ClaimID: fmt.Sprintf("cl_g%02d", i), Kind: store.KindFact, Created: pipelineNow},
				store.ItemRevision{Text: fmt.Sprintf("Global rule %02d: %s", i, strings.Repeat("keep it so ", 17)), Scope: store.ScopeGlobal,
					State: store.StateActive, Author: store.AuthorYou, Origin: store.OriginReview, Created: pipelineNow})
			if err != nil {
				return err
			}
			global = append(global, id)
		}
		return nil
	})
	c.build(t)
	c.pass(t)
	if c.m.calls != 1 || !strings.Contains(c.m.prompt, fmt.Sprintf("[%d] fact: Global rule", global[0])) || strings.Contains(c.m.prompt, dupes[0].text) {
		t.Errorf("%d calls, prompt %q; want one call with the global items only", c.m.calls, c.m.prompt)
	}
}
