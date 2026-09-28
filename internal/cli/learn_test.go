package cli

import (
	"bytes"
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

// learnSession is one more bound herdr session of the fixture, and its store.
type learnSession struct {
	name, sock, store string
	kind              bindings.Kind
}

// bindLearn binds herdr session name with its own socket: to the fixture's main store, or to
// a new test store with kind test.
func (f *fixture) bindLearn(t *testing.T, name string, kind bindings.Kind) learnSession {
	t.Helper()
	dir := filepath.Dir(f.store)
	s := learnSession{name: name, sock: filepath.Join(dir, name, "herdr.sock"), store: f.store, kind: kind}
	if kind == bindings.KindTest {
		s.store = filepath.Join(dir, name+"-store")
	}
	for _, d := range []string{filepath.Dir(s.sock), s.store} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bs, _, err := bindings.Read(f.file)
	if err != nil {
		t.Fatal(err)
	}
	bs = append(bs, bindings.Binding{Session: name, Socket: s.sock, Store: s.store, Kind: kind, Created: testNow})
	if err := bindings.Write(f.file, bs); err != nil {
		t.Fatal(err)
	}
	return s
}

// learn runs brain learn from pane w14:p1 of the herdr session with socket sock. In another
// session than default, herdr has every pane that the call names, and the workspace map is
// not synced.
func (f *fixture) learn(t *testing.T, sock string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e := f.env(sock, "w14:p1", "w14", &out)
	if sock != f.sock {
		// bindLearn puts the socket of session name in the folder name.
		e.Environ = append(e.Environ, "HERDR_SESSION="+filepath.Base(filepath.Dir(sock)))
		e.Pane = func(_ context.Context, target herdr.Target, id string) (herdr.Pane, error) {
			if target.Socket != sock {
				return herdr.Pane{}, herdr.ErrPaneNotFound
			}
			w, _, _ := strings.Cut(id, ":")
			return herdr.Pane{ID: id, WorkspaceID: w, TabID: w + ":t1"}, nil
		}
		e.Sync = func(context.Context, workspace.Session, *store.Store) error { return nil }
	}
	err := Learn(context.Background(), e, args)
	return out.String(), err
}

func (f *fixture) mustLearn(t *testing.T, sock string, args ...string) string {
	t.Helper()
	out, err := f.learn(t, sock, args...)
	if err != nil {
		t.Fatalf("Learn(%v) = %v", args, err)
	}
	return out
}

// learningOn reads the learning switch of pane in herdr session s, as E11 will.
func learningOn(t *testing.T, s learnSession, workspaceKey, pane string) bool {
	t.Helper()
	st := openStore(t, s.store)
	defer closeStore(t, st)
	var on bool
	write(t, st, func(tx *sql.Tx) (err error) {
		on, err = store.LearningOn(context.Background(), tx, s.kind != bindings.KindTest, s.name, workspaceKey, pane)
		return err
	})
	return on
}

// AC: the switch survives a new process; status shows it. Each call opens the store anew,
// and the switch lives only in the store.
func TestLearnStatus(t *testing.T) {
	f := newStatusFixture(t)
	if out := f.mustStatus(t); !strings.Contains(out, "\nlearning: on\n") {
		t.Errorf("status:\n%s\nwant learning: on", out)
	}
	out := f.mustLearn(t, f.sock, "off", "--pane", "w14:p1")
	if want := "learning is off for pane w14:p1 of herdr session \"default\". Delivery and brain remember stay on.\n"; out != want {
		t.Errorf("learn off --pane = %q, want %q", out, want)
	}
	f.mustLearn(t, f.sock, "off", "--workspace", "w15")
	if out := f.mustStatus(t); !strings.Contains(out, "\nlearning: on; off for workspace ws_a15, pane w14:p1 of herdr session \"default\"\n") {
		t.Errorf("status:\n%s\nwant the workspace and the pane off", out)
	}
	f.mustLearn(t, f.sock, "off")
	if out := f.mustStatus(t); !strings.Contains(out, "\nlearning: off for the whole store; delivery and brain remember stay on\n") {
		t.Errorf("status:\n%s\nwant learning off for the whole store", out)
	}
}

// AC: two real bindings with the same herdr ids, and one test binding: a pane switch in one
// does not change the others.
func TestLearnPaneIsolation(t *testing.T) {
	f := newFixture(t)
	main := learnSession{name: "default", sock: f.sock, store: f.store, kind: bindings.KindMain}
	other := f.bindLearn(t, "other", bindings.KindMain)
	test := f.bindLearn(t, "t1", bindings.KindTest)
	f.mustLearn(t, test.sock, "on")

	f.mustLearn(t, main.sock, "off", "--pane", "w14:p1")
	f.mustLearn(t, other.sock, "off", "--pane", "w14:p2")
	f.mustLearn(t, test.sock, "off", "--pane", "w14:p3")
	for _, tt := range []struct {
		s    learnSession
		pane string
		want bool
	}{
		{main, "w14:p1", false}, {main, "w14:p2", true}, {main, "w14:p3", true},
		{other, "w14:p1", true}, {other, "w14:p2", false}, {other, "w14:p3", true},
		{test, "w14:p1", true}, {test, "w14:p2", true}, {test, "w14:p3", false},
	} {
		if got := learningOn(t, tt.s, "ws_a14", tt.pane); got != tt.want {
			t.Errorf("session %s pane %s: learning on = %v, want %v", tt.s.name, tt.pane, got, tt.want)
		}
	}
}

// AC: a switch changes no item revision, no compiled output and no delivery.
func TestLearnChangesNothingElse(t *testing.T) {
	f := newFixture(t)
	receipt(t, f.store, "default", "claude", "w14:p1", "s-1", store.ResultEmitted, "")
	rows, out, deliveries := f.rows(t), readTree(t, filepath.Join(f.store, outDir)), countDeliveries(t, f.store)
	for _, args := range [][]string{{"off"}, {"off", "--workspace", "w14"}, {"off", "--pane", "w14:p1"},
		{"on", "--pane", "w14:p1"}, {"on"}} {
		f.mustLearn(t, f.sock, args...)
	}
	if got := f.rows(t); !reflect.DeepEqual(got, rows) {
		t.Errorf("rows = %+v, want %+v", got, rows)
	}
	if got := readTree(t, filepath.Join(f.store, outDir)); !reflect.DeepEqual(got, out) {
		t.Errorf("output = %v, want %v", got, out)
	}
	if got := countDeliveries(t, f.store); got != deliveries {
		t.Errorf("receipts = %d, want %d", got, deliveries)
	}
}

// AC: store off plus pane on: learning is off for that pane.
func TestLearnStoreOffWins(t *testing.T) {
	f := newFixture(t)
	s := learnSession{name: "default", sock: f.sock, store: f.store, kind: bindings.KindMain}
	f.mustLearn(t, f.sock, "off")
	out := f.mustLearn(t, f.sock, "on", "--pane", "w14:p1")
	if !strings.Contains(out, "Learning stays off there: a wider scope is off") {
		t.Errorf("learn on --pane = %q, want the store off note", out)
	}
	if learningOn(t, s, "ws_a14", "w14:p1") {
		t.Error("learning is on for w14:p1, want off: the store is off")
	}
}

// A workspace off also wins over a pane on in that workspace, and the output says so.
func TestLearnWorkspaceOffWins(t *testing.T) {
	f := newFixture(t)
	f.mustLearn(t, f.sock, "off", "--workspace", "w14")
	if out := f.mustLearn(t, f.sock, "on", "--pane", "w14:p1"); !strings.Contains(out, "Learning stays off there") {
		t.Errorf("learn on --pane = %q, want the stays off note", out)
	}
	s := learnSession{name: "default", sock: f.sock, store: f.store, kind: bindings.KindMain}
	if learningOn(t, s, "ws_a14", "w14:p1") {
		t.Error("learning is on for w14:p1, want off: its workspace is off")
	}
}

// AC: a new test store starts with learning off (§12.1); brain learn on there turns it on.
func TestLearnNewTestStoreOff(t *testing.T) {
	f := newFixture(t)
	test := f.bindLearn(t, "t1", bindings.KindTest)
	if learningOn(t, test, "", "w14:p1") {
		t.Error("learning is on in a new test store, want off")
	}
	f.mustLearn(t, test.sock, "on")
	if !learningOn(t, test, "", "w14:p1") {
		t.Error("learning is off after brain learn on, want on")
	}
	// The main store keeps its default.
	if !learningOn(t, learnSession{name: "default", store: f.store, kind: bindings.KindMain}, "", "w14:p1") {
		t.Error("learning is off in the main store, want on")
	}
}

func TestLearnErrors(t *testing.T) {
	f := newFixture(t)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{nil, "want on or off"},
		{[]string{"maybe"}, "want on or off"},
		{[]string{"off", "--workspace", "w14", "--pane", "w14:p1"}, "exclude each other"},
		{[]string{"off", "extra"}, `unexpected argument "extra"`},
		{[]string{"off", "--pane", "w99:p1"}, "pane w99:p1"},
		{[]string{"off", "--workspace", "w99"}, "workspace w99 is not mapped"},
		// An empty value never switches the whole store.
		{[]string{"off", "--workspace", ""}, "--workspace is empty"},
		{[]string{"off", "--pane", ""}, "--pane is empty"},
		// A label is not a workspace id, so it never picks another workspace than the id.
		{[]string{"off", "--workspace", "shop"}, "workspace shop is not mapped"},
		// herdr answers the old id of a moved pane with its new id (S49).
		{[]string{"off", "--pane", "w14:p2"}, "pane w14:p2 is now pane w20:p2"},
	} {
		if _, err := f.learn(t, f.sock, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Learn(%v) = %v, want %q", tt.args, err, tt.want)
		}
	}
	if n := countLearning(t, f.store); n != 0 {
		t.Errorf("learning settings = %d after failed calls, want 0", n)
	}
}

// readTree returns the files under dir, by path, with their content.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // a test reads its own temp folder
		files[path] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func countDeliveries(t *testing.T, dir string) int {
	return count(t, dir, "SELECT COUNT(*) FROM deliveries")
}

func countLearning(t *testing.T, dir string) int {
	return count(t, dir, "SELECT COUNT(*) FROM learning_settings")
}

func count(t *testing.T, dir, query string) int {
	t.Helper()
	s := openStore(t, dir)
	defer closeStore(t, s)
	var n int
	write(t, s, func(tx *sql.Tx) error { return tx.QueryRow(query).Scan(&n) })
	return n
}
