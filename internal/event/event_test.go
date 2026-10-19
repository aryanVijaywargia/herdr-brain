package event

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

var testNow = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

// eventJSON is the HERDR_PLUGIN_EVENT_JSON of a close in herdr 0.9.1 (E2-S6 test step 2).
const eventJSON = `{"event":"workspace_closed","data":{"type":"workspace_closed","workspace_id":"w2","workspace":{"label":"B2"}}}`

func TestParseClosed(t *testing.T) {
	tests := []struct {
		name    string
		environ []string
		want    Closed
		wantErr bool
	}{
		{"named session", []string{"HERDR_SESSION=brain-e2s6", "HERDR_SOCKET_PATH=/s/herdr.sock", "HERDR_PLUGIN_EVENT_JSON=" + eventJSON},
			Closed{Session: "brain-e2s6", Socket: "/s/herdr.sock", WorkspaceID: "w2"}, false},
		{"default session", []string{"HERDR_SOCKET_PATH=/s/herdr.sock", "HERDR_PLUGIN_EVENT_JSON=" + eventJSON},
			Closed{Session: "default", Socket: "/s/herdr.sock", WorkspaceID: "w2"}, false},
		{"no event", []string{"HERDR_SOCKET_PATH=/s/herdr.sock"}, Closed{}, true},
		{"no workspace id", []string{"HERDR_SOCKET_PATH=/s/herdr.sock", `HERDR_PLUGIN_EVENT_JSON={"data":{}}`}, Closed{}, true},
		{"no socket", []string{"HERDR_PLUGIN_EVENT_JSON=" + eventJSON}, Closed{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseClosed(tt.environ)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("ParseClosed = %+v, %v; want %+v, error %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// bound is one bound herdr session "work" with its store and a mapped workspace w2.
type bound struct {
	bindingsFile string
	socket       string
	ws           workspace.Session
	key          string // the key of w2
}

func newBound(t *testing.T) bound {
	t.Helper()
	home := t.TempDir()
	storeDir := filepath.Join(home, "store")
	sessionDir := filepath.Join(home, "herdr", "sessions", "work")
	root := filepath.Join(home, "project") // outside git: the root is the project folder
	for _, d := range []string{storeDir, sessionDir, root} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	b := bindings.Binding{Session: "work", Socket: filepath.Join(sessionDir, "herdr.sock"), Store: storeDir,
		Kind: bindings.KindMain, Created: testNow}
	bf := filepath.Join(home, "data", "bindings.json")
	if err := bindings.Write(bf, []bindings.Binding{b}); err != nil {
		t.Fatal(err)
	}
	session := `{"version":3,"workspaces":[{"id":"w2","identity_cwd":"` + root + `"}]}`
	if err := os.WriteFile(filepath.Join(sessionDir, "session.json"), []byte(session), 0o600); err != nil {
		t.Fatal(err)
	}
	st := openStore(t, storeDir)
	ws := workspace.For(b)
	if err := ws.Refresh(context.Background(), os.Environ(), st, []herdr.Workspace{{ID: "w2", Label: "B2", Number: 1}}, testNow); err != nil {
		t.Fatal(err)
	}
	m, err := ws.Lookup("w2")
	if err != nil || m.State != workspace.Mapped {
		t.Fatalf("Lookup(w2) = %+v, %v; want mapped", m, err)
	}
	return bound{bindingsFile: bf, socket: b.Socket, ws: ws, key: m.Entry.Key}
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return st
}

func (b bound) state(t *testing.T) workspace.State {
	t.Helper()
	m, err := b.ws.Lookup("w2")
	if err != nil {
		t.Fatal(err)
	}
	return m.State
}

// The close event of a bound session marks the pair closed and its row closed.
func TestWorkspaceClosed(t *testing.T) {
	b := newBound(t)
	env := []string{"HERDR_SESSION=work", "HERDR_SOCKET_PATH=" + b.socket, "HERDR_PLUGIN_EVENT_JSON=" + eventJSON}
	if err := WorkspaceClosed(context.Background(), env, b.bindingsFile); err != nil {
		t.Fatalf("WorkspaceClosed: %v", err)
	}
	if s := b.state(t); s != workspace.Unmapped {
		t.Errorf("Lookup(w2) = %q, want %q", s, workspace.Unmapped)
	}
	var row store.Workspace
	st := openStore(t, filepath.Dir(b.ws.MapFile))
	if err := st.Write(context.Background(), func(tx *sql.Tx) error {
		var err error
		row, err = store.WorkspaceByKey(context.Background(), tx, b.key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if row.Status != store.WorkspaceClosed {
		t.Errorf("row status = %q, want %q", row.Status, store.WorkspaceClosed)
	}
}

// An unbound session, a binding of another session name, or an unknown workspace changes
// nothing and is no error.
func TestWorkspaceClosedIgnored(t *testing.T) {
	b := newBound(t)
	other := filepath.Join(t.TempDir(), "herdr.sock")
	tests := []struct {
		name string
		env  []string
	}{
		{"unbound socket", []string{"HERDR_SESSION=work", "HERDR_SOCKET_PATH=" + other, "HERDR_PLUGIN_EVENT_JSON=" + eventJSON}},
		{"other session name", []string{"HERDR_SESSION=other", "HERDR_SOCKET_PATH=" + b.socket, "HERDR_PLUGIN_EVENT_JSON=" + eventJSON}},
		{"default session name", []string{"HERDR_SOCKET_PATH=" + b.socket, "HERDR_PLUGIN_EVENT_JSON=" + eventJSON}},
		{"unknown workspace", []string{"HERDR_SESSION=work", "HERDR_SOCKET_PATH=" + b.socket,
			`HERDR_PLUGIN_EVENT_JSON={"data":{"workspace_id":"w9"}}`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := WorkspaceClosed(context.Background(), tt.env, b.bindingsFile); err != nil {
				t.Fatalf("WorkspaceClosed: %v", err)
			}
			if s := b.state(t); s != workspace.Mapped {
				t.Errorf("Lookup(w2) = %q, want %q", s, workspace.Mapped)
			}
		})
	}
}

// No bindings file yet: no session is bound, so the event does nothing.
func TestWorkspaceClosedNoBindings(t *testing.T) {
	env := []string{"HERDR_SOCKET_PATH=/s/herdr.sock", "HERDR_PLUGIN_EVENT_JSON=" + eventJSON}
	if err := WorkspaceClosed(context.Background(), env, filepath.Join(t.TempDir(), "bindings.json")); err != nil {
		t.Errorf("WorkspaceClosed: %v", err)
	}
}

func TestMainUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"workspace-renamed"}, {"workspace-closed", "x"}} {
		if err := Main(context.Background(), args, nil); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("Main(%q) = %v, want a usage error", args, err)
		}
	}
}
