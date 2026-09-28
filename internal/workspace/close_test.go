package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

func closeWS(t *testing.T, s testSession, id string) {
	t.Helper()
	if err := s.Close(context.Background(), s.st, id); err != nil {
		t.Fatalf("Close(%s): %v", id, err)
	}
}

// D4: a closed pair is unmapped for the hook, and its rows are closed; the items stay.
func TestClosedPairUnmapped(t *testing.T) {
	repo := gitRepo(t)
	s := newSession(t, "default")
	save(t, s, map[string]string{"w4": repo})
	refresh(t, s, w("w4", "D"))
	old := mapped(t, s, "w4")

	closeWS(t, s, "w4")
	if m := lookup(t, s, "w4"); m.State != Unmapped {
		t.Errorf("Lookup(w4) after close = %q, want %q", m.State, Unmapped)
	}
	if r := row(t, s, old.Key); r.Status != store.WorkspaceClosed {
		t.Errorf("row status = %q, want %q", r.Status, store.WorkspaceClosed)
	}
	m, err := readMap(s.MapFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 1 || m.Entries[0].Key != old.Key || !m.Entries[0].Closed {
		t.Errorf("map entries = %+v, want the entry of %q kept and closed", m.Entries, old.Key)
	}
}

// D4: herdr reuses a closed id after a restart. The next brain call gives it a new key, also
// in the same project folder, so it never gets the items of the closed workspace (§11).
func TestReusedIDGetsNewKey(t *testing.T) {
	repo := gitRepo(t)
	s := newSession(t, "default")
	save(t, s, map[string]string{"w4": repo})
	refresh(t, s, w("w4", "D"))
	old := mapped(t, s, "w4")
	closeWS(t, s, "w4")

	refresh(t, s, w("w4", "E"))
	got := mapped(t, s, "w4")
	if got.Key == old.Key {
		t.Fatalf("reused w4 has the closed key %q, want a new key", old.Key)
	}
	if r := row(t, s, got.Key); r.Status != store.WorkspaceLive || r.Label != "E" {
		t.Errorf("new row = %+v, want live with label E", r)
	}
	if r := row(t, s, old.Key); r.Status != store.WorkspaceClosed || r.Label != "D" {
		t.Errorf("closed row = %+v, want closed with label D", r)
	}
}

// D4, E2-S4 open point: a close marks the remapped rows of the pair closed too. A later remap
// of the reused id does not offer the closed entries for a relink.
func TestCloseMarksRemappedRows(t *testing.T) {
	a, b := gitRepo(t), gitRepo(t)
	s := newSession(t, "default")
	save(t, s, map[string]string{"w1": a})
	refresh(t, s, w("w1", "brain"))
	first := mapped(t, s, "w1")
	save(t, s, map[string]string{"w1": b})
	r, err := s.Remap(deadline(t), os.Environ(), s.st, "w1", testNow)
	if err != nil {
		t.Fatalf("Remap: %v", err)
	}

	closeWS(t, s, "w1")
	for _, k := range []string{first.Key, r.New.Key} {
		if got := row(t, s, k); got.Status != store.WorkspaceClosed {
			t.Errorf("row %s status = %q, want %q", k, got.Status, store.WorkspaceClosed)
		}
	}

	save(t, s, map[string]string{"w1": a})
	refresh(t, s, w("w1", "next"))
	save(t, s, map[string]string{"w1": filepath.Join(b, "sub")})
	r2, err := s.Remap(deadline(t), os.Environ(), s.st, "w1", testNow)
	if err != nil {
		t.Fatalf("second Remap: %v", err)
	}
	if len(r2.Old) != 1 || r2.Old[0].Key == first.Key || r2.Old[0].Key == r.New.Key {
		t.Errorf("second Remap old = %+v, want only the entry of the reused w1", r2.Old)
	}
}

// An unknown pair, or a pair of another session, changes nothing.
func TestCloseUnknownPair(t *testing.T) {
	repo := gitRepo(t)
	a := newSession(t, "A")
	b := newSession(t, "B", a)
	save(t, a, map[string]string{"w1": repo})
	refresh(t, a, w("w1", "brain"))
	before, err := os.ReadFile(a.MapFile)
	if err != nil {
		t.Fatal(err)
	}

	closeWS(t, a, "w9")
	closeWS(t, b, "w1")
	after, err := os.ReadFile(a.MapFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("map changed:\n%s\nwant:\n%s", after, before)
	}
	mapped(t, a, "w1")
}

// A map file from before the closed flag still parses: the file stays version 1.
func TestMapWithoutClosedFlag(t *testing.T) {
	repo := gitRepo(t)
	s := newSession(t, "default")
	save(t, s, map[string]string{"w1": repo})
	writeFile(t, s.MapFile, `{"version":1,"entries":[{"key":"ws_old","session":"default","workspace_id":"w1","label":"x","project":"`+canon(t, repo)+`"}]}`)
	if got := mapped(t, s, "w1"); got.Key != "ws_old" {
		t.Errorf("key = %q, want ws_old", got.Key)
	}
}
