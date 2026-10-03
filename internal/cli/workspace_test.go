package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// addEntry adds the map entry key of herdr workspace id in herdr session session, with the
// project of w14, and its store row. A closed entry waits for a relink to ws_a14 (S20); with
// replacedBy, a remap replaced it (S83).
func (f *fixture) addEntry(t *testing.T, key, session, id string, closed bool, replacedBy ...string) {
	t.Helper()
	path := filepath.Join(f.store, "workspaces.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Version int              `json:"version"`
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	e := maps.Clone(m.Entries[0])
	e["key"], e["session"], e["workspace_id"], e["label"] = key, session, id, "old "+id
	status := store.WorkspaceLive
	if closed {
		e["closed"], status = true, store.WorkspaceClosed
	}
	for _, r := range replacedBy {
		e["replaced_by"] = r
	}
	m.Entries = append(m.Entries, e)
	writeJSON(t, path, m)
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		return store.InsertWorkspace(ctx, tx, store.Workspace{Key: key, HerdrSession: session, HerdrWorkspaceID: id,
			Status: status, LastSeen: testNow})
	})
}

// oldItem commits an active item of kind and claim with text in workspace key ws, and builds
// the output. It returns the item id.
func (f *fixture) oldItem(t *testing.T, ws string, kind store.Kind, claim, text string) int64 {
	t.Helper()
	var id int64
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		id, _, err = store.CreateItem(ctx, tx, store.Item{ClaimID: claim, Kind: kind, Created: testNow},
			store.ItemRevision{Text: text, Scope: store.ScopeWorkspace, WorkspaceKey: &ws, State: store.StateActive,
				Author: store.AuthorYou, Origin: store.OriginRemember, Created: testNow})
		return err
	})
	s := openStore(t, f.store)
	defer closeStore(t, s)
	if _, err := output.Build(context.Background(), s, filepath.Join(f.store, outDir), nil, now); err != nil {
		t.Fatalf("Build() = %v", err)
	}
	return id
}

// link runs `brain workspace link args` from pane in herdr workspace ws.
func (f *fixture) link(t *testing.T, pane, ws string, args ...string) (string, error) {
	t.Helper()
	if _, ok := f.panes[pane]; !ok {
		f.panes[pane] = herdr.Pane{ID: pane, WorkspaceID: ws, TabID: ws + ":t1"}
	}
	var out bytes.Buffer
	err := Workspace(context.Background(), f.env(f.sock, pane, ws, &out), append([]string{"link"}, args...))
	return out.String(), err
}

func (f *fixture) mustLink(t *testing.T, args ...string) string {
	t.Helper()
	out, err := f.link(t, "w14:p1", "w14", args...)
	if err != nil {
		t.Fatalf("link %q from w14 = %v", args, err)
	}
	return out
}

// keyOf is the workspace key of item id now: "" for a global item.
func (r rows) keyOf(t *testing.T, id int64) string {
	t.Helper()
	it, _ := r.item(t, id)
	if it.WorkspaceKey == nil {
		return ""
	}
	return *it.WorkspaceKey
}

// dump prints every row of tables, for a before and after compare.
func (f *fixture) dump(t *testing.T, tables ...string) string {
	t.Helper()
	var b strings.Builder
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		for _, tb := range tables {
			if err := dumpTable(ctx, tx, tb, &b); err != nil {
				return err
			}
		}
		return nil
	})
	return b.String()
}

func dumpTable(ctx context.Context, tx *sql.Tx, table string, b *strings.Builder) error {
	rows, err := tx.QueryContext(ctx, "SELECT * FROM "+table)
	if err != nil {
		return err
	}
	defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		fmt.Fprintf(b, "%s %v\n", table, vals)
	}
	return rows.Err()
}

