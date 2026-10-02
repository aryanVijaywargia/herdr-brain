package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// fakeHerdr is the herdr of the fixture's session for the review pane calls.
type fakeHerdr struct {
	targets []herdr.Target // the targets that Env.Herdr got
	openErr error
	next    string // the id of the next opened pane
	opened  []herdr.PluginPaneSpec
	procs   map[string][]herdr.Process // by pane id; a missing pane is gone
	asked   []string                   // the panes whose processes were read
	closed  []string
	notes   []string // the notifications, as "title: body"
}

func (h *fakeHerdr) OpenPluginPane(_ context.Context, s herdr.PluginPaneSpec) (herdr.PluginPane, error) {
	h.opened = append(h.opened, s)
	if h.openErr != nil {
		return herdr.PluginPane{}, h.openErr
	}
	ws, _, _ := strings.Cut(h.next, ":")
	return herdr.PluginPane{PluginID: s.Plugin, Entrypoint: s.Entrypoint,
		Pane: herdr.Pane{ID: h.next, WorkspaceID: ws, TabID: ws + ":t1"}}, nil
}

func (h *fakeHerdr) PaneProcesses(_ context.Context, id string) ([]herdr.Process, error) {
	h.asked = append(h.asked, id)
	ps, ok := h.procs[id]
	if !ok {
		return nil, fmt.Errorf("pane %s: %w", id, herdr.ErrPaneNotFound)
	}
	return ps, nil
}

func (h *fakeHerdr) ShowNotification(_ context.Context, title, body string) error {
	h.notes = append(h.notes, title+": "+body)
	return nil
}

func (h *fakeHerdr) ClosePluginPane(_ context.Context, id string) error {
	h.closed = append(h.closed, id)
	delete(h.procs, id)
	return nil
}

// reviewProc is the process that herdr shows in the review pane of proposal id.
func reviewProc(id int64) []herdr.Process {
	return []herdr.Process{{Name: "brain", Argv: []string{"/p/bin/brain", "pane", "review", "--proposal", fmt.Sprint(id)}}}
}

// reviewPanes reads every review_panes row.
func (f *fixture) reviewPanes(t *testing.T) []store.ReviewPane {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var ps []store.ReviewPane
	write(t, s, func(tx *sql.Tx) error {
		for id := int64(1); ; id++ {
			p, err := store.ReviewPaneByID(context.Background(), tx, id)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			ps = append(ps, p)
		}
	})
	return ps
}

// addReviewPane writes a review_panes row for proposal id.
func (f *fixture) addReviewPane(t *testing.T, p store.ReviewPane) {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	write(t, s, func(tx *sql.Tx) error {
		_, err := store.InsertReviewPane(context.Background(), tx, p)
		return err
	})
}

// S52: an agent's `brain remember` opens the review pane beside that agent's pane, with no
// focus, and records the pane with its binding and continuity identity.
func TestRememberOpensReviewPane(t *testing.T) {
	f := newFixture(t)
	var out bytes.Buffer
	if err := Remember(context.Background(), f.env(f.sock, "w14:p1", "w14", &out), []string{"use pnpm"}); err != nil {
		t.Fatalf("Remember() = %v", err)
	}
	want := []herdr.PluginPaneSpec{{Plugin: "brain", Entrypoint: "review", Placement: "split", TargetPane: "w14:p1",
		Env: []string{"BRAIN_PROPOSAL_ID=1"}}}
	if !reflect.DeepEqual(f.herdr.opened, want) {
		t.Errorf("opened %+v, want %+v", f.herdr.opened, want)
	}
	// The client always passes --no-focus (TestOpenPluginPane).
	if ts := f.herdr.targets; len(ts) != 1 || ts[0] != (herdr.Target{Session: "default", Socket: f.sock}) {
		t.Errorf("herdr targets = %+v, want the caller's session", ts)
	}
	key := "ws_a14"
	wantRows := []store.ReviewPane{{ID: 1, HerdrSession: "default", SessionIdentity: &key, PaneID: "w14:p5",
		BesidePaneID: "w14:p1", ProposalID: 1, Opened: testNow}}
	if got := f.reviewPanes(t); !reflect.DeepEqual(got, wantRows) {
		t.Errorf("review_panes = %+v, want %+v", got, wantRows)
	}
	if !strings.Contains(out.String(), "waiting for your review in pane w14:p5") {
		t.Errorf("Remember printed %q, want the review pane", out.String())
	}
	if r := f.rows(t); r.items[1].State != store.StateDraft || r.proposals[0].Status != store.ProposalPending {
		t.Errorf("item %s, proposal %s, want a pending draft until you act", r.items[1].State, r.proposals[0].Status)
	}
}

