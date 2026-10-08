package cli

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/resolver"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// popupEnviron is the env of a popup that herdr opens from an action with paneID of workspace
// ws focused: no HERDR_PANE_ID and no HERDR_WORKSPACE_ID, the caller in the context JSON (M0
// E5-S3 popup facts).
func (f *fixture) popupEnviron(paneID, ws string) []string {
	return []string{"HERDR_ENV=1", "HERDR_SOCKET_PATH=" + f.sock, "HERDR_SESSION=default", "HERDR_PLUGIN_ENTRYPOINT_ID=recall",
		fmt.Sprintf(`HERDR_PLUGIN_CONTEXT_JSON={"workspace_id":%q,"workspace_label":"shop","tab_id":"%s:t1","focused_pane_id":%q,"invocation_source":"api"}`,
			ws, ws, paneID)}
}

// popup runs the recall popup with environ and the keys, in a screen of rows and cols, and
// returns its last screen.
func (f *fixture) popup(t *testing.T, environ []string, keys string, rows, cols int) (string, error) {
	t.Helper()
	// The popup does not sync the workspace map (it writes nothing): an earlier brain call
	// gives the map its herdr labels, as in real use.
	if _, err := f.review(t); err != nil {
		t.Fatal(err)
	}
	return f.popupOnly(t, environ, keys, rows, cols)
}

// popupOnly is popup with no brain call before it.
func (f *fixture) popupOnly(t *testing.T, environ []string, keys string, rows, cols int) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e := f.env(f.sock, "", "", &out)
	e.Environ = environ
	err := recallPopup(context.Background(), e, bufio.NewReader(strings.NewReader(keys)), func() (int, int) { return rows, cols })
	screen := out.String()
	return screen[strings.LastIndex(screen, clearScreen)+len(clearScreen):], err
}

var listedID = regexp.MustCompile(`(?m)^(?:> |  )\[(\d+)\] `)

