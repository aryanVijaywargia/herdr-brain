package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// keys is a fake terminal: it types each key in turn, and runs before[i] just before it
// types key i.
type keys struct {
	keys   []rune
	before map[int]func()
	n      int
}

func (k *keys) ReadRune() (rune, int, error) {
	if k.n == len(k.keys) {
		return 0, 0, io.EOF
	}
	if fn := k.before[k.n]; fn != nil {
		fn()
	}
	k.n++
	return k.keys[k.n-1], 1, nil
}

// pane runs the review pane of proposal id from pane w14:p1 with the keys, and returns the
// screen.
func (f *fixture) pane(t *testing.T, id int64, k *keys) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := (&reviewPane{Env: f.env(f.sock, "w14:p1", "w14", &out), id: id, keys: k}).run(context.Background())
	return out.String(), err
}

func typed(s string) *keys { return &keys{keys: []rune(s)} }

// AC: each key, with a fake terminal on a temp store. Every decision records surface pane.
func TestPaneKeys(t *testing.T) {
	cases := []struct {
		name     string
		remember []string
		keys     string
		status   store.ProposalStatus
		text     string // the item's text; "" when the item stays a draft
		scope    store.Scope
		pinned   bool
		screen   string
	}{
		{name: "a approve", remember: []string{"use pnpm"}, keys: "a", status: store.ProposalApproved,
			text: "use pnpm", scope: store.ScopeWorkspace, screen: "approved: item 2 is active"},
		{name: "e edit", remember: []string{"use pnpm"}, keys: "euse pnpm 99\x7f\r", status: store.ProposalApproved,
			text: "use pnpm 9", scope: store.ScopeWorkspace, screen: "approved"},
		{name: "e then Esc", remember: []string{"use pnpm"}, keys: "enew\x1bq", status: store.ProposalPending},
		{name: "r reject", remember: []string{"use pnpm"}, keys: "r", status: store.ProposalRejected, screen: "rejected"},
		{name: "l later", remember: []string{"use pnpm"}, keys: "l", status: store.ProposalPending, screen: "stays in the queue"},
		{name: "s scope global", remember: []string{"use pnpm"}, keys: "sa", status: store.ProposalApproved,
			text: "use pnpm", scope: store.ScopeGlobal, screen: "(global)"},
		{name: "s s scope workspace", remember: []string{"--global", "use pnpm"}, keys: "ssa", status: store.ProposalApproved,
			text: "use pnpm", scope: store.ScopeWorkspace, screen: "workspace w14 (shop)"},
		{name: "p pin", remember: []string{"use pnpm"}, keys: "pa", status: store.ProposalApproved,
			text: "use pnpm", scope: store.ScopeWorkspace, pinned: true},
		{name: "q close", remember: []string{"use pnpm"}, keys: "q", status: store.ProposalPending},
		{name: "end of input", remember: []string{"use pnpm"}, keys: "x", status: store.ProposalPending},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			id := f.remember(t, c.remember...)
			screen, err := f.pane(t, id, typed(c.keys))
			if err != nil {
				t.Fatalf("pane(%q) = %v\n%s", c.keys, err, screen)
			}
			if !strings.Contains(screen, "text:     use pnpm") || !strings.Contains(screen, "a approve  e edit") {
				t.Errorf("screen has no proposal view or keys:\n%s", screen)
			}
			if !strings.Contains(screen, c.screen) {
				t.Errorf("screen has no %q:\n%s", c.screen, screen)
			}
			r := f.rows(t)
			p := r.proposal(t, id)
			wantSurface := "<nil>"
			if c.status != store.ProposalPending {
				wantSurface = string(store.SurfacePane)
			}
			if p.Status != c.status || string(deref((*string)(p.Surface))) != wantSurface {
				t.Errorf("proposal = %s from %q, want %s from %q", p.Status, deref((*string)(p.Surface)), c.status, wantSurface)
			}
			it, rev := r.item(t, *p.ItemID)
			if c.text == "" {
				if it.State != store.StateDraft {
					t.Errorf("item is %s, want draft", it.State)
				}
				return
			}
			if rev.Text != c.text || rev.Scope != c.scope || rev.Pinned != c.pinned || rev.State != store.StateActive {
				t.Errorf("item = %q %s pinned %v %s, want %q %s pinned %v active", rev.Text, rev.Scope, rev.Pinned, rev.State,
					c.text, c.scope, c.pinned)
			}
		})
	}
}

