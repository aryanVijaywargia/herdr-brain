package launch

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// fakeHerdr is a herdr API socket that answers pane.get.
type fakeHerdr struct {
	path  string
	mu    sync.Mutex
	panes map[string]string // pane id → its agent; "" for none
	moved map[string]string // an old pane id → its new id: pane.get answers the old id
}

func newFakeHerdr(t *testing.T) *fakeHerdr {
	t.Helper()
	dir, err := os.MkdirTemp("", "lh") // short: a socket path has a limit of about 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &fakeHerdr{path: filepath.Join(dir, "s"), panes: map[string]string{}, moved: map[string]string{}}
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.answer(conn)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return f
}

func (f *fakeHerdr) set(id, agent string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panes[id] = agent
}

// answer answers the one pane.get request of conn.
func (f *fakeHerdr) answer(conn net.Conn) {
	defer conn.Close() //nolint:errcheck // test
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req struct {
		Params struct {
			PaneID string `json:"pane_id"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}
	f.mu.Lock()
	id := req.Params.PaneID
	if n, ok := f.moved[id]; ok {
		id = n
	}
	agent, ok := f.panes[id]
	f.mu.Unlock()
	if !ok {
		_, _ = io.WriteString(conn, `{"id":"brain","error":{"code":"pane_not_found","message":"no pane"}}`+"\n")
		return
	}
	a := "null"
	if agent != "" {
		a = `"` + agent + `"`
	}
	ws, _, _ := strings.Cut(id, ":")
	_, _ = fmt.Fprintf(conn, `{"id":"brain","result":{"type":"pane_info","pane":{"pane_id":%q,"workspace_id":%q,"tab_id":"%s:t1","agent":%s}}}`+"\n",
		id, ws, ws, a)
}

// env is one store bound to the herdr session s1 of a fake herdr.
type env struct {
	f        *fakeHerdr
	b        bindings.Binding
	bindings string
}

func newEnv(t *testing.T) env {
	t.Helper()
	f := newFakeHerdr(t)
	b := bindings.Binding{Session: "s1", Socket: f.path, Store: t.TempDir(), Kind: bindings.KindMain, Created: time.Now()}
	bf := filepath.Join(t.TempDir(), "bindings.json")
	if err := bindings.Write(bf, []bindings.Binding{b}); err != nil {
		t.Fatal(err)
	}
	return env{f: f, b: b, bindings: bf}
}

func (e env) create(t *testing.T, pane, cli string) string {
	t.Helper()
	dir, err := Create(context.Background(), e.b, "w1", pane, cli, time.Now())
	if err != nil {
		t.Fatalf("Create() = %v", err)
	}
	return dir
}

func (e env) check(t *testing.T, dir string, environ []string) (Launch, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return Check(ctx, e.bindings, dir, environ)
}

// records returns every launch record of the store of e, with no times. The store has an end
// reason exactly when it has an end.
func (e env) records(t *testing.T) []store.Launch {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, e.b.Store, busyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	var ls []store.Launch
	err = st.Write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, id_hash, socket, workspace_id, pane_id, cli_kind, native_session_id,
			COALESCE(end_reason, '') FROM launches ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close() //nolint:errcheck // test
		for rows.Next() {
			var l store.Launch
			if err := rows.Scan(&l.ID, &l.IDHash, &l.Socket, &l.WorkspaceID, &l.PaneID, &l.CLI, &l.NativeSessionID, &l.EndReason); err != nil {
				return err
			}
			ls = append(ls, l)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return ls
}

// AC 3: the launch folder is 0700 and its ID file 0600; the ID is 128 random bits; the store
// has only the SHA-256 of the ID, and no store file holds the ID.
func TestCreate(t *testing.T) {
	e := newEnv(t)
	dir := e.create(t, "w1:p1", "claude")
	if want := paths.Launch(e.b.Store, 1); dir != want {
		t.Errorf("Create() = %s, want %s", dir, want)
	}
	for path, want := range map[string]fs.FileMode{dir: 0o700, filepath.Join(dir, idFile): 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("mode of %s = %o, want %o", path, got, want)
		}
	}
	id, err := os.ReadFile(filepath.Join(dir, idFile))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(string(id))
	if err != nil || len(raw) != 16 {
		t.Fatalf("launch ID %q: %v, want 32 hex digits", id, err)
	}
	if strings.Contains(dir, string(id)) {
		t.Errorf("launch folder %s holds the launch ID", dir)
	}
	sum := sha256.Sum256(raw)
	ls := e.records(t)
	if len(ls) != 1 || ls[0].IDHash != hex.EncodeToString(sum[:]) || ls[0].Socket != e.b.Socket ||
		ls[0].WorkspaceID != "w1" || ls[0].PaneID != "w1:p1" || ls[0].CLI != "claude" || ls[0].NativeSessionID != "" {
		t.Errorf("records = %+v, want one claude launch of w1:p1 with the hash of the ID", ls)
	}
	files, err := filepath.Glob(filepath.Join(e.b.Store, store.FileName+"*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("store files = %v, %v", files, err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, id) || bytes.Contains(data, raw) {
			t.Errorf("store file %s holds the launch ID", f)
		}
	}
	if other := e.create(t, "w1:p2", "codex"); other == dir {
		t.Errorf("second Create() = %s, want a new folder", other)
	}
}

// A launch needs its pane, workspace and CLI kind.
func TestCreateRefusesEmpty(t *testing.T) {
	e := newEnv(t)
	if _, err := Create(context.Background(), e.b, "w1", "", "claude", time.Now()); err == nil {
		t.Error("Create() with no pane = nil, want an error")
	}
	if ls := e.records(t); len(ls) != 0 {
		t.Errorf("records = %+v, want none", ls)
	}
}

// AC 1: the check passes a running launch in its live pane, and refuses an unknown or ended
// launch ID, an ID from another pane, a pane that is gone or moved, and another agent.
func TestCheck(t *testing.T) {
	for _, c := range []struct {
		name    string
		setup   func(t *testing.T, e env, dir string)
		environ []string
		want    Reason // "" for a pass
	}{
		{name: "running", environ: []string{"HERDR_PANE_ID=w1:p1"}},
		{name: "no agent yet", setup: func(_ *testing.T, e env, _ string) { e.f.set("w1:p1", "") }},
		{name: "unknown ID", setup: func(t *testing.T, _ env, dir string) {
			writeID(t, dir, strings.Repeat("ab", 16))
		}, want: Unknown},
		{name: "malformed ID", setup: func(t *testing.T, _ env, dir string) { writeID(t, dir, "not an id") }, want: Unknown},
		{name: "ID of another folder", setup: func(t *testing.T, e env, dir string) {
			other := e.create(t, "w1:p1", "claude")
			id, err := os.ReadFile(filepath.Join(other, idFile))
			if err != nil {
				t.Fatal(err)
			}
			writeID(t, dir, string(id))
		}, want: Unknown},
		{name: "folder gone", setup: func(t *testing.T, _ env, dir string) {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		}, want: Unknown},
		{name: "ended", setup: func(t *testing.T, e env, dir string) {
			id := idText(t, dir) // End deletes the folder; the ID is still at hand
			if err := End(context.Background(), e.b.Store, e.b.Socket, func(store.Launch) bool { return true },
				store.EndPaneClosed, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			writeID(t, dir, id)
		}, want: Ended},
		{name: "another pane", environ: []string{"HERDR_PANE_ID=w1:p2"}, want: OtherPane},
		{name: "pane gone", setup: func(_ *testing.T, e env, _ string) {
			e.f.mu.Lock()
			defer e.f.mu.Unlock()
			delete(e.f.panes, "w1:p1")
		}, want: PaneGone},
		{name: "moved", setup: func(_ *testing.T, e env, _ string) {
			e.f.mu.Lock()
			defer e.f.mu.Unlock()
			delete(e.f.panes, "w1:p1")
			e.f.panes["w2:p1"] = "claude"
			e.f.moved["w1:p1"] = "w2:p1"
		}, want: Moved},
		{name: "another agent", setup: func(_ *testing.T, e env, _ string) { e.f.set("w1:p1", "codex") }, want: OtherAgent},
		{name: "unbound store", setup: func(t *testing.T, e env, _ string) {
			if err := bindings.Write(e.bindings, nil); err != nil {
				t.Fatal(err)
			}
		}, want: Unbound},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.f.set("w1:p1", "claude")
			e.f.set("w1:p2", "claude")
			dir := e.create(t, "w1:p1", "claude")
			if c.setup != nil {
				c.setup(t, e, dir)
			}
			l, err := e.check(t, dir, c.environ)
			if c.want == "" {
				if err != nil || l.PaneID != "w1:p1" || l.Dir != dir || l.Binding.Session != "s1" || l.Binding.Store != e.b.Store {
					t.Errorf("Check() = %+v, %v, want the launch of w1:p1", l, err)
				}
				return
			}
			var r *RefusedError
			if !errors.As(err, &r) || r.Reason != c.want {
				t.Errorf("Check() = %v, want refused: %s", err, c.want)
			}
		})
	}
}

// A path that is not a launch folder is an error before any read.
func TestCheckNotAFolder(t *testing.T) {
	e := newEnv(t)
	dir := e.create(t, "w1:p1", "claude")
	for _, p := range []string{"launches/1", filepath.Join(e.b.Store, "other", "1"), filepath.Join(dir, idFile), dir + "/.", filepath.Join(filepath.Dir(dir), "0")} {
		if _, err := e.check(t, p, nil); err == nil {
			t.Errorf("Check(%s) = nil, want an error", p)
		}
	}
}

// End ends only the running records that gone picks, of its herdr server, and deletes their
// folders. A second End of the same record changes nothing.
func TestEnd(t *testing.T) {
	e := newEnv(t)
	p1 := e.create(t, "w1:p1", "claude")
	p2 := e.create(t, "w1:p2", "codex")
	other := e.b
	other.Socket = "/tmp/other.sock"
	p3, err := Create(context.Background(), other, "w1", "w1:p1", "claude", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	gone := func(l store.Launch) bool { return l.PaneID == "w1:p1" }
	for range 2 {
		if err := End(context.Background(), e.b.Store, e.b.Socket, gone, store.EndAgentExit, time.Now()); err != nil {
			t.Fatalf("End() = %v", err)
		}
	}
	ls := e.records(t)
	if len(ls) != 3 || ls[0].EndReason != store.EndAgentExit ||
		ls[1].EndReason != "" || ls[2].EndReason != "" {
		t.Errorf("records = %+v, want only the first ended by agent exit", ls)
	}
	if _, err := os.Stat(p1); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("folder %s: %v, want it deleted", p1, err)
	}
	for _, dir := range []string{p2, p3} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("folder %s: %v, want it kept", dir, err)
		}
	}
}

func idText(t *testing.T, dir string) string {
	t.Helper()
	id, err := os.ReadFile(filepath.Join(dir, idFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(id)
}

func writeID(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, idFile), []byte(id), 0o600); err != nil {
		t.Fatal(err)
	}
}