// oldShop adds the closed key ws_a9 (S20) with an active fact, an active skill and a retired
// item, and an active item of ws_a14. It returns the ids of the fact, the skill, the retired
// item and the ws_a14 item.
func (f *fixture) oldShop(t *testing.T) (fact, skill, retired, own int64) {
	t.Helper()
	f.addEntry(t, "ws_a9", "default", "w9", true)
	fact = f.oldItem(t, "ws_a9", store.KindFact, "c-old", "old fact marker-old")
	skill = f.oldItem(t, "ws_a9", store.KindSkill, "c-skill", skillText("deploy", "old"))
	retired = f.oldItem(t, "ws_a9", store.KindFact, "c-retired", "retired marker")
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		cur, err := store.ItemRevisionByKey(ctx, tx, retired, 1)
		if err != nil {
			return err
		}
		cur.State = store.StateRetired
		_, err = store.WriteRevision(ctx, tx, 1, cur)
		return err
	})
	return fact, skill, retired, f.addActive(t, ptr("ws_a14"), "own item")
}

// AC S20, §9.4: the dry run lists what moves and writes nothing. The link moves the items of
// the closed old key, with a new revision each; a fresh hook in the new workspace gets the old
// items and skills; the items of the new key stay. A second link of the same pair moves
// nothing.
func TestWorkspaceLink(t *testing.T) {
	f := newFixture(t)
	fact, skill, retired, own := f.oldShop(t)
	before := f.rows(t)

	out := f.mustLink(t, "ws_a9", "ws_a14")
	for _, want := range []string{fmt.Sprintf("moves 3: items %d, %d, %d", fact, skill, retired), "nothing was written",
		"brain workspace link ws_a9 ws_a14 --confirm"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run = %q, want %q", out, want)
		}
	}
	if after := f.rows(t); after.newest != before.newest || after.keyOf(t, fact) != "ws_a9" {
		t.Fatalf("dry run wrote: newest generation %d, want %d; item %d key %q", after.newest, before.newest, fact, after.keyOf(t, fact))
	}

	if out := f.mustLink(t, "ws_a9", "ws_a14", "--confirm"); !strings.Contains(out, "linked ws_a9 to ws_a14: moved 3") {
		t.Errorf("link = %q, want the moved items", out)
	}
	r := f.rows(t)
	for _, id := range []int64{fact, skill, retired} {
		it, cur := r.item(t, id)
		if r.keyOf(t, id) != "ws_a14" || cur.Author != store.AuthorYou || it.CurrentRevision != before.revisionOf(t, id)+1 {
			t.Errorf("item %d after link: key %q, author %s, revision %d, want ws_a14, you, one new revision",
				id, r.keyOf(t, id), cur.Author, it.CurrentRevision)
		}
	}
	if it, _ := r.item(t, retired); it.State != store.StateRetired {
		t.Errorf("retired item after link: %s, want retired", it.State)
	}
	if r.keyOf(t, own) != "ws_a14" || r.revisionOf(t, own) != 1 {
		t.Errorf("own item after link: key %q, revision %d, want ws_a14 at 1", r.keyOf(t, own), r.revisionOf(t, own))
	}
	if b := f.brief(t); !strings.Contains(b, "marker-old") || !strings.Contains(b, "- deploy: ") || strings.Contains(b, "retired marker") {
		t.Errorf("brief of ws_a14 after link = %q, want the old fact and skill", b)
	}
	d := f.hookStart(t, "s1")
	for _, id := range []int64{fact, skill} {
		if !slices.Contains(d.ItemRevisions, store.RevisionRef{Item: id, Revision: 2}) {
			t.Errorf("hook receipt = %v, want item %d revision 2", d.ItemRevisions, id)
		}
	}

	newest := f.rows(t).newest
	if out := f.mustLink(t, "ws_a9", "ws_a14", "--confirm"); !strings.Contains(out, "no items to move; nothing was written") {
		t.Errorf("second link = %q, want nothing to move", out)
	}
	if got := f.rows(t).newest; got != newest {
		t.Errorf("second link: newest generation %d, want %d", got, newest)
	}
}

