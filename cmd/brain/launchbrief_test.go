package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/hook"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/resolver"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// itemOf is the text of the workspace item of herdr workspace ws in TestLaunchBrief.
func itemOf(ws string) string { return "the workspace item of " + ws }

// addItems adds a global item and an item of each workspace of h, and builds the brief.
func (h *mcpHome) addItems(t *testing.T) {
	t.Helper()
	item := func(ctx context.Context, tx *sql.Tx, key *string, text string) error {
		r := store.ItemRevision{Text: text, Scope: store.ScopeGlobal, State: store.StateActive, Author: store.AuthorYou,
			Origin: store.OriginRemember, Created: time.Now()}
		if key != nil {
			r.Scope, r.WorkspaceKey = store.ScopeWorkspace, key
		}
		_, _, err := store.CreateItem(ctx, tx, store.Item{ClaimID: text, Kind: store.KindFact, Created: time.Now()}, r)
		return err
	}
	h.write(t, func(ctx context.Context, tx *sql.Tx) error {
		for _, ws := range []string{"w1", "w2"} {
			key := "ws_a" + ws[1:]
			if err := item(ctx, tx, &key, itemOf(ws)); err != nil {
				return err
			}
		}
		return item(ctx, tx, nil, "a global item")
	})
	h.build(t)
}

// startOut runs the start hook of the CLI kind in this process, with the env env and the start
// input, and returns its output: the hook of the launch folder dir, or of the env when dir is
// "". The start lies an hour ahead, so the hook cap and the lookup deadline do not cut a run of
// a race build or a busy machine: this check is of the brief, and the hook tests check the
// time limits.
func (h *launchHome) startOut(t *testing.T, kind, dir string, env []string, input string) string {
	t.Helper()
	p, err := adapter.Load(kind)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	hk := hook.Hook{Profile: p, Event: adapter.EventStart, Launch: dir, Environ: env,
		Bindings: filepath.Join(h.home, ".local", "share", "brain", "bindings.json"), Retest: filepath.Join(h.home, "retest"),
		Pane: resolver.HerdrPane(env), Start: time.Now().Add(time.Hour), Now: time.Now}
	if err := hk.Run(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("%s start hook (launch %q): %v", kind, dir, err)
	}
	return out.String()
}

// additionalContext returns the brief in start hook output out.
func additionalContext(t *testing.T, out string) string {
	t.Helper()
	var v struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("start hook output %q (%v), want a SessionStart brief", out, err)
	}
	return v.HookSpecificOutput.AdditionalContext
}