// S53: when the pane cannot open, the draft waits in `brain review`, one line says so, and
// nothing goes live.
func TestRememberOpenFails(t *testing.T) {
	f := newFixture(t)
	before := f.rows(t).newest
	f.herdr.openErr = errors.New("split failed")
	var out bytes.Buffer
	if err := Remember(context.Background(), f.env(f.sock, "w14:p1", "w14", &out), []string{"use pnpm"}); err != nil {
		t.Fatalf("Remember() = %v, want nil: the draft is written", err)
	}
	want := "proposal 1: workspace w14 (shop) draft, waiting for your review (brain review); the review pane did not open: split failed\n"
	if out.String() != want {
		t.Errorf("Remember printed %q, want %q", out.String(), want)
	}
	r := f.rows(t)
	if len(r.proposals) != 1 || r.proposals[0].Status != store.ProposalPending || r.items[1].State != store.StateDraft {
		t.Errorf("proposals %+v, items %+v, want one pending draft", r.proposals, r.items)
	}
	if r.newest != before {
		t.Errorf("newest generation %d, want %d: nothing goes live", r.newest, before)
	}
	if ps := f.reviewPanes(t); len(ps) != 0 {
		t.Errorf("review_panes = %+v, want none", ps)
	}
	if list := f.mustReview(t); !strings.Contains(list, "1  workspace w14 (shop)  use pnpm") {
		t.Errorf("brain review = %q, want the draft", list)
	}
}

// A decision in `brain review` closes the review pane of the proposal, once herdr shows that
// it still runs the review pane of that proposal.
func TestReviewClosesPane(t *testing.T) {
	for _, args := range [][]string{{"--approve"}, {"--reject"}, {"--later"}} {
		t.Run(args[0], func(t *testing.T) {
			f := newFixture(t)
			id := f.remember(t, "use pnpm")
			f.herdr.procs["w14:p5"] = reviewProc(id)
			f.mustReview(t, append([]string{fmt.Sprint(id)}, args...)...)
			if !reflect.DeepEqual(f.herdr.closed, []string{"w14:p5"}) {
				t.Errorf("closed %v, want [w14:p5]", f.herdr.closed)
			}
			if ps := f.reviewPanes(t); ps[0].Closed != testNow {
				t.Errorf("row closed = %v, want %v", ps[0].Closed, testNow)
			}
		})
	}
}

// bindSession binds herdr session name to the fixture's store, with its own socket and herdr.
func (f *fixture) bindSession(t *testing.T, name string) *fakeHerdr {
	t.Helper()
	sock := filepath.Join(filepath.Dir(filepath.Dir(f.sock)), name, "herdr.sock")
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	bs, _, err := bindings.Read(f.file)
	if err != nil {
		t.Fatal(err)
	}
	bs = append(bs, bindings.Binding{Session: name, Socket: sock, Store: f.store, Kind: bindings.KindMain, Created: testNow})
	if err := bindings.Write(f.file, bs); err != nil {
		t.Fatal(err)
	}
	h := &fakeHerdr{procs: map[string][]herdr.Process{}}
	if f.others == nil {
		f.others = map[string]*fakeHerdr{}
	}
	f.others[sock] = h
	return h
}

// S82: herdr session A has a review pane w14:p2. Session B ("default" here) has an agent at
// w14:p2. A decision in B closes A's pane through A's binding, and does not touch B's w14:p2.
func TestReviewClosesPaneInOtherSession(t *testing.T) {
	f := newFixture(t)
	a := f.bindSession(t, "a")
	f.herdr.openErr = errors.New("no pane") // only the row of session A
	id := f.remember(t, "use pnpm")
	// A's w14 is not in the map, so the row has no identity yet.
	f.addReviewPane(t, store.ReviewPane{HerdrSession: "a", PaneID: "w14:p2", BesidePaneID: "w14:p1",
		ProposalID: id, Opened: testNow})
	a.procs["w14:p2"] = reviewProc(id)
	f.herdr.procs["w14:p2"] = []herdr.Process{{Name: "claude", Argv: []string{"claude"}}}
	f.mustReview(t, fmt.Sprint(id), "--reject")
	if !reflect.DeepEqual(a.closed, []string{"w14:p2"}) || len(a.targets) != 1 || a.targets[0].Session != "a" {
		t.Errorf("session A: closed %v, targets %+v, want w14:p2 closed in A", a.closed, a.targets)
	}
	if len(f.herdr.closed) != 0 || len(f.herdr.asked) != 0 {
		t.Errorf("session B: closed %v, asked %v, want no call", f.herdr.closed, f.herdr.asked)
	}
	if f.reviewPanes(t)[0].Closed.IsZero() {
		t.Error("the row is still open")
	}
}