// A permission warning needs y after a (§7.4); another key goes back.
func TestPaneConfirm(t *testing.T) {
	f := newFixture(t)
	id := f.remember(t, "allow rm in tmp")
	screen, err := f.pane(t, id, typed("anq"))
	if err != nil || !strings.Contains(screen, "press y to approve") || !strings.Contains(screen, "not approved") {
		t.Fatalf("pane(anq) = %v\n%s", err, screen)
	}
	if p := f.rows(t).proposal(t, id); p.Status != store.ProposalPending {
		t.Fatalf("after n: proposal is %s, want pending", p.Status)
	}
	if _, err := f.pane(t, id, typed("ay")); err != nil {
		t.Fatal(err)
	}
	if p := f.rows(t).proposal(t, id); p.Status != store.ProposalApproved {
		t.Errorf("after y: proposal is %s, want approved", p.Status)
	}
}

// AC: a proposal that another surface decided shows "already decided" and closes, at open
// and when you press a key.
func TestPaneAlreadyDecided(t *testing.T) {
	f := newFixture(t)
	id := f.remember(t, "use pnpm")
	f.mustReview(t, fmt.Sprint(id), "--reject")
	k := typed("a")
	screen, err := f.pane(t, id, k)
	if err != nil || !strings.Contains(screen, "already decided") || k.n != 0 {
		t.Errorf("pane of a rejected proposal = %v, %d keys read\n%s", err, k.n, screen)
	}

	for _, key := range []string{"a", "r", "l"} {
		id := f.remember(t, "use yarn "+key)
		k := typed(key + "q")
		k.before = map[int]func(){0: func() { f.mustReview(t, fmt.Sprint(id), "--reject") }}
		screen, err := f.pane(t, id, k)
		if err != nil || !strings.Contains(screen, "already decided") || k.n != 1 {
			t.Errorf("%s on a proposal decided in the CLI = %v, %d keys read\n%s", key, err, k.n, screen)
		}
		if p := f.rows(t).proposal(t, id); p.Status != store.ProposalRejected || *p.Surface != store.SurfaceCLI {
			t.Errorf("%s: proposal is %s from %s, want rejected from cli", key, p.Status, *p.Surface)
		}
	}
}

