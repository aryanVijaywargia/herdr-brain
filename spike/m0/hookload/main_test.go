package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenBriefSize(t *testing.T) {
	store := t.TempDir()
	if err := gen(store, defaultKey); err != nil {
		t.Fatal(err)
	}
	var rec record
	brief, _, err := readBrief(store, defaultKey, &rec)
	if err != nil {
		t.Fatalf("readBrief = %v, want nil", err)
	}
	if len(brief) != briefBudget {
		t.Errorf("brief length = %d, want %d", len(brief), briefBudget)
	}
	if !strings.HasPrefix(brief, beginMarker) || !strings.HasSuffix(brief, endMarker) {
		t.Errorf("brief does not start and end with the markers")
	}
}

// TestReadBriefWithholds breaks one thing in a good generation for each case.
func TestReadBriefWithholds(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func(t *testing.T, store string)
	}{
		{"no pointer", func(t *testing.T, s string) { remove(t, s, "out/current") }},
		{"bad pointer", func(t *testing.T, s string) { write(t, s, "out/current", "../gen-1\n") }},
		{"pointer to missing gen", func(t *testing.T, s string) { write(t, s, "out/current", "gen-2\n") }},
		{"no manifest", func(t *testing.T, s string) { remove(t, s, "out/gen-1/manifest.json") }},
		{"manifest not json", func(t *testing.T, s string) { write(t, s, "out/gen-1/manifest.json", "{") }},
		{"manifest other gen", func(t *testing.T, s string) {
			editManifest(t, s, func(m *manifest) { m.Generation = 7 })
		}},
		{"part not in manifest", func(t *testing.T, s string) {
			editManifest(t, s, func(m *manifest) { m.Files = m.Files[:1] })
		}},
		{"duplicate path", func(t *testing.T, s string) {
			editManifest(t, s, func(m *manifest) { m.Files = append(m.Files, m.Files[0]) })
		}},
		{"escaping path", func(t *testing.T, s string) {
			editManifest(t, s, func(m *manifest) { m.Files[0].Path = "../global.md" })
		}},
		{"missing part", func(t *testing.T, s string) { remove(t, s, "out/gen-1/global.md") }},
		{"edited part", func(t *testing.T, s string) {
			write(t, s, "out/gen-1/workspaces/"+defaultKey+".md", "hand edit")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := t.TempDir()
			if err := gen(store, defaultKey); err != nil {
				t.Fatal(err)
			}
			c.corrupt(t, store)
			var rec record
			if _, _, err := readBrief(store, defaultKey, &rec); !errors.Is(err, errWithheld) {
				t.Errorf("readBrief = %v, want %v", err, errWithheld)
			}
		})
	}
}

func TestHookOutsideHerdrPrintsNothing(t *testing.T) {
	var out bytes.Buffer
	env := map[string]string{"HOOKLOAD_STORE": t.TempDir()}
	hook(strings.NewReader(sampleInput), &out, func(k string) string { return env[k] })
	if out.Len() != 0 {
		t.Errorf("hook outside herdr printed %q, want nothing", out.String())
	}
}

func TestRank(t *testing.T) {
	v := make([]float64, 20)
	for i := range v {
		v[i] = float64(i + 1)
	}
	for _, c := range []struct{ p, want float64 }{{50, 10}, {95, 19}, {100, 20}, {1, 1}} {
		if got := rank(v, c.p); got != c.want {
			t.Errorf("rank(1..20, %v) = %v, want %v", c.p, got, c.want)
		}
	}
}

func write(t *testing.T, store, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(store, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func remove(t *testing.T, store, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(store, rel)); err != nil {
		t.Fatal(err)
	}
}

func editManifest(t *testing.T, store string, edit func(*manifest)) {
	t.Helper()
	file := filepath.Join(store, "out/gen-1/manifest.json")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	edit(&m)
	if b, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	write(t, store, "out/gen-1/manifest.json", string(b))
}