// AC §9.4 link undo: undo of one moved item from the new workspace sends it back to the old
// key, and the next hook no longer gets it. The item is out of the caller's view again.
func TestWorkspaceLinkUndo(t *testing.T) {
	f := newFixture(t)
	fact, _, _, _ := f.oldShop(t)
	f.mustLink(t, "ws_a9", "ws_a14", "--confirm")

	if out := f.mustUndo(t, fmt.Sprint(fact)); !strings.Contains(out, "back in workspace ws_a9") {
		t.Errorf("undo = %q, want the item back in ws_a9", out)
	}
	r := f.rows(t)
	if _, cur := r.item(t, fact); r.keyOf(t, fact) != "ws_a9" || cur.State != store.StateActive || cur.UndoesRevision == nil {
		t.Errorf("item %d after undo: key %q, state %s, want ws_a9, active, an undo revision", fact, r.keyOf(t, fact), cur.State)
	}
	if b := f.brief(t); strings.Contains(b, "marker-old") || !strings.Contains(b, "- deploy: ") {
		t.Errorf("brief of ws_a14 after undo = %q, want the skill only", b)
	}
	if d := f.hookStart(t, "s2"); slices.ContainsFunc(d.ItemRevisions, func(r store.RevisionRef) bool { return r.Item == fact }) {
		t.Errorf("hook receipt after undo = %v, want no item %d", d.ItemRevisions, fact)
	}
	if _, err := f.undo(t, fmt.Sprint(fact)); !errors.Is(err, errNotFound) {
		t.Errorf("second undo = %v, want errNotFound", err)
	}
}

// revisionOf is the current revision of item id.
func (r rows) revisionOf(t *testing.T, id int64) int64 {
	t.Helper()
	it, _ := r.item(t, id)
	return it.CurrentRevision
}

// AC S83, §9.4 link scope: a live old key, a call from a pane outside the new workspace, a key
// that is not in the relink list (unknown, the new key itself, or an entry of another herdr
// session) and bad arguments are refused, and nothing is written.
func TestWorkspaceLinkRefused(t *testing.T) {
	f := newFixture(t)
	f.addEntry(t, "ws_a9", "default", "w9", true)
	f.addEntry(t, "ws_t9", "t1", "w9", true) // closed, same project, another herdr session
	moved := f.oldItem(t, "ws_a9", store.KindFact, "c-old", "old fact")
	other := f.oldItem(t, "ws_t9", store.KindFact, "c-other", "other session fact")
	live := f.addActive(t, ptr("ws_a14"), "live item")
	cases := []struct {
		name, pane, ws string
		args           []string
		want           string
	}{
		// ws_a14 is in the relink list (same root as ws_a15), but herdr workspace w14 maps to it.
		{"live old key", "w15:p1", "w15", []string{"ws_a14", "ws_a15", "--confirm"}, "workspace ws_a14 is live: herdr workspace w14"},
		{"pane outside new", "w15:p1", "w15", []string{"ws_a9", "ws_a14", "--confirm"}, "from a pane in workspace ws_a14; this pane is in ws_a15"},
		{"unmapped pane", "w16:p1", "w16", []string{"ws_a9", "ws_a14", "--confirm"}, "from a pane in workspace ws_a14: workspace w16 is not mapped"},
		{"unknown key", "w14:p1", "w14", []string{"ws_nope", "ws_a14", "--confirm"}, "ws_nope is not in the relink list"},
		{"new key as old", "w15:p1", "w15", []string{"ws_a15", "ws_a15", "--confirm"}, "ws_a15 is not in the relink list"},
		{"other herdr session", "w14:p1", "w14", []string{"ws_t9", "ws_a14", "--confirm"}, "ws_t9 is not in the relink list"},
		{"one key", "w14:p1", "w14", []string{"ws_a9", "--confirm"}, "usage"},
		{"bad flag", "w14:p1", "w14", []string{"ws_a9", "ws_a14", "--force"}, "usage"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := f.rows(t)
			_, err := f.link(t, c.pane, c.ws, c.args...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("link %q from %s = %v, want %q", c.args, c.ws, err, c.want)
			}
			after := f.rows(t)
			if after.newest != before.newest || after.keyOf(t, moved) != "ws_a9" || after.keyOf(t, other) != "ws_t9" ||
				after.keyOf(t, live) != "ws_a14" {
				t.Errorf("refused link wrote: newest %d (want %d), keys %q %q %q", after.newest, before.newest,
					after.keyOf(t, moved), after.keyOf(t, other), after.keyOf(t, live))
			}
		})
	}
	if err := Workspace(context.Background(), f.env(f.sock, "w14:p1", "w14", &bytes.Buffer{}), []string{"move"}); err == nil ||
		!strings.Contains(err.Error(), "usage") {
		t.Errorf("Workspace(move) = %v, want the usage", err)
	}
}