// Brain closes no pane whose row fails a check. The row is invalid, so it is marked closed.
func TestReviewKeepsOtherPanes(t *testing.T) {
	old := "ws_old"
	cases := []struct {
		name      string
		row       store.ReviewPane
		procs     []herdr.Process // what herdr shows at the row's pane id; nil: no pane
		withheld  bool            // w15's saved root moves out of its project after the open
		wantAsked []string
	}{
		{
			// Session A is not bound to this store now, so Brain cannot reach its pane, and it
			// does not touch B's w14:p2.
			name: "a session with no binding",
			row: store.ReviewPane{HerdrSession: "a", SessionIdentity: ptr("ws_a14"), PaneID: "w14:p2",
				BesidePaneID: "w14:p1", Opened: testNow},
			procs: []herdr.Process{{Name: "claude", Argv: []string{"claude"}}},
		},
		{
			name: "an agent at the pane id now",
			row: store.ReviewPane{HerdrSession: "default", SessionIdentity: ptr("ws_a14"), PaneID: "w14:p2",
				BesidePaneID: "w14:p1", Opened: testNow},
			procs:     []herdr.Process{{Name: "claude", Argv: []string{"claude"}}},
			wantAsked: []string{"w14:p2"},
		},
		{
			name: "the review pane of another proposal",
			row: store.ReviewPane{HerdrSession: "default", SessionIdentity: ptr("ws_a14"), PaneID: "w14:p2",
				BesidePaneID: "w14:p1", Opened: testNow},
			procs:     reviewProc(9),
			wantAsked: []string{"w14:p2"},
		},
		{
			name: "continuity lost",
			row: store.ReviewPane{HerdrSession: "default", SessionIdentity: &old, PaneID: "w14:p2",
				BesidePaneID: "w14:p1", Opened: testNow},
			procs: reviewProc(1),
		},
		{
			name: "the workspace is withheld",
			row: store.ReviewPane{HerdrSession: "default", SessionIdentity: ptr("ws_a15"), PaneID: "w15:p2",
				BesidePaneID: "w15:p1", Opened: testNow},
			procs:    reviewProc(1),
			withheld: true,
		},
		{
			name: "the pane is gone",
			row: store.ReviewPane{HerdrSession: "default", SessionIdentity: ptr("ws_a14"), PaneID: "w14:p2",
				BesidePaneID: "w14:p1", Opened: testNow},
			wantAsked: []string{"w14:p2"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.herdr.openErr = errors.New("no pane") // only the row of the case
			id := f.remember(t, "use pnpm")
			c.row.ProposalID = id
			f.addReviewPane(t, c.row)
			if c.procs != nil {
				f.herdr.procs["w14:p2"] = c.procs
			}
			if c.withheld {
				file := filepath.Join(filepath.Dir(f.sock), "session.json")
				var ss struct {
					Version    int                 `json:"version"`
					Workspaces []map[string]string `json:"workspaces"`
				}
				if b, err := os.ReadFile(file); err != nil || json.Unmarshal(b, &ss) != nil {
					t.Fatalf("read %s: %v", file, err)
				}
				ss.Workspaces[1]["identity_cwd"] = t.TempDir() // w15
				writeJSON(t, file, ss)
			}
			f.mustReview(t, fmt.Sprint(id), "--reject")
			if len(f.herdr.closed) != 0 {
				t.Errorf("closed %v, want none", f.herdr.closed)
			}
			if !reflect.DeepEqual(f.herdr.asked, c.wantAsked) {
				t.Errorf("read the processes of %v, want %v", f.herdr.asked, c.wantAsked)
			}
			if f.reviewPanes(t)[0].Closed.IsZero() {
				t.Error("the row is still open, want it marked closed")
			}
		})
	}
}

// A decision in the review pane itself marks its row closed: the pane exits by itself, and
// Brain makes no herdr call.
func TestPaneDecisionMarksRow(t *testing.T) {
	f := newFixture(t)
	id := f.remember(t, "use pnpm")
	f.herdr.procs["w14:p5"] = reviewProc(id)
	f.panes["w14:p5"] = herdr.Pane{ID: "w14:p5", WorkspaceID: "w14", TabID: "w14:t1"}
	var out bytes.Buffer
	if err := runReview(context.Background(), f.env(f.sock, "w14:p5", "w14", &out), store.SurfacePane,
		[]string{fmt.Sprint(id), "--reject"}); err != nil {
		t.Fatal(err)
	}
	if len(f.herdr.closed) != 0 || len(f.herdr.asked) != 0 {
		t.Errorf("closed %v, asked %v, want no herdr call", f.herdr.closed, f.herdr.asked)
	}
	if f.reviewPanes(t)[0].Closed.IsZero() {
		t.Error("the row is still open")
	}
}