// listed returns the item ids that a search screen lists, in order.
func listed(screen string) []string {
	var ids []string
	for _, m := range listedID.FindAllStringSubmatch(screen, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// printed returns the item ids that brain recall printed, in order.
func printed(out string) []string {
	var ids []string
	for _, m := range regexp.MustCompile(`(?m)^- \[(\d+)\] `).FindAllStringSubmatch(out, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// AC 1 and 2 (E20-S3): the popup takes the focused pane of the action context, and lists the
// same items as brain recall from that pane, in the same order, each with its scope.
func TestRecallPopupSearch(t *testing.T) {
	f := newFixture(t)
	add := func(key, text string, state store.State, p store.ProposalStatus) int64 {
		return recallItem(t, f, key, text, state, p)
	}
	global := add("", "Use pnpm for packages", store.StateActive, "")
	shop := add("ws_a14", "run pnpm test before a push", store.StateActive, "")
	add("ws_a14", "Packaging lives in the vendor folder", store.StateActive, "")
	blog := add("ws_a15", "pnpm packages of the blog", store.StateActive, "")
	add("ws_a14", "pnpm packages pending", store.StateDraft, store.ProposalPending)
	add("", "pnpm packages retired", store.StateRetired, "")
	f.panes["w15:p1"] = herdr.Pane{ID: "w15:p1", WorkspaceID: "w15", TabID: "w15:t1"}

	cases := []struct {
		name, pane, ws, words string
		want                  []string // lines that the screen holds
		not                   []string
	}{
		{name: "shop", pane: "w14:p1", ws: "w14", words: "PNPM pack",
			want: []string{"global items and the items of workspace w14 (shop)",
				fmt.Sprintf("> [%d] global: Use pnpm for packages", global),
				fmt.Sprintf("  [%d] workspace w14 (shop): run pnpm test before a push", shop)},
			not: []string{fmt.Sprintf("[%d]", blog), "pending", "retired"}},
		{name: "blog", pane: "w15:p1", ws: "w15", words: "pnpm",
			want: []string{fmt.Sprintf("[%d] workspace w15 (blog): pnpm packages of the blog", blog)},
			not:  []string{fmt.Sprintf("[%d]", shop)}},
		// w16 has no saved root: no Brain key, so the global items only, as the brief.
		{name: "no key", pane: "w16:p1", ws: "w16", words: "pnpm",
			want: []string{"the global items only", fmt.Sprintf("> [%d] global: Use pnpm for packages", global)},
			not:  []string{fmt.Sprintf("[%d]", shop), fmt.Sprintf("[%d]", blog)}},
		{name: "no match", pane: "w14:p1", ws: "w14", words: "docker", want: []string{`no Brain item matches "docker"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			screen, err := f.popup(t, f.popupEnviron(c.pane, c.ws), c.words, 0, 0)
			if err != nil {
				t.Fatalf("popup(%q) = %v", c.words, err)
			}
			for _, w := range c.want {
				if !strings.Contains(screen, w) {
					t.Errorf("popup(%q) screen =\n%s\nwant %q", c.words, screen, w)
				}
			}
			for _, n := range c.not {
				if strings.Contains(screen, n) {
					t.Errorf("popup(%q) screen =\n%s\nwant no %q", c.words, screen, n)
				}
			}
			out, _, err := f.recall(t, c.pane, c.ws, strings.Fields(c.words)...)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := listed(screen), printed(out); !slices.Equal(got, want) {
				t.Errorf("popup(%q) lists %v, want the items of brain recall %v", c.words, got, want)
			}
		})
	}
}

// approvedWithSecret approves the extractor proposal of pane w14:p1, with a secret in the
// stored text of its newest excerpt, and returns the item and the proposal.
func (f *fixture) approvedWithSecret(t *testing.T, secretText string) (item, pid int64) {
	t.Helper()
	pid, _ = f.extractorProposal(t)
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE observations SET excerpt = ? WHERE capture_identity = 'a2'`, secretText)
		return err
	})
	f.mustReview(t, fmt.Sprint(pid), "--approve")
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		p, err := store.ProposalByID(ctx, tx, pid)
		item = *p.ItemID
		return err
	})
	return item, pid
}

// AC 3 (E20-S3): Enter shows the chosen item in full, with its source excerpts through the
// secret filter as in review, and the brain review command for a change (AC 4). Esc goes back
// to the search.
func TestRecallPopupItem(t *testing.T) {
	f := newFixture(t)
	const key = "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUv"
	item, pid := f.approvedWithSecret(t, "the key is "+key)
	screen, err := f.popup(t, f.popupEnviron("w14:p1", "w14"), "pnpm\r", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		fmt.Sprintf("item %d (preference, active), revision 2, workspace w14 (shop)\n- [%d] use pnpm\n", item, item),
		fmt.Sprintf("change:   brain review --item %d --edit \"<text>\" | --retire\n", item),
		fmt.Sprintf("proposal: brain review %d (its evidence view: brain review %d --evidence)\n", pid, pid),
		"excerpts:\n  unconfirmed, session", "workspace shop, " + localTime(testNow) + ": the key is [secret]",
		"\n" + recallItemKeys + "\n",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("item screen =\n%s\nwant %q", screen, want)
		}
	}
	if strings.Contains(screen, key) {
		t.Errorf("item screen =\n%s\nholds the secret", screen)
	}
	// The excerpt lines are the ones that brain review <proposal> shows.
	out := f.mustReview(t, fmt.Sprint(pid))
	_, ex, _ := strings.Cut(out, "excerpts:\n")
	ex, _, _ = strings.Cut(ex, "source:")
	if !strings.Contains(screen, ex) {
		t.Errorf("item screen =\n%s\nwant the excerpts of brain review %d:\n%s", screen, pid, ex)
	}

	back, err := f.popup(t, f.popupEnviron("w14:p1", "w14"), "pnpm\r\x1b", 0, 0)
	if err != nil || !strings.Contains(back, fmt.Sprintf("> [%d] workspace w14 (shop): use pnpm", item)) {
		t.Errorf("popup after Esc = %v, screen =\n%s\nwant the search again", err, back)
	}
}

// AC 3 (E20-S3): an item that you wrote with brain remember has no source excerpt; an edit in
// review keeps the excerpts of the extractor proposal that made the item.
func TestRecallPopupItemSource(t *testing.T) {
	f := newFixture(t)
	f.addActive(t, ptr("ws_a14"), "deploy with make ship")
	item, pid := f.approvedWithSecret(t, "text")
	if _, err := f.review(t, "--item", fmt.Sprint(item), "--edit", "use pnpm 9"); err != nil {
		t.Fatal(err)
	}
	f.mustReview(t, fmt.Sprint(pid+1), "--approve")
	cases := []struct{ keys, want string }{
		{"make\r", "excerpts: none: no captured source text made this item\n"},
		{"pnpm\r", fmt.Sprintf("revision 3, workspace w14 (shop)\n- [%d] use pnpm 9\n", item)},
		{"pnpm\r", fmt.Sprintf("proposal: brain review %d ", pid)},
	}
	for _, c := range cases {
		screen, err := f.popup(t, f.popupEnviron("w14:p1", "w14"), c.keys, 0, 0)
		if err != nil || !strings.Contains(screen, c.want) {
			t.Errorf("popup(%q) = %v, screen =\n%s\nwant %q", c.keys, err, screen, c.want)
		}
	}
}

// AC 4 (E20-S3): the popup changes no data: no recall record, even with a receipt that brain
// recall records on, no workspace map sync (a later clock would show), no agent check of the
// pane, and no other row or map change.
func TestRecallPopupNoChange(t *testing.T) {
	f := newFixture(t)
	f.approvedWithSecret(t, "text")
	recallItem(t, f, "", "use pnpm everywhere", store.StateActive, "")
	if _, err := f.review(t); err != nil {
		t.Fatal(err)
	}
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		session := "default"
		for _, agent := range []string{"a", "b"} {
			d := store.Delivery{Profile: "claude", HerdrSession: &session, PaneID: "w14:p1", AgentSessionID: &agent,
				ItemRevisions: []store.RevisionRef{}, Result: store.ResultEmitted, At: testNow}
			id, err := store.InsertDelivery(ctx, tx, d)
			if err != nil {
				return err
			}
			if agent == "b" {
				// A pending agent check: the next brain call that writes checks it.
				if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET agent_check = ? WHERE id = ?`, resolver.AgentPending, id); err != nil {
					return err
				}
			}
		}
		return nil
	})
	p := f.panes["w14:p1"]
	p.AgentSession = &herdr.AgentSession{Agent: "claude", Kind: "id", Value: "a"}
	f.panes["w14:p1"] = p
	before, beforeMap := f.dump(t, f.tables(t)...), string(readFile(t, filepath.Join(f.store, "workspaces.json")))
	syncs := f.syncs
	if _, err := f.popupOnly(t, f.popupEnviron("w14:p1", "w14"), "pnpm\x1b[B\r\x1b[B\x1bpnpm\r", 0, 0); err != nil {
		t.Fatal(err)
	}
	if after := f.dump(t, f.tables(t)...); after != before {
		t.Errorf("store after the popup =\n%s\nwant it as before:\n%s", after, before)
	}
	if got := string(readFile(t, filepath.Join(f.store, "workspaces.json"))); got != beforeMap || f.syncs != syncs {
		t.Errorf("popup synced the workspace map (%d syncs, map changed: %t), want no sync", f.syncs-syncs, got != beforeMap)
	}
	// brain recall from the same pane records on the receipt of agent a: the popup did not.
	if _, _, err := f.recall(t, "w14:p1", "w14", "pnpm"); err != nil || len(f.recalls(t)) != 1 {
		t.Errorf("Recall() = %v, recalls = %v, want one row", err, f.recalls(t))
	}
}

// The popup checks its pane again before each read (§8.6): a pane that moved to another
// workspace gets no item of its old workspace.
func TestRecallPopupRecheck(t *testing.T) {
	f := newFixture(t)
	shop := recallItem(t, f, "ws_a14", "pnpm in shop", store.StateActive, "")
	if _, err := f.review(t); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	e := f.env(f.sock, "", "", &out)
	e.Environ = f.popupEnviron("w14:p1", "w14")
	// The popup opens, then the pane moves before the first key.
	keys := &firstRead{Reader: strings.NewReader("pnpm"), fn: func() {
		f.panes["w14:p1"] = herdr.Pane{ID: "w14:p1", WorkspaceID: "w20", TabID: "w20:t1"}
	}}
	err := recallPopup(context.Background(), e, bufio.NewReader(keys), func() (int, int) { return 0, 0 })
	screen := out.String()
	last := screen[strings.LastIndex(screen, clearScreen):]
	if err != nil || !strings.Contains(last, "moved to workspace w20") || strings.Contains(last, fmt.Sprintf("[%d]", shop)) {
		t.Errorf("popup after a move = %v, screen =\n%s\nwant the move error and no shop item", err, last)
	}
}

// tables are the names of the tables of the store.
func (f *fixture) tables(t *testing.T) []string {
	t.Helper()
	var names []string
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
		if err != nil {
			return err
		}
		defer rows.Close() //nolint:errcheck // read-only
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			names = append(names, n)
		}
		return rows.Err()
	})
	return names
}

// The keys of the popup: Up and Down (arrows, Ctrl+P and Ctrl+N) move the choice and stay in
// the list, Backspace edits the words, Esc on the search closes, q on an item closes.
func TestRecallPopupKeys(t *testing.T) {
	f := newFixture(t)
	a := recallItem(t, f, "", "pnpm one", store.StateActive, "")
	b := recallItem(t, f, "", "pnpm two", store.StateActive, "")
	cases := []struct{ name, keys, want string }{
		{"first", "pnpm", fmt.Sprintf("> [%d]", a)},
		{"down", "pnpm\x1b[B", fmt.Sprintf("> [%d]", b)},
		{"down past the end", "pnpm\x1b[B\x1b[B\x0e", fmt.Sprintf("> [%d]", b)},
		{"down up", "pnpm\x1b[B\x1b[A", fmt.Sprintf("> [%d]", a)},
		{"ctrl n ctrl p", "pnpm\x0e\x10\x0e", fmt.Sprintf("> [%d]", b)},
		{"enter the second", "pnpm\x1b[B\r", fmt.Sprintf("item %d ", b)},
		{"backspace", "pnpmx\x7f", fmt.Sprintf("> [%d]", a)},
		{"no words", "  ", "type words to search"},
		{"esc closes", "pnpm\x1bdocker", fmt.Sprintf("> [%d]", a)},
		{"q closes the item", "pnpm\rq\x1bdocker", fmt.Sprintf("item %d ", a)},
		{"enter on no match", "docker\r", `no Brain item matches "docker"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			screen, err := f.popup(t, f.popupEnviron("w14:p1", "w14"), c.keys, 0, 0)
			if err != nil || !strings.Contains(screen, c.want) {
				t.Errorf("popup(%q) = %v, screen =\n%s\nwant %q", c.keys, err, screen, c.want)
			}
		})
	}
}