// AC §9.4 what moves: with an open candidate, a rejection and a pending extraction in the old
// workspace, and a candidate of the same claim in the new one, only the items move. Their
// evidence keeps its source, so no old observation becomes evidence of the new key. The new
// key has an item of the claim now, so its candidate gets no duplicate proposal (§7.3). A
// pending proposal of a moved item ends as superseded; a draft stays with its proposal in old.
func TestWorkspaceLinkMovesOnlyItems(t *testing.T) {
	f := newFixture(t)
	f.addEntry(t, "ws_a9", "default", "w9", true)
	claim := f.claim(t)
	oldCand := f.correct(t, claim, "ws_a9", "old-1")
	f.correct(t, claim, "ws_a14", "new-1")
	item := f.oldItem(t, "ws_a9", store.KindPreference, claim, "use pnpm")
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		rejected, err := store.ClaimID(ctx, tx, store.ClaimKey{Situation: "testing", Rule: "use jest"}, testNow)
		if err != nil {
			return err
		}
		_, err = store.Reject(ctx, tx, store.Candidate{ClaimID: rejected, Scope: store.ScopeWorkspace, WorkspaceKey: "ws_a9"}, "no", testNow)
		return err
	})
	f.signalSession(t, "ws_a9", "old-agent") // ended, not extracted yet
	var promo int64
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		base := int64(1)
		promo, err = store.InsertProposal(ctx, tx, store.Proposal{ItemID: &item, BaseRevision: &base, Text: "use pnpm",
			Scope: store.ScopeGlobal, Target: "global", Diff: "scope", Status: store.ProposalPending, Created: testNow})
		return err
	})
	s := openStore(t, f.store)
	draft, err := saveDraft(context.Background(), s, draft{text: "old draft", kind: store.KindFact, scope: store.ScopeWorkspace,
		workspaceKey: ptr("ws_a9"), target: "workspace w9"}, testNow)
	closeStore(t, s)
	if err != nil {
		t.Fatal(err)
	}
	stays := []string{"candidates", "evidence_links", "rejections", "sessions", "segments", "observations", "outcomes",
		"prompt_index", "extraction_batches", "extraction_watermarks", "learning_settings", "extractor_settings", "claims"}
	before, proposals := f.dump(t, stays...), len(f.rows(t).proposals)

	out := f.mustLink(t, "ws_a9", "ws_a14")
	if !strings.Contains(out, fmt.Sprintf("moves 1: item %d", item)) || !strings.Contains(out, fmt.Sprintf("superseded: item %d", promo)) {
		t.Errorf("dry run = %q, want item %d and proposal %d", out, item, promo)
	}
	f.mustLink(t, "ws_a9", "ws_a14", "--confirm")

	if after := f.dump(t, stays...); after != before {
		t.Errorf("link changed rows that stay with ws_a9:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	r := f.rows(t)
	if len(r.proposals) != proposals {
		t.Errorf("proposals after link: %d, want %d (no new proposal)", len(r.proposals), proposals)
	}
	if p := r.proposal(t, promo); p.Status != store.ProposalSuperseded {
		t.Errorf("proposal %d of the moved item: %s, want superseded", promo, p.Status)
	}
	dp := r.proposal(t, draft)
	if dp.Status != store.ProposalPending || r.keyOf(t, *dp.ItemID) != "ws_a9" {
		t.Errorf("draft proposal %d: %s, key %q, want pending in ws_a9", draft, dp.Status, r.keyOf(t, *dp.ItemID))
	}
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		// The claim is known in both keys: the item is in ws_a14, and it was in ws_a9 (§7.3).
		for key, want := range map[string]bool{"ws_a14": true, "ws_a9": true, "ws_a15": false} {
			if has, err := store.ClaimHasItem(ctx, tx, claim, key); err != nil || has != want {
				t.Errorf("ClaimHasItem(%s) = %v, %v, want %v", key, has, err, want)
			}
		}
		es, err := store.CandidateExcerpts(ctx, tx, oldCand.ID)
		if err != nil || len(es) != 1 || es[0].WorkspaceKey != "ws_a9" {
			t.Errorf("evidence of the moved item = %+v, %v, want one excerpt of ws_a9", es, err)
		}
		return nil
	})
	if out := f.mustReview(t); strings.Contains(out, fmt.Sprint(promo)) && strings.Contains(out, "use pnpm") {
		t.Errorf("queue of ws_a14 = %q, want no proposal of the moved item", out)
	}
}

