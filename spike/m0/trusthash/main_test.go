package main

import (
	"os"
	"path/filepath"
	"testing"
)

func ptr[T any](v T) *T { return &v }

// Codex 0.154 wrote the golden values (spike/m0/data/e8s3-trust-hash.txt) to
// ~/.codex/config.toml when the owner trusted these hooks in the TUI. The home
// path in the commands is now /Users/me, so the want values are those of the
// same hookHash for the new commands.
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
			h:     handler{Command: "/Users/me/Projects/herdr-brain/spike/m0/hook/brainspike codex", Timeout: ptr[uint64](5)},
			want:  "sha256:4fed4ee72a2f82fa75f16a67f8c029472a60db04e4f514cad71688593c7fec5f",
		},
		{
			name:  "hooks.json session start, no matcher",
			event: "SessionStart",
			h:     handler{Command: "bash '/Users/me/.codex/herdr-agent-state.sh' session", Timeout: ptr[uint64](10)},
			want:  "sha256:5f6e3cf290a1a1d3bb540c44b4e67ea428b1b011a759d70107ff33d9b29aa31f",
		},
		{
			name:    "hooks.json subagent start, matcher, default timeout",
			event:   "SubagentStart",
			matcher: ptr("tldraw-offline"),
			h:       handler{Command: "sh '/Users/me/skills/tldraw-offline/inject-server-context.sh' SubagentStart"},
			want:    "sha256:9f0508aa58e15ee9d34edd5d468d37aed5d5baf373b75883eb09fc497ba486eb",
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