// A long item view is cut to the popup, and its keys stay the last line.
func TestRecallPopupCut(t *testing.T) {
	f := newFixture(t)
	recallItem(t, f, "", "pnpm "+strings.Repeat("long ", 30)+"\nsecond "+strings.Repeat("line ", 30), store.StateActive, "")
	screen, err := f.popup(t, f.popupEnviron("w14:p1", "w14"), "pnpm\r", 8, 60)
	if err != nil {
		t.Fatal(err)
	}
	if n := screenRows(screen, 60); n > 8 {
		t.Errorf("screen takes %d rows, want at most 8:\n%s", n, screen)
	}
	if !strings.HasSuffix(screen, "\n"+recallItemKeys+"\n") || !strings.Contains(screen, "more lines: Down shows them\n") {
		t.Errorf("screen =\n%s\nwant a cut line and the keys last", screen)
	}
}

// popupEnv takes the caller of a popup from the action context, and keeps the pane of a call
// that has its own.
func TestPopupEnv(t *testing.T) {
	ctxJSON := func(s string) string { return "HERDR_PLUGIN_CONTEXT_JSON=" + s }
	cases := []struct {
		name    string
		environ []string
		want    []string
		err     string
	}{
		{name: "context", environ: []string{"HOME=/h", ctxJSON(`{"workspace_id":"w2","focused_pane_id":"w2:p1"}`)},
			want: []string{"HOME=/h", ctxJSON(`{"workspace_id":"w2","focused_pane_id":"w2:p1"}`), "HERDR_PANE_ID=w2:p1", "HERDR_WORKSPACE_ID=w2"}},
		{name: "own pane", environ: []string{"HERDR_PANE_ID=w1:p1", "HERDR_WORKSPACE_ID=w1", ctxJSON(`{"workspace_id":"w2","focused_pane_id":"w2:p1"}`)},
			want: []string{"HERDR_PANE_ID=w1:p1", "HERDR_WORKSPACE_ID=w1", ctxJSON(`{"workspace_id":"w2","focused_pane_id":"w2:p1"}`)}},
		{name: "no context", environ: []string{"HOME=/h"}, err: "herdr action (brain.recall, brain.retired)"},
		{name: "bad context", environ: []string{ctxJSON("{")}, err: "read HERDR_PLUGIN_CONTEXT_JSON"},
		{name: "no focused pane", environ: []string{ctxJSON(`{"workspace_id":"w2"}`)}, err: "no focused pane"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := popupEnv(c.environ)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Errorf("popupEnv() = %v, %v, want an error with %q", got, err, c.err)
				}
				return
			}
			if err != nil || !slices.Equal(got, c.want) {
				t.Errorf("popupEnv() = %v, %v, want %v", got, err, c.want)
			}
		})
	}
}

