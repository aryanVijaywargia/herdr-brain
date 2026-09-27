package main

import (
	"os"
	"path/filepath"
	"testing"
)

func ptr[T any](v T) *T { return &v }

// The want values are trusted_hash values that Codex 0.154 wrote to
// ~/.codex/config.toml when the owner trusted these hooks in the TUI.
func TestHookHashMatchesCodex(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		matcher *string
		h       handler
		want    string
	}{
		{
			name:  "M0 inline session flags hook",
			event: "SessionStart",
			h:     handler{Command: "/Users/aryanvijaywargia/Projects/herdr-brain/spike/m0/hook/brainspike codex", Timeout: ptr[uint64](5)},
			want:  "sha256:e13f3a03acf65bd0d2f320a9d8879834e42e07d4b30f5954058c1aa9c22f28c7",
		},
		{
			name:  "hooks.json session start, no matcher",
			event: "SessionStart",
			h:     handler{Command: "bash '/Users/aryanvijaywargia/.codex/herdr-agent-state.sh' session", Timeout: ptr[uint64](10)},
			want:  "sha256:9b6563c12756ab055c0b4a03352e4016706dd0f8a43d2e5cbcf1a84241a84d01",
		},
		{
			name:    "hooks.json subagent start, matcher, default timeout",
			event:   "SubagentStart",
			matcher: ptr("tldraw-offline"),
			h:       handler{Command: "sh '/Users/aryanvijaywargia/skills/tldraw-offline/inject-server-context.sh' SubagentStart"},
			want:    "sha256:a7e36c07775b97099838895ecb15743029e4c59ce6144ae67a2e98ccc2b306b3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.h.Type = "command"
			got, err := hookHash(tt.event, eventLabels[tt.event], tt.matcher, tt.h)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("hash = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestHookHashChangesWithCommand(t *testing.T) {
	a, _ := hookHash("SessionStart", "session_start", nil, handler{Command: "brain hook", Timeout: ptr[uint64](5)})
	b, _ := hookHash("SessionStart", "session_start", nil, handler{Command: "brain hook ", Timeout: ptr[uint64](5)})
	c, _ := hookHash("SessionStart", "session_start", nil, handler{Command: "brain hook", Timeout: ptr[uint64](6)})
	if a == b || a == c {
		t.Errorf("hash must change with command and timeout: %s %s %s", a, b, c)
	}
}

func TestKeyUsesPositionAndReadsOnlyHookState(t *testing.T) {
	dir := t.TempDir()
	hooks := filepath.Join(dir, "hooks.json")
	cfg := filepath.Join(dir, "config.toml")
	events := map[string][]group{"SessionStart": {
		{Hooks: []handler{{Type: "command", Command: "other"}}},
		{Hooks: []handler{{Type: "command", Command: "brain hook", Timeout: ptr[uint64](5)}}},
	}}
	entries, err := entriesFor(hooks, events)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].Key != hooks+":session_start:1:0" {
		t.Fatalf("entries = %+v", entries)
	}
	toml := "model = \"x\"\ntrusted_hash = \"not-state\"\n\n[hooks.state.\"" + entries[1].Key + "\"]\ntrusted_hash = \"" + entries[1].Hash + "\"\n\n[other]\ntrusted_hash = \"no\"\n"
	if err := os.WriteFile(cfg, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := readTrustedHashes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 1 || state[entries[1].Key] != entries[1].Hash {
		t.Errorf("state = %v", state)
	}
}