// AC §6.7: forget of a source session of the old workspace, from a pane in the new one, still
// finds the evidence of a moved item: the candidate of its claim keeps its links.
func TestWorkspaceLinkForgetSource(t *testing.T) {
	f := newFixture(t)
	f.addEntry(t, "ws_a9", "default", "w9", true)
	claim := f.claim(t)
	c := f.correct(t, claim, "ws_a9", "old-1")
	f.oldItem(t, "ws_a9", store.KindPreference, claim, "use pnpm")
	f.mustLink(t, "ws_a9", "ws_a14", "--confirm")

	out := f.mustForget(t, now, "--session", "old-1")
	if !strings.Contains(out, fmt.Sprintf("candidate %d:", c.ID)) {
		t.Errorf("forget --session old-1 = %q, want candidate %d", out, c.ID)
	}
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		if es, err := store.CandidateExcerpts(ctx, tx, c.ID); err != nil || len(es) != 0 {
			t.Errorf("evidence after forget = %+v, %v, want none", es, err)
		}
		return nil
	})
}

// AC §8.2: a skill of the old key whose name an active skill in the new key has refuses the
// link and asks for a new name, in the dry run too. No file changes.
func TestWorkspaceLinkSkillNameClash(t *testing.T) {
	f := newFixture(t)
	f.addEntry(t, "ws_a9", "default", "w9", true)
	old := f.oldItem(t, "ws_a9", store.KindSkill, "c-skill", skillText("deploy", "old"))
	f.approvedSkill(t, skillText("deploy", "new"))
	files, newest := fileTree(t, filepath.Dir(f.store)), f.rows(t).newest

	for _, args := range [][]string{{"ws_a9", "ws_a14"}, {"ws_a9", "ws_a14", "--confirm"}} {
		_, err := f.link(t, "w14:p1", "w14", args...)
		if !errors.Is(err, store.ErrSkillName) || !strings.Contains(err.Error(), "new name") {
			t.Errorf("link %q = %v, want ErrSkillName that asks for a new name", args, err)
		}
	}
	if after := fileTree(t, filepath.Dir(f.store)); !mapsEqualExceptDB(files, after) {
		t.Error("files changed after the refused link")
	}
	if r := f.rows(t); r.newest != newest || r.keyOf(t, old) != "ws_a9" {
		t.Errorf("after the refusal: newest %d, key %q, want %d and ws_a9", r.newest, r.keyOf(t, old), newest)
	}
}

// AC E4, §9.4 link undo: undo across workspaces stays refused unless the move was a link from
// an old key that is not live. Undo of a moved item from another workspace finds nothing. An
// item that came to ws_a15 from ws_a14 cannot go back: ws_a14 is in the relink list (same
// root as ws_a15), but it is live.
func TestWorkspaceLinkUndoScope(t *testing.T) {
	f := newFixture(t)
	f.addEntry(t, "ws_a9", "default", "w9", true)
	moved := f.oldItem(t, "ws_a9", store.KindFact, "c-old", "old fact")
	f.mustLink(t, "ws_a9", "ws_a14", "--confirm")
	cross := f.oldItem(t, "ws_a14", store.KindFact, "c-cross", "cross fact")
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		cur, err := store.ItemRevisionByKey(ctx, tx, cross, 1)
		if err != nil {
			return err
		}
		cur.WorkspaceKey = ptr("ws_a15")
		_, err = store.WriteRevision(ctx, tx, 1, cur)
		return err
	})
	f.panes["w15:p1"] = herdr.Pane{ID: "w15:p1", WorkspaceID: "w15", TabID: "w15:t1"}
	newest := f.rows(t).newest
	for _, id := range []int64{moved, cross} {
		var out bytes.Buffer
		if err := Undo(context.Background(), f.env(f.sock, "w15:p1", "w15", &out), []string{fmt.Sprint(id)}); !errors.Is(err, errNotFound) {
			t.Errorf("undo of item %d from w15 = %v, want errNotFound", id, err)
		}
	}
	if r := f.rows(t); r.keyOf(t, cross) != "ws_a15" || r.keyOf(t, moved) != "ws_a14" || r.newest != newest {
		t.Errorf("after the refused undos: keys %q %q, newest %d, want ws_a15, ws_a14, %d",
			r.keyOf(t, cross), r.keyOf(t, moved), r.newest, newest)
	}
}