// A popup whose focused pane herdr does not confirm reads nothing: it shows the error until a
// key, and returns it.
func TestRecallPopupDenied(t *testing.T) {
	f := newFixture(t)
	for _, environ := range [][]string{f.popupEnviron("w99:p1", "w99"), {"HERDR_ENV=1"}} {
		screen, err := f.popup(t, environ, "x", 0, 0)
		if err == nil || !strings.Contains(screen, err.Error()) || !strings.HasSuffix(screen, "Press a key to close.") {
			t.Errorf("popup(%v) = %v, screen =\n%s\nwant the error and a key to close", environ, err, screen)
		}
	}
}

// firstRead is a reader that runs fn before its first read.
type firstRead struct {
	io.Reader
	fn func()
}

func (r *firstRead) Read(b []byte) (int, error) {
	if r.fn != nil {
		r.fn()
		r.fn = nil
	}
	return r.Reader.Read(b)
}

// AC 3 and 4 (E20-S3): Down scrolls a long item view, so its last lines show too; Up goes back.
func TestRecallPopupScroll(t *testing.T) {
	f := newFixture(t)
	id := recallItem(t, f, "", "pnpm "+strings.Repeat("long ", 30)+"\nsecond "+strings.Repeat("line ", 30), store.StateActive, "")
	end, err := f.popup(t, f.popupEnviron("w14:p1", "w14"), "pnpm\r"+strings.Repeat("\x1b[B", 10), 8, 60)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("change:   brain review --item %d --edit \"<text>\" | --retire", id)
	if !strings.Contains(end, want) || !strings.Contains(end, "more lines above\n") || strings.Contains(end, "Down shows them") {
		t.Errorf("screen after Down =\n%s\nwant the last lines, with %q", end, want)
	}
	if n := screenRows(end, 60); n > 8 {
		t.Errorf("screen takes %d rows, want at most 8:\n%s", n, end)
	}
	top, err := f.popup(t, f.popupEnviron("w14:p1", "w14"), "pnpm\r"+strings.Repeat("\x1b[B", 10)+strings.Repeat("\x1b[A", 10), 8, 60)
	if err != nil || !strings.HasPrefix(top, fmt.Sprintf("item %d ", id)) {
		t.Errorf("screen after Up = %v,\n%s\nwant the first line again", err, top)
	}
}

// A list that does not fit the popup keeps the chosen item on the screen.
func TestRecallPopupListWindow(t *testing.T) {
	f := newFixture(t)
	var ids []int64
	for i := range 10 {
		ids = append(ids, recallItem(t, f, "", fmt.Sprintf("pnpm %d", i), store.StateActive, ""))
	}
	screen, err := f.popup(t, f.popupEnviron("w14:p1", "w14"), "pnpm"+strings.Repeat("\x1b[B", 9), 9, 60)
	if err != nil || !strings.Contains(screen, fmt.Sprintf("> [%d] global: pnpm 9", ids[9])) {
		t.Errorf("screen = %v,\n%s\nwant the last item chosen and shown", err, screen)
	}
	if n := screenRows(screen, 60); n > 9 {
		t.Errorf("screen takes %d rows, want at most 9:\n%s", n, screen)
	}
	if got := listed(screen); len(got) == 0 || len(got) == len(ids) {
		t.Errorf("screen lists %v, want a window of the 10 items", got)
	}
}