// flagValue returns the value of the last --name=value arg of args.
func flagValue(t *testing.T, args []string, name string) string {
	t.Helper()
	for _, a := range slices.Backward(args) {
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	t.Fatalf("args %q have no %s", args, name)
	return ""
}

// codexConfig decodes the -c values of args into one table, as Codex adds them to one layer.
func codexConfig(t *testing.T, args []string) (cfg map[string]any, overrides []string) {
	t.Helper()
	cfg = map[string]any{}
	for i, a := range args {
		if a != "-c" || i+1 == len(args) {
			continue
		}
		overrides = append(overrides, args[i+1])
		var v map[string]any
		if _, err := toml.Decode(args[i+1], &v); err != nil {
			t.Fatalf("-c %s: %v", args[i+1], err)
		}
		for k, x := range v {
			old, ok := cfg[k].(map[string]any)
			if sub, isMap := x.(map[string]any); ok && isMap {
				for sk, sx := range sub {
					old[sk] = sx
				}
				continue
			}
			cfg[k] = x
		}
	}
	return cfg, overrides
}

// startHook returns the command of Brain's start hook in the args of a launch of the CLI kind:
// for Claude from its --settings file, for Codex from its -c hooks. For Codex it also checks
// the hook entry: additionalContextLimit 0, and its trust in the one hooks.state table.
func startHook(t *testing.T, kind string, args []string) string {
	t.Helper()
	type group struct {
		Hooks []map[string]any `json:"hooks" toml:"hooks"`
	}
	if kind == "claude" {
		b, err := os.ReadFile(flagValue(t, args, "--settings"))
		if err != nil {
			t.Fatal(err)
		}
		var s struct {
			Hooks map[string][]group `json:"hooks"`
		}
		if err := json.Unmarshal(b, &s); err != nil || len(s.Hooks["SessionStart"]) != 1 {
			t.Fatalf("claude settings %s (%v), want one SessionStart group", b, err)
		}
		cmd, _ := s.Hooks["SessionStart"][0].Hooks[0]["command"].(string)
		return cmd
	}
	cfg, overrides := codexConfig(t, args)
	hooks, _ := cfg["hooks"].(map[string]any)
	groups, _ := hooks["SessionStart"].([]any)
	if len(groups) != 1 {
		t.Fatalf("codex -c hooks.SessionStart = %v, want one group", hooks["SessionStart"])
	}
	g, _ := groups[0].(map[string]any)
	hs, _ := g["hooks"].([]any)
	handler, _ := hs[0].(map[string]any)
	cmd, _ := handler["command"].(string)
	if handler["additionalContextLimit"] != int64(0) {
		t.Errorf("codex start hook %v, want additionalContextLimit 0", handler)
	}
	zero := uint64(0)
	_, sum, err := harness.CodexHookHash("SessionStart", nil, cmd, &zero)
	if err != nil {
		t.Fatal(err)
	}
	states := slices.DeleteFunc(slices.Clone(overrides), func(o string) bool { return !strings.HasPrefix(o, "hooks.state=") })
	state, _ := hooks["state"].(map[string]any)
	entry, _ := state["/<session-flags>/config.toml:session_start:0:0"].(map[string]any)
	if len(states) != 1 || entry["trusted_hash"] != sum {
		t.Errorf("codex hooks.state = %q, want one table that trusts the start hook with %s", states, sum)
	}
	for _, o := range overrides {
		if strings.Contains(o, "developer_instructions") {
			t.Errorf("codex -c %s: want no developer_instructions override", o)
		}
	}
	return cmd
}

// launchReceipts returns the number of receipts of each launch record in the store of h.
func (h *launchHome) launchReceipts(t *testing.T) map[int64]int {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, h.store, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	n := map[int64]int{}
	err = st.Write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT launch_id, count(*) FROM deliveries WHERE launch_id IS NOT NULL GROUP BY 1`)
		if err != nil {
			return err
		}
		defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
		for rows.Next() {
			var id int64
			var c int
			if err := rows.Scan(&id, &c); err != nil {
				return err
			}
			n[id] = c
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// launchedBrief checks that the args of launch record id of the CLI kind have Brain's start
// hook of the launch, runs it with the env env and the start input, and returns the brief that
// the CLI got: for Claude the brief.md file of its --append-system-prompt-file flag, for Codex
// the hook output.
func (h *launchHome) launchedBrief(t *testing.T, kind string, id int64, args, env []string, input string) string {
	t.Helper()
	if got, want := startHook(t, kind, args), h.hookCommand(t, kind, "start", id); got != want {
		t.Errorf("%s start hook %q, want %q", kind, got, want)
	}
	out := h.startOut(t, kind, paths.Launch(h.store, id), env, input)
	if kind == "codex" {
		return additionalContext(t, out)
	}
	if out != "" {
		t.Errorf("claude start hook of the launch printed %q, want nothing", out)
	}
	if flagValue(t, args, "--system-prompt-snapshot") != "off" {
		t.Errorf("claude args %q, want --system-prompt-snapshot=off", args)
	}
	b, err := os.ReadFile(flagValue(t, args, "--append-system-prompt-file"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// AC #444, end to end with the brain binary and the fake CLIs: for the same workspace, the
// fake CLI gets the same brief text as the current start hook `brain hook --profile <kind>
// --event start` gives: claude in the brief.md file of its --append-system-prompt-file flag
// with the system prompt snapshot off, codex in the output of its -c SessionStart hook, run in
// an env with no herdr variables, as a shared Codex app server runs it. The hooks run in this
// process with no time limit (startOut). Codex gets no
// developer_instructions override, and the trust of the start hook is in the one hooks.state
// table. Two panes in two workspaces each get the brief of their own workspace. Each launch
// writes one receipt with its launch record ID; the Claude start hook prints nothing.
func TestLaunchBrief(t *testing.T) {
	h := newMCPHome(t)
	h.addItems(t)
	before := h.configs(t)
	appServer := []string{"HOME=" + h.home, "PATH=/usr/bin:/bin"}
	var launches int64
	for _, kind := range []string{"claude", "codex"} {
		for _, ws := range []string{"w1", "w2"} {
			pane := ws + ":p1"
			env := with(h.env, "HERDR_PANE_ID="+pane, "HERDR_WORKSPACE_ID="+ws)
			input := `{"session_id":"` + kind + "-" + ws + `","cwd":"` + h.home + `"}`
			want := additionalContext(t, h.startOut(t, kind, "", env, input))
			other := map[string]string{"w1": "w2", "w2": "w1"}[ws]
			if !strings.Contains(want, itemOf(ws)) || strings.Contains(want, itemOf(other)) {
				t.Fatalf("start hook brief of %s = %q, want the item of %s only", pane, want, ws)
			}
			if stderr := h.brain(t, env, kind, "hi"); stderr != "" {
				t.Errorf("brain %s in %s: stderr %q, want none", kind, pane, stderr)
			}
			launches++
			// The fake CLI ended already, but a real CLI runs while it runs its start hook: the
			// launch check refuses a launch whose process is gone.
			h.write(t, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE launches SET pid = ? WHERE id = ?`, os.Getpid(), launches)
				return err
			})
			if got := h.launchedBrief(t, kind, launches, h.logged(t, kind), appServer, input); got != want {
				t.Errorf("brain %s in %s: the CLI got the brief %q, want the brief of the start hook %q", kind, pane, got, want)
			}
			if n := h.launchReceipts(t); n[launches] != 1 {
				t.Errorf("brain %s in %s: receipts of each launch = %v, want 1 for launch %d", kind, pane, n, launches)
			}
		}
	}
	if after := h.configs(t); !maps.Equal(after, before) {
		t.Errorf("the files under .claude and .codex changed:\n%v\nwant\n%v", after, before)
	}
}

// Review of #444: brain forget removes the text of the item from the brief.md of a Claude
// launch too, and reports done only then.
func TestLaunchForget(t *testing.T) {
	h := newMCPHome(t)
	h.addItems(t)
	h.brain(t, h.env, "claude", "hi")
	path := flagValue(t, h.logged(t, "claude"), "--append-system-prompt-file")
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), itemOf("w1")) {
		t.Fatalf("brief.md = %q, %v, want the item of w1", b, err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "forget", "1")
	cmd.Env, cmd.Dir = h.env, h.home
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "forget item 1: done") {
		t.Fatalf("brain forget 1 = %s, %v, want done", out, err)
	}
	if b, err := os.ReadFile(path); err != nil || strings.Contains(string(b), itemOf("w1")) {
		t.Errorf("brief.md after brain forget 1 = %q, %v, want no text of item 1", b, err)
	}
}