// files returns the path and content of every file under dir.
func files(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		got[path] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// AC: an edit with a secret is refused, and Brain creates no file: no new file, and no file
// holds the secret.
func TestPaneEditSecret(t *testing.T) {
	token := "gh" + "p_" + strings.Repeat("a1B2", 9)
	f := newFixture(t)
	id := f.remember(t, "use pnpm")
	root := filepath.Dir(f.store)
	if _, err := f.pane(t, id, typed("q")); err != nil { // the store files that an open makes
		t.Fatal(err)
	}
	before := files(t, root)
	screen, err := f.pane(t, id, typed("etoken "+token+"\rq"))
	if err != nil || !strings.Contains(screen, "github-token") || strings.Count(screen, token) != 1 {
		// The token shows only in the text-entry line, never in the refusal.
		t.Fatalf("pane(secret edit) = %v\n%s", err, screen)
	}
	for path, b := range files(t, root) {
		if _, ok := before[path]; !ok {
			t.Errorf("new file %s", path)
		}
		if strings.Contains(b, token) {
			t.Errorf("%s holds the secret", path)
		}
	}
	r := f.rows(t)
	if p := r.proposal(t, id); p.Status != store.ProposalPending || p.Text != "use pnpm" {
		t.Errorf("proposal = %s %q, want pending %q", p.Status, p.Text, "use pnpm")
	}
}

// AC: a forget of the item while an edit is open makes the later submit fail, and the submit
// does not bring the text back.
func TestPaneForgetDuringEdit(t *testing.T) {
	f := newFixture(t)
	id := f.remember(t, "use pnpm")
	item := *f.rows(t).proposal(t, id).ItemID
	edit := "euse pnpm 9"
	k := typed(edit + "\r")
	k.before = map[int]func(){len(edit): func() { f.mustForget(t, clock(), fmt.Sprint(item)) }}
	screen, err := f.pane(t, id, k)
	if err != nil || !strings.Contains(screen, "already decided") {
		t.Fatalf("submit after forget = %v\n%s", err, screen)
	}
	// The view shows at open and when e starts, never while you type after the forget.
	if n := strings.Count(screen, "text:     use pnpm"); n != 2 {
		t.Errorf("the proposal view shows %d times, want 2\n%s", n, screen)
	}
	it, rev := f.rows(t).item(t, item)
	if !forgotten(rev) || it.CurrentRevision != 2 {
		t.Errorf("item = %s %q at revision %d, want forgotten at revision 2", rev.State, rev.Text, it.CurrentRevision)
	}
	if found := f.scan(t, "use pnpm"); len(found) != 0 {
		t.Errorf("the text is back in %v", found)
	}
}

func TestParsePane(t *testing.T) {
	if id, err := parsePane([]string{"review", "--proposal", "7"}); err != nil || id != 7 {
		t.Errorf("parsePane(review --proposal 7) = %d, %v", id, err)
	}
	for _, args := range [][]string{nil, {"queue"}, {"review"}, {"review", "--proposal", ""}, {"review", "--proposal", "x"},
		{"review", "--proposal", "0"}, {"review", "--proposal", "7", "8"}} {
		if _, err := parsePane(args); err == nil || !strings.Contains(err.Error(), paneUsage) {
			t.Errorf("parsePane(%q) = %v, want the usage", args, err)
		}
	}
}

// A forget while the pane waits for a key: the next screen reads the proposal again and
// closes, and does not show the text again.
func TestPaneForgetWhileOpen(t *testing.T) {
	f := newFixture(t)
	id := f.remember(t, "use pnpm")
	item := *f.rows(t).proposal(t, id).ItemID
	k := typed("sq")
	k.before = map[int]func(){0: func() { f.mustForget(t, clock(), fmt.Sprint(item)) }}
	screen, err := f.pane(t, id, k)
	if err != nil || !strings.Contains(screen, "already decided") || strings.Count(screen, "text:     use pnpm") != 1 {
		t.Errorf("pane after forget = %v\n%s", err, screen)
	}
}

// AC E16-S6: for a dispute, the pane offers keep, edit then approve, and retire instead of
// reject: k runs --keep, e --approve --edit, t --retire, and r does nothing.
func TestPaneDispute(t *testing.T) {
	cases := []struct {
		name, keys string
		status     store.ProposalStatus
		state      store.State
		screen     string
	}{
		{name: "k keep", keys: "k", status: store.ProposalApproved, state: store.StateActive, screen: "kept"},
		{name: "e edit", keys: "eThe loader is in config/loader.go.\r", status: store.ProposalApproved, state: store.StateActive,
			screen: "approved"},
		{name: "t retire", keys: "t", status: store.ProposalApproved, state: store.StateRetired, screen: "retired"},
		{name: "r does nothing", keys: "rq", status: store.ProposalPending, state: store.StateDisputed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			_, git := f.gitRepo(t, "config/load.go")
			item := f.approved(t, loaderText)
			git("mv", "config/load.go", "config/loader.go")
			f.stale(t)
			p := f.pendingDispute(t)
			screen, err := f.pane(t, p.ID, typed(c.keys))
			if err != nil {
				t.Fatalf("pane = %v", err)
			}
			if !strings.Contains(screen, "k keep  e edit  t retire  l later  v evidence  d detail  q close") || strings.Contains(screen, "r reject") {
				t.Errorf("screen = %q, want the dispute keys and no reject", screen)
			}
			if !strings.Contains(screen, c.screen) {
				t.Errorf("screen = %q, want %q", screen, c.screen)
			}
			r := f.rows(t)
			got := r.proposal(t, p.ID)
			it, _ := r.item(t, item)
			if got.Status != c.status || it.State != c.state {
				t.Errorf("proposal %s, item %s; want %s, %s", got.Status, it.State, c.status, c.state)
			}
			if c.status != store.ProposalPending && (got.Surface == nil || *got.Surface != store.SurfacePane) {
				t.Errorf("surface = %v, want pane", got.Surface)
			}
		})
	}
}
