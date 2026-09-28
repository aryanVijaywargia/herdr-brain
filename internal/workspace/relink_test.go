package workspace

import (
	"path/filepath"
	"slices"
	"testing"
)

// S83, S20: a replaced entry and a closed entry with the project of a current entry wait for a
// relink. Entries of another herdr session and a closed entry with no current project do not.
func TestRelinks(t *testing.T) {
	s := Session{Name: "default", MapFile: filepath.Join(t.TempDir(), "workspaces.json")}
	e := func(key, session, project string) Entry {
		return Entry{Key: key, Session: session, WorkspaceID: key, Label: key, Project: project}
	}
	cur, replaced, closed, alone, other := e("k1", "default", "/p"), e("k2", "default", "/p"),
		e("k3", "default", "/p"), e("k4", "default", "/q"), e("k5", "t1", "/p")
	replaced.ReplacedBy = "k1"
	closed.Closed, alone.Closed, other.Closed = true, true, true
	if err := writeMap(s.MapFile, &mapFile{Version: 1, Entries: []Entry{cur, replaced, closed, alone, other}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Relinks()
	if err != nil {
		t.Fatal(err)
	}
	want := []Relink{{Old: replaced, New: cur}, {Old: closed, New: cur}}
	if !slices.Equal(got, want) {
		t.Errorf("Relinks() = %+v, want %+v", got, want)
	}
}
