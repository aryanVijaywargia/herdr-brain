package adapter

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

// The Claude Code 2.1.284 payloads of the E11-S1 test (M0 test E11-S1),
// with the fields that Brain does not read cut out.
const (
	claudePrompt = `{"session_id":"88ce9f75-8b96-40bb-8ff5-7f9fd3aa5071","cwd":"/private/tmp/e11s1/agent",
		"hook_event_name":"UserPromptSubmit","prompt":"Reply with exactly 120 lines of plain text and nothing else."}`
	claudeTool = `{"session_id":"4e5d3e1a-2058-4cbd-a185-fc7b488a25ac","cwd":"/private/tmp/e11s1/agent/sub",
		"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"cd sub && sleep 3 && pwd",
		"description":"Change to sub directory"},"tool_response":{"stdout":"/private/tmp/e11s1/agent/sub","stderr":""}}`
	claudeToolFailure = `{"session_id":"4e5d3e1a-2058-4cbd-a185-fc7b488a25ac","cwd":"/private/tmp/e11s1/agent/sub",
		"hook_event_name":"PostToolUseFailure","tool_name":"Bash","tool_input":{"command":"ls nosuchfile-zq"},
		"error":"Exit code 1\nls: nosuchfile-zq: No such file or directory","is_interrupt":false}`
	claudeStop = `{"session_id":"88ce9f75-8b96-40bb-8ff5-7f9fd3aa5071","cwd":"/private/tmp/e11s1/agent",
		"hook_event_name":"Stop","stop_hook_active":false,"last_assistant_message":"ROW 1 qv-5178\nROW 2 qv-5178"}`
)