// AC S83: herdr lost the state of the session, and a remap gave the workspace a new key. The
// replaced key waits for a relink, and the link moves its items.
func TestWorkspaceLinkReplaced(t *testing.T) {
	f := newFixture(t)
	f.addEntry(t, "ws_a8", "default", "w8", false, "ws_a14")
	id := f.oldItem(t, "ws_a8", store.KindFact, "c-old", "old fact marker-old")
	f.mustLink(t, "ws_a8", "ws_a14", "--confirm")
	if r := f.rows(t); r.keyOf(t, id) != "ws_a14" || !strings.Contains(f.brief(t), "marker-old") {
		t.Errorf("after link: key %q, brief %q, want ws_a14 and the old item", r.keyOf(t, id), f.brief(t))
	}
}

// Review fix: undo of a link move ends the pending proposals of the item, which no pane could
// decide in the closed old key. It works when the old key is no longer in the relink list, as
// long as it is not live: here its project changed, so no current entry shares its root.
func TestWorkspaceLinkUndoEndsProposals(t *testing.T) {
	f := newFixture(t)
	fact, _, _, _ := f.oldShop(t)
	f.mustLink(t, "ws_a9", "ws_a14", "--confirm")
	pid := f.mustPromote(t, fact)
	path := filepath.Join(f.store, "workspaces.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	entries, _ := m["entries"].([]any)
	last, _ := entries[len(entries)-1].(map[string]any)
	last["project"] = "/elsewhere"
	writeJSON(t, path, m)

	f.mustUndo(t, fmt.Sprint(fact))
	r := f.rows(t)
	if r.keyOf(t, fact) != "ws_a9" || r.proposal(t, pid).Status != store.ProposalSuperseded {
		t.Errorf("after undo: key %q, proposal %d %s, want ws_a9 and superseded", r.keyOf(t, fact), pid, r.proposal(t, pid).Status)
	}
}

// Review fix: a queued extraction of the old key runs after the link. Its evidence goes to the
// old candidate of the moved skill's claim, but the claim stays known in the old key: no draft
// and no pending proposal appear there (§7.3, §9.4).
func TestWorkspaceLinkQueuedExtraction(t *testing.T) {
	f := newFixture(t)
	f.setClaude(t)
	f.addEntry(t, "ws_a9", "default", "w9", true)
	var claim string
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		claim, err = store.ClaimID(ctx, tx, store.ClaimKey{Situation: "releasing", Rule: "follow the release steps"}, testNow)
		return err
	})
	skill := f.oldItem(t, "ws_a9", store.KindSkill, claim, skillText("release", "old"))
	f.mustLink(t, "ws_a9", "ws_a14", "--confirm")
	f.releaseSession(t, "ws_a9") // ended, not extracted yet
	proposals := len(f.rows(t).proposals)

	s := openStore(t, f.store)
	cli := &skillCLI{}
	err := extract.Pass{Store: s, Runner: cli, HerdrSession: "default", DefaultOn: true,
		Now: func() time.Time { return testNow.Add(time.Hour) }}.Run(context.Background())
	closeStore(t, s)
	if err != nil || cli.calls != 1 {
		t.Fatalf("Run() = %v with %d calls, want one call", err, cli.calls)
	}
	r := f.rows(t)
	if len(r.proposals) != proposals {
		t.Errorf("proposals after the old extraction: %d, want %d", len(r.proposals), proposals)
	}
	for _, it := range r.items {
		if it.ID != skill && it.ClaimID == claim {
			t.Errorf("item %d of the moved claim in %v (%s), want none", it.ID, *it.WorkspaceKey, it.State)
		}
	}
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		if c, err := store.CandidateOf(ctx, tx, claim, "ws_a9"); err != nil || c.Count == 0 {
			t.Errorf("old candidate = %+v, %v, want the new evidence", c, err)
		}
		return nil
	})
}
