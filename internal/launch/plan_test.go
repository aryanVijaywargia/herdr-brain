package launch

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
)

// testPlan has each part that E24-S4, S5 and S6 add: hooks with and without a matcher, two
// hooks of one event, an MCP server and a brief. The command has a quote and a backslash.
func testPlan() Plan {
	return Plan{
		Hooks: []Hook{
			{Event: "SessionStart", Command: `/opt/brain hook --launch "/l/1" \x`},
			{Event: "PostToolUse", Matcher: "Bash", Command: "/opt/brain hook tool"},
			{Event: "SessionStart", Command: "/opt/brain hook start2"},
			{Event: "Stop", Command: "/opt/brain hook stop"},
		},
		MCP:   &Server{Command: "/opt/brain", Args: []string{"mcp", "--launch", "/l/1"}, EnvVars: []string{"HERDR_ENV", "HERDR_PANE_ID"}},
		Brief: "BRIEF\n",
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return v
}

// checkModes checks that each file of the launch folder dir has mode 0600.
func checkModes(t *testing.T, dir string, want int) {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != want {
		t.Errorf("launch folder has %d files, want %d", len(es), want)
	}
	for _, e := range es {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode(); got != fs.FileMode(0o600) {
			t.Errorf("mode of %s = %v, want -rw-------", e.Name(), got)
		}
	}
}

// The Claude --settings merge (E24-S1 Q7): the user's settings file stays, the Brain hooks come
// after the user's hooks of an event, the allow rule joins the user's rules, and the user's
// flag is dropped. The user's append file and the brief make one file; a user snapshot value
// stays and Brain drops its off with a note. The user's other args stay in their order.
func TestBuildClaudeMerge(t *testing.T) {
	work := t.TempDir()
	userSettings := filepath.Join(work, "user.json")
	if err := os.WriteFile(userSettings, []byte(`{"model":"opus","n":1.50,
		"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"user-start"}]}]},
		"permissions":{"allow":["Bash(ls)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	userBrief := filepath.Join(work, "user.md")
	if err := os.WriteFile(userBrief, []byte("USER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	args := []string{"--settings", userSettings, "--model", "opus", "--append-system-prompt-file=" + userBrief,
		"--system-prompt-snapshot", "on", "fix it"}
	got, notes, err := parse(mustLoad(t, "claude"), args).argv(dir, testPlan())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--settings=" + filepath.Join(dir, "settings.json"), "--mcp-config=" + filepath.Join(dir, "mcp.json"),
		"--append-system-prompt-file=" + filepath.Join(dir, "brief.md"), "--model", "opus", "--system-prompt-snapshot", "on", "fix it"}
	if !slices.Equal(got, want) {
		t.Errorf("argv() =\n%q\nwant\n%q", got, want)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "--system-prompt-snapshot") {
		t.Errorf("notes = %q, want the snapshot note", notes)
	}
	s := readJSON(t, filepath.Join(dir, "settings.json"))
	wantSettings := map[string]any{
		"model": "opus", "n": 1.5,
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "user-start"}}},
				map[string]any{"hooks": []any{map[string]any{"type": "command", "command": `/opt/brain hook --launch "/l/1" \x`}}},
				map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/opt/brain hook start2"}}},
			},
			"PostToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": "/opt/brain hook tool"}}}},
			"Stop":        []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/opt/brain hook stop"}}}},
		},
		"permissions": map[string]any{"allow": []any{"Bash(ls)", "mcp__brain"}},
	}
	if !reflect.DeepEqual(s, wantSettings) {
		t.Errorf("settings.json = %v\nwant %v", s, wantSettings)
	}
	mcp := readJSON(t, filepath.Join(dir, "mcp.json"))
	wantMCP := map[string]any{"mcpServers": map[string]any{"brain": map[string]any{
		"type": "stdio", "command": "/opt/brain", "args": []any{"mcp", "--launch", "/l/1"}}}}
	if !reflect.DeepEqual(mcp, wantMCP) {
		t.Errorf("mcp.json = %v, want %v", mcp, wantMCP)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "brief.md")); err != nil || string(b) != "USER\n\nBRIEF\n" {
		t.Errorf("brief.md = %q, %v, want the user's text, a blank line and the brief", b, err)
	}
	checkModes(t, dir, 3)
	if b, err := os.ReadFile(userSettings); err != nil || !strings.HasPrefix(string(b), `{"model":"opus"`) {
		t.Errorf("the user's settings file changed: %q, %v", b, err)
	}
}

// A --settings JSON text merges as a file does; with no user flag, Brain's own snapshot off
// comes after the brief; a user --settings of the wrong shape is an error.
func TestBuildClaudeSettings(t *testing.T) {
	d := mustLoad(t, "claude")
	dir := t.TempDir()
	p := Plan{Hooks: testPlan().Hooks[:1], Brief: "B"}
	got, _, err := parse(d, []string{"--settings={\"env\":{\"A\":\"1\"}}", "hi"}).argv(dir, p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--settings=" + filepath.Join(dir, "settings.json"), "--append-system-prompt-file=" + filepath.Join(dir, "brief.md"),
		"--system-prompt-snapshot=off", "hi"}
	if !slices.Equal(got, want) {
		t.Errorf("argv() = %q, want %q", got, want)
	}
	s := readJSON(t, filepath.Join(dir, "settings.json"))
	if s["env"] == nil || s["permissions"] != nil || s["hooks"] == nil {
		t.Errorf("settings.json = %v, want the user's env and the Brain hook, no allow rule with no server", s)
	}
	for _, bad := range []string{`{"hooks":[]}`, `{"hooks":{"SessionStart":{}}}`, `[1]`, filepath.Join(dir, "nope.json"),
		`{"model":"opus"} trailing`, `{"model":"opus"} {"disableAllHooks":true}`} {
		if _, _, err := parse(d, []string{"--settings", bad}).argv(t.TempDir(), p); err == nil {
			t.Errorf("--settings %s: argv() = nil error", bad)
		}
	}
}

// The Codex flags (E24-S1 Q4, Q5 and its fact-check): each -c value is TOML, the hooks of one
// event are one array in the order of the plan, and one hooks.state table holds the trusted
// hash of each group under its key, with the hash rule of internal/harness.
func TestBuildCodex(t *testing.T) {
	dir := t.TempDir()
	p := testPlan()
	got, _, err := parse(mustLoad(t, "codex"), []string{"resume", "--last"}).argv(dir, p)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "--no-daemon" || !slices.Equal(got[len(got)-2:], []string{"resume", "--last"}) {
		t.Fatalf("argv() = %q, want --no-daemon first and the user's args last", got)
	}
	cfg := decodeConfig(t, got[1:len(got)-2])
	wantServer := map[string]any{"command": "/opt/brain", "args": []any{"mcp", "--launch", "/l/1"}, "env_vars": []any{"HERDR_ENV", "HERDR_PANE_ID"}}
	if s := obj(t, cfg, "mcp_servers")["brain"]; !reflect.DeepEqual(s, wantServer) {
		t.Errorf("mcp_servers.brain = %v, want %v", s, wantServer)
	}
	hooks := obj(t, cfg, "hooks")
	state := obj(t, hooks, "state")
	index := map[string]int{}
	for _, h := range p.Hooks {
		var m *string
		if h.Matcher != "" {
			m = &h.Matcher
		}
		limit := uint64(0)
		label, sum, err := harness.CodexHookHash(h.Event, m, h.Command, &limit)
		if err != nil {
			t.Fatal(err)
		}
		gi := index[h.Event]
		index[h.Event]++
		key := fmt.Sprintf("/<session-flags>/config.toml:%s:%d:0", label, gi)
		if e, _ := state[key].(map[string]any); e["trusted_hash"] != sum {
			t.Errorf("hooks.state[%q] = %v, want trusted_hash %s", key, state[key], sum)
		}
		g, _ := item(t, hooks[h.Event], gi).(map[string]any)
		handler, _ := item(t, g["hooks"], 0).(map[string]any)
		if handler["command"] != h.Command || handler["type"] != "command" || handler["additionalContextLimit"] != int64(0) ||
			h.Matcher != "" && g["matcher"] != h.Matcher {
			t.Errorf("hooks.%s[%d] = %v, want %+v", h.Event, gi, g, h)
		}
	}
	if len(state) != len(p.Hooks) {
		t.Errorf("hooks.state = %v, want one entry for each hook", state)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "brief.md")); err != nil || string(b) != p.Brief {
		t.Errorf("brief.md = %q, %v, want %q", b, err, p.Brief)
	}
	checkModes(t, dir, 1)
}

// decodeConfig decodes the pairs "-c", "<key>=<TOML value>" of flags into one table, as Codex
// adds -c values to one layer. It fails unless exactly one flag sets hooks.state.
func decodeConfig(t *testing.T, flags []string) map[string]any {
	t.Helper()
	cfg := map[string]any{}
	states := 0
	for i := 0; i < len(flags); i += 2 {
		if flags[i] != "-c" || i+1 == len(flags) {
			t.Fatalf("flags[%d:] = %q, want -c and a value", i, flags[i:])
		}
		if strings.HasPrefix(flags[i+1], "hooks.state=") {
			states++
		}
		var v map[string]any
		if _, err := toml.Decode(flags[i+1], &v); err != nil {
			t.Fatalf("-c %s: %v", flags[i+1], err)
		}
		merge(cfg, v)
	}
	if states != 1 {
		t.Errorf("flags have %d hooks.state values, want 1", states)
	}
	return cfg
}

// obj returns the table at key of m.
func obj(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	o, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %v, want a table", key, m[key])
	}
	return o
}

// item returns item i of the array v.
func item(t *testing.T, v any, i int) any {
	t.Helper()
	a, ok := v.([]any)
	if !ok || i >= len(a) {
		t.Fatalf("%v has no item %d", v, i)
	}
	return a[i]
}

// merge adds the TOML tables of v to cfg, key by key, as Codex adds -c values to one layer.
func merge(cfg, v map[string]any) {
	for k, x := range v {
		sub, ok := x.(map[string]any)
		old, isMap := cfg[k].(map[string]any)
		if ok && isMap {
			merge(old, sub)
			continue
		}
		cfg[k] = x
	}
}

func TestTOMLString(t *testing.T) {
	for _, s := range []string{"", `a"b\c`, "tab\tnew\nline\x7f\x01", "ü €"} {
		var v struct{ K string }
		if _, err := toml.Decode("K = "+tomlString(s), &v); err != nil || v.K != s {
			t.Errorf("tomlString(%q) = %s: decodes to %q, %v", s, tomlString(s), v.K, err)
		}
	}
}