func TestParseRecord(t *testing.T) {
	p, err := Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	one := 1
	for _, tc := range []struct {
		name, event, raw string
		want             Record
	}{
		{"prompt", EventPrompt, claudePrompt, Record{SessionID: "88ce9f75-8b96-40bb-8ff5-7f9fd3aa5071",
			CWD: "/private/tmp/e11s1/agent", Text: "Reply with exactly 120 lines of plain text and nothing else."}},
		// PostToolUse has no exit status.
		{"tool", EventTool, claudeTool, Record{SessionID: "4e5d3e1a-2058-4cbd-a185-fc7b488a25ac",
			CWD: "/private/tmp/e11s1/agent/sub", Command: "cd sub && sleep 3 && pwd"}},
		{"tool failure", EventTool, claudeToolFailure, Record{SessionID: "4e5d3e1a-2058-4cbd-a185-fc7b488a25ac",
			CWD: "/private/tmp/e11s1/agent/sub", Command: "ls nosuchfile-zq", ExitStatus: &one}},
		{"stop", EventStop, claudeStop, Record{SessionID: "88ce9f75-8b96-40bb-8ff5-7f9fd3aa5071",
			CWD: "/private/tmp/e11s1/agent", Text: "ROW 1 qv-5178\nROW 2 qv-5178"}},
		// A Codex reply can be null (M0 test E11-S1).
		{"null reply", EventStop, `{"session_id":"s","last_assistant_message":null}`, Record{SessionID: "s"}},
		// An error of another shape gives no exit status.
		{"other error", EventTool, `{"session_id":"s","tool_input":{"command":"ls"},"error":"Interrupted"}`,
			Record{SessionID: "s", Command: "ls"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.ParseRecord(tc.event, []byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if got.SessionID != tc.want.SessionID || got.CWD != tc.want.CWD || got.Text != tc.want.Text ||
				got.Command != tc.want.Command || (got.ExitStatus == nil) != (tc.want.ExitStatus == nil) ||
				got.ExitStatus != nil && *got.ExitStatus != *tc.want.ExitStatus {
				t.Errorf("ParseRecord = %+v, want %+v", got, tc.want)
			}
		})
	}
	for _, raw := range []string{`[]`, `{"cwd":"/x","prompt":"p"}`, `{"session_id":"s","prompt":7}`} {
		if _, err := p.ParseRecord(EventPrompt, []byte(raw)); !errors.Is(err, ErrRetest) {
			t.Errorf("ParseRecord(%s) = %v, want ErrRetest", raw, err)
		}
	}
	if _, err := p.ParseRecord(EventTool, []byte(`{"session_id":"s","tool_input":{}}`)); !errors.Is(err, ErrRetest) {
		t.Errorf("ParseRecord of a tool with no command = %v, want ErrRetest", err)
	}
}

// #341: a Codex prompt that answers questions of Codex gives only your answers, one on each
// line, with no tag, JSON or question: a list or one reply, with the end tag of the Codex
// source or with none, as the eval report shows it. Another prompt stays as it is, also one
// that holds the tag after its start, and Claude has no tag. A reply that is not JSON, or an
// answer that is not a string, is a new input shape.
func TestParseRecordQuestionReply(t *testing.T) {
	codex, err := Load("codex")
	if err != nil {
		t.Fatal(err)
	}
	claude, err := Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	const (
		tag = "<send_user_message_question_reply>"
		end = "</send_user_message_question_reply>"
	)
	parse := func(p *Profile, prompt string) (string, error) {
		t.Helper()
		raw, err := json.Marshal(map[string]string{"session_id": "s", "prompt": prompt})
		if err != nil {
			t.Fatal(err)
		}
		r, err := p.ParseRecord(EventPrompt, raw)
		return r.Text, err
	}
	for _, tc := range []struct {
		p            *Profile
		prompt, want string
	}{
		{codex, tag + "\n" + `[{"questionItemId":"q-1","question":"Where is the db?","answer":"Use the seed."}]` + "\n" + end,
			"Use the seed."},
		{codex, " " + tag + `{"question":"Where?","answer":"Here."}` + end + "\n", "Here."},
		{codex, tag + ` [{"answer":"Use the seed.","question":"Where is the db?","questionItemId":"q-1"}]`, "Use the seed."},
		{codex, tag + `[{"answer":"A","question":"Q1"},{"answer":"","question":"Q2"},{"answer":"B","question":"Q3"}]` + end, "A\nB"},
		{codex, "Explain " + tag, "Explain " + tag},
		{claude, tag + `[{"answer":"A"}]` + end, tag + `[{"answer":"A"}]` + end},
	} {
		if got, err := parse(tc.p, tc.prompt); err != nil || got != tc.want {
			t.Errorf("ParseRecord(%s, %q) = %q, %v, want %q", tc.p.Kind, tc.prompt, got, err, tc.want)
		}
	}
	for _, prompt := range []string{tag + "not json" + end, tag + `[{"answer":["A"]}]` + end} {
		if _, err := parse(codex, prompt); !errors.Is(err, ErrRetest) {
			t.Errorf("ParseRecord(codex, %q) = %v, want ErrRetest", prompt, err)
		}
	}
}

// Claude and Codex turn on the hook records, each after its live hook test; Codex has no
// failure event (#285).
func TestHooks(t *testing.T) {
	c, err := Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	want := []Hook{{"SessionStart", EventStart, ""}, {"UserPromptSubmit", EventPrompt, ""},
		{"PostToolUse", EventTool, "Bash"}, {"PostToolUseFailure", EventTool, "Bash"}, {"Stop", EventStop, ""}}
	if got := c.Hooks(); !slices.Equal(got, want) {
		t.Errorf("claude Hooks = %+v, want %+v", got, want)
	}
	x, err := Load("codex")
	if err != nil {
		t.Fatal(err)
	}
	want = []Hook{{"SessionStart", EventStart, ""}, {"UserPromptSubmit", EventPrompt, ""},
		{"PostToolUse", EventTool, "Bash"}, {"Stop", EventStop, ""}}
	if got := x.Hooks(); !slices.Equal(got, want) {
		t.Errorf("codex Hooks = %+v, want %+v", got, want)
	}
	for _, e := range []string{EventPrompt, EventTool, EventStop} {
		if !c.On(e) || !x.On(e) {
			t.Errorf("On(%s): claude %v, codex %v, want true, true", e, c.On(e), x.On(e))
		}
	}
}
