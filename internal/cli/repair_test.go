package cli

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

func (f *fixture) repair(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Repair(context.Background(), f.env(f.sock, "w14:p1", "w14", &out), args)
	return out.String(), err
}

func (f *fixture) mustRepair(t *testing.T, args ...string) string {
	t.Helper()
	out, err := f.repair(t, args...)
	if err != nil {
		t.Fatalf("Repair(%q) = %v", args, err)
	}
	return out
}

// genDir is the folder of output generation gen.
func (f *fixture) genDir(gen int64) string {
	return filepath.Join(f.store, outDir, "gen-"+strconv.FormatInt(gen, 10))
}

// handEdit writes "mine" over the global part of generation gen.
func (f *fixture) handEdit(t *testing.T, gen int64) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.genDir(gen), "global.md"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) conflicts(t *testing.T) []output.ConflictError {
	t.Helper()
	cs, err := output.Conflicts(filepath.Join(f.store, outDir))
	if err != nil {
		t.Fatalf("Conflicts() = %v", err)
	}
	return cs
}

// AC: a hand edit in the current generation. brain repair reports it and changes nothing;
// brain repair --confirm deletes the folder and builds a new generation that matches its
// manifest.
func TestRepairCurrentConflict(t *testing.T) {
	f := newFixture(t)
	f.handEdit(t, 1)

	out := f.mustRepair(t)
	if !strings.Contains(out, "conflict: output generation 1 changed outside Brain: global.md") {
		t.Errorf("Repair() = %q, want the conflict", out)
	}
	if b, err := os.ReadFile(filepath.Join(f.genDir(1), "global.md")); err != nil || string(b) != "mine" {
		t.Errorf("global.md after Repair() = %q, %v, want your edit kept", b, err)
	}
	if len(f.conflicts(t)) != 1 {
		t.Errorf("Conflicts() after Repair() = %v, want the conflict kept", f.conflicts(t))
	}

	out = f.mustRepair(t, "--confirm")
	if !strings.Contains(out, "deleted: output generation 1 changed outside Brain") {
		t.Errorf("Repair(--confirm) = %q, want the deleted conflict", out)
	}
	if _, err := os.Stat(f.genDir(1)); !os.IsNotExist(err) {
		t.Errorf("generation 1 after --confirm: %v, want it gone", err)
	}
	// Read checks each file against the manifest of the generation that the pointer names.
	b, err := output.Read(filepath.Join(f.store, outDir), "ws_a14")
	if err != nil || b.Generation <= 1 || !strings.Contains(b.Global, liveText) {
		t.Errorf("output.Read() = generation %d, %q, %v; want a new generation with %q", b.Generation, b.Global, err, liveText)
	}
	if cs := f.conflicts(t); len(cs) != 0 {
		t.Errorf("Conflicts() after --confirm = %v, want none", cs)
	}
}

// AC #119: brain repair shows each conflict once, on stdout and stderr together.
func TestRepairShowsConflictOnce(t *testing.T) {
	f := newFixture(t)
	f.handEdit(t, 1)
	// A commit with no build: the output is behind, so the repair tries to build and meets the
	// conflict.
	s := openStore(t, f.store)
	write(t, s, func(tx *sql.Tx) error {
		_, _, err := store.CreateItem(context.Background(), tx, store.Item{ClaimID: "c-new", Kind: store.KindFact, Created: testNow},
			store.ItemRevision{Text: "new item", Scope: store.ScopeGlobal, State: store.StateActive,
				Author: store.AuthorYou, Origin: store.OriginRemember, Created: testNow})
		return err
	})
	closeStore(t, s)
	var stdout, stderr bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &stdout)
	e.Stderr = &stderr
	if err := Repair(context.Background(), e, nil); err != nil {
		t.Fatalf("Repair() = %v", err)
	}
	const conflict = "output generation 1 changed outside Brain"
	if n := strings.Count(stdout.String()+stderr.String(), conflict); n != 1 {
		t.Errorf("Repair() shows the conflict %d times, want 1\nstdout: %q\nstderr: %q", n, stdout.String(), stderr.String())
	}
}

// AC: a hand edit in a retired generation that cleanup kept: --confirm deletes the folder.
func TestRepairRetiredConflict(t *testing.T) {
	f := newFixture(t)
	f.addActive(t, nil, "second item")
	f.handEdit(t, 1)
	if len(f.conflicts(t)) != 1 {
		t.Fatalf("Conflicts() = %v, want generation 1", f.conflicts(t))
	}

	out := f.mustRepair(t, "--confirm")
	if !strings.Contains(out, "deleted: output generation 1") {
		t.Errorf("Repair(--confirm) = %q, want the deleted conflict", out)
	}
	if _, err := os.Stat(f.genDir(1)); !os.IsNotExist(err) {
		t.Errorf("generation 1 after --confirm: %v, want it gone", err)
	}
	if cs := f.conflicts(t); len(cs) != 0 {
		t.Errorf("Conflicts() after --confirm = %v, want none", cs)
	}
	if b, err := output.Read(filepath.Join(f.store, outDir), "ws_a14"); err != nil || b.Generation != 2 {
		t.Errorf("output.Read() = generation %d, %v; want generation 2 kept", b.Generation, err)
	}
}

// AC: with no conflict, --confirm is the normal repair and says so.
func TestRepairNoConflict(t *testing.T) {
	f := newFixture(t)
	for _, args := range [][]string{nil, {"--confirm"}} {
		if out := f.mustRepair(t, args...); !strings.Contains(out, "no output conflict") {
			t.Errorf("Repair(%q) = %q, want no conflict", args, out)
		}
	}
	if _, err := os.Stat(f.genDir(1)); err != nil {
		t.Errorf("generation 1 after a repair with no conflict: %v, want it kept", err)
	}
}

func TestRepairErrors(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"x"}, "want no arguments"},
		{[]string{"--nope"}, "flag provided but not defined"},
	} {
		if _, err := f.repair(t, c.args...); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("Repair(%q) = %v, want %q", c.args, err, c.wantErr)
		}
	}
}
