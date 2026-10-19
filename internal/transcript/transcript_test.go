package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const noLimit = 1 << 30

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func exit(n int) *int { return &n }

// offset returns the Codex record id of the line of the file at path that holds text.
func offset(t *testing.T, path, id, text string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(b, []byte(text))
	if i < 0 {
		t.Fatalf("%s holds no %q", path, text)
	}
	return id + "@" + strconv.Itoa(bytes.LastIndexByte(b[:i], '\n')+1)
}

func TestReadClaude(t *testing.T) {
	path := filepath.Join("testdata", "claude-basic.jsonl")
	got, err := ReadClaude(path, noLimit)
	if err != nil {
		t.Fatal(err)
	}
	ts := func(s string) time.Time { return at(t, "2026-09-01T10:"+s+".000Z") }
	want := Session{
		ID: "claude-basic", CWD: "/work/app", Interactive: true,
		First: ts("00:00"), Last: ts("05:10"),
		Turns: []Turn{{
			Prompt: Text{ID: "u1", At: ts("00:00"), Text: "use pnpm, not npm"},
			// Two shell calls in one record give two commands with two ids.
			Commands: []Command{
				{ID: "a1#1", At: ts("00:01"), Command: "pnpm test", Exit: exit(1),
					Failure: &Text{ID: "u2#0", At: ts("00:03"), Text: "Exit code 1\nFAIL test/a.test.js"}},
				{ID: "a1#2", At: ts("00:01"), Command: "pnpm lint"},
			},
			Reply: Text{ID: "a3", At: ts("00:04"), Text: "The tests fail in a.test.js."},
			// m1 is on two lines and counts once: 10+20+30+5, then m2: 1+2.
			Tokens: 68, TokensKnown: true,
		}, {
			Prompt: Text{ID: "u3", At: ts("05:00"), Text: "fix it"},
			Reply:  Text{ID: "a4", At: ts("05:10"), Text: "Fixed."},
			// No usage field: unknown, not 0.
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadClaude(%s) =\n%+v\nwant\n%+v", path, got, want)
	}
}

func TestReadCodex(t *testing.T) {
	path := filepath.Join("testdata", "codex-basic.jsonl")
	got, err := ReadCodex(path, noLimit)
	if err != nil {
		t.Fatal(err)
	}
	ts := func(s string) time.Time { return at(t, "2026-09-02T08:"+s+".000Z") }
	id := func(text string) string { return offset(t, path, "s-codex-1", text) }
	want := Session{
		ID: "s-codex-1", CWD: "/work/api", Interactive: true,
		First: ts("00:00"), Last: ts("02:04"),
		Turns: []Turn{{
			// The prompt comes from the response_item, not the event_msg copy.
			Prompt: Text{ID: id(`"run the tests"`), At: ts("00:03"), Text: "run the tests"},
			Commands: []Command{
				{ID: id(`"call_id":"c1"}`), At: ts("00:05"), Command: "make test", Exit: exit(2),
					Failure: &Text{ID: id(`"Exit code: 2`), At: ts("00:06"),
						Text: "Exit code: 2\nWall time: 1.2 seconds\nOutput:\nFAIL api_test"}},
				{ID: id(`"cmd\": \"make lint`), At: ts("00:07"), Command: "make lint", Exit: exit(0)},
				// No known exit line: the exit status stays unknown.
				{ID: id(`"local_shell_call"`), At: ts("00:09"), Command: "ls -la"},
			},
			Reply: Text{ID: id("make test fails"), At: ts("00:13"), Text: "make test fails in api_test."},
			// The growth of total_tokens; the repeated line adds nothing.
			Tokens: 1000, TokensKnown: true,
		}, {
			Prompt: Text{ID: id(`"thanks"`), At: ts("01:00"), Text: "thanks"},
			Reply:  Text{ID: id("You are welcome."), At: ts("01:01"), Text: "You are welcome."},
		}, {
			Prompt: Text{ID: id(`"run it again"`), At: ts("02:00"), Text: "run it again"},
			Reply:  Text{ID: id(`"Done."`), At: ts("02:04"), Text: "Done."},
			Tokens: 500, TokensKnown: true,
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadCodex(%s) =\n%+v\nwant\n%+v", path, got, want)
	}
}

// TestDrops checks the filters of the text that Brain drops: each fixture holds one line of
// each kind, and only the kept prompt and reply reach a turn.
func TestDrops(t *testing.T) {
	for _, tc := range []struct {
		file   string
		read   func(string, int64) (Session, error)
		tokens int64
	}{
		{"claude-drops.jsonl", ReadClaude, 8},
		{"codex-drops.jsonl", ReadCodex, 0},
	} {
		t.Run(tc.file, func(t *testing.T) {
			s, err := tc.read(filepath.Join("testdata", tc.file), noLimit)
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Turns) != 1 {
				t.Fatalf("turns = %+v, want 1", s.Turns)
			}
			tn := s.Turns[0]
			if tn.Prompt.Text != "keep this prompt" || tn.Reply.Text != "kept reply" || tn.Tokens != tc.tokens {
				t.Errorf("turn = %+v, want prompt %q, reply %q, tokens %d", tn, "keep this prompt", "kept reply", tc.tokens)
			}
			for _, c := range tn.Commands {
				if c.Failure != nil {
					t.Errorf("command %+v has a failure, want none: only a failed shell result is kept", c)
				}
			}
			b, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{"reminder", "hook text", "Brain brief", "/clear", "local output", "meta record",
				"env text", "agents file", "developer", "read file body", "No response requested", "Request interrupted",
				"sidechain", "compaction summary", "success output", "event copy", "image", "base64",
				"task note", "local caveat", "bash input", "bash output", "shell command text", "shell result"} {
				if bytes.Contains(b, []byte(text)) {
					t.Errorf("%s: session holds %q, want it dropped", tc.file, text)
				}
			}
		})
	}
}

// TestClaudeMessage checks that the reply is the whole text of the last message, also over two
// lines, and that a shell result that did not fail keeps its exit line but no output.
func TestClaudeMessage(t *testing.T) {
	path := write(t, `{"type":"user","uuid":"u1","message":{"content":"run it"}}
{"type":"assistant","uuid":"a1","message":{"id":"m1","content":[{"type":"text","text":"First."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"make"}}]}}
{"type":"user","uuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"Exit code 0\nok","is_error":false}]}}
{"type":"assistant","uuid":"a2","message":{"id":"m2","content":[{"type":"text","text":"Tests failed."},{"type":"text","text":"Retry with pnpm."}]}}
{"type":"assistant","uuid":"a3","message":{"id":"m2","content":[{"type":"text","text":"Done."}]}}
`)
	s, err := ReadClaude(path, noLimit)
	if err != nil {
		t.Fatal(err)
	}
	want := Turn{
		Prompt:   Text{ID: "u1", Text: "run it"},
		Commands: []Command{{ID: "a1#1", Command: "make", Exit: exit(0)}},
		Reply:    Text{ID: "a2", Text: "Tests failed.\nRetry with pnpm.\nDone."},
	}
	if len(s.Turns) != 1 || !reflect.DeepEqual(s.Turns[0], want) {
		t.Errorf("ReadClaude turns = %+v, want [%+v]", s.Turns, want)
	}
}

func TestCodexMarks(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		meta                         string
		interactive, fork, sideChain bool
	}{
		{"cli", `"source":"cli"`, true, false, false},
		{"vscode", `"source":"vscode"`, true, false, false},
		{"exec", `"source":"exec"`, false, false, false},
		{"sub-agent source", `"source":{"subagent":"review"}`, false, false, false},
		{"fork", `"source":"cli","forked_from_id":"s-parent"`, true, true, false},
		{"side chain", `"source":"cli","parent_thread_id":"s-parent"`, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := write(t, `{"timestamp":"2026-09-02T08:00:00Z","type":"session_meta","payload":{"id":"s1","cwd":"/w",`+tc.meta+"}}\n")
			s, err := ReadCodex(path, noLimit)
			if err != nil {
				t.Fatal(err)
			}
			if s.Interactive != tc.interactive || s.Fork != tc.fork || s.SideChain != tc.sideChain {
				t.Errorf("ReadCodex(%s) = interactive %v, fork %v, side chain %v; want %v, %v, %v",
					tc.meta, s.Interactive, s.Fork, s.SideChain, tc.interactive, tc.fork, tc.sideChain)
			}
		})
	}
}

// TestLimit checks that a reader reads only up to its byte limit, and skips a last line that
// the limit cuts.
func TestLimit(t *testing.T) {
	path := filepath.Join("testdata", "claude-basic.jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Up to the middle of the line of the second prompt.
	limit := int64(bytes.Index(b, []byte(`"fix it"`)))
	s, err := ReadClaude(path, limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Turns) != 1 || s.Turns[0].Prompt.Text != "use pnpm, not npm" {
		t.Errorf("ReadClaude(%s, %d) turns = %+v, want only the first", path, limit, s.Turns)
	}
}

func write(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUnknownFormat(t *testing.T) {
	const meta = `{"timestamp":"2026-09-02T08:00:00Z","type":"session_meta","payload":{"id":"s1","source":"cli"}}` + "\n"
	for _, tc := range []struct {
		name string
		read func(string, int64) (Session, error)
		text string
	}{
		{"claude not json", ReadClaude, "not json\n"},
		{"claude no type", ReadClaude, `{"uuid":"u1"}` + "\n"},
		{"claude user with no message", ReadClaude, `{"type":"user","uuid":"u1"}` + "\n"},
		{"claude content a number", ReadClaude, `{"type":"user","uuid":"u1","message":{"content":3}}` + "\n"},
		{"claude block with no type", ReadClaude, `{"type":"user","uuid":"u1","message":{"content":[{"text":"x"}]}}` + "\n"},
		{"claude bad time", ReadClaude, `{"type":"user","uuid":"u1","timestamp":"yesterday","message":{"content":"x"}}` + "\n"},
		{"claude flag a string", ReadClaude, `{"type":"user","uuid":"u1","isMeta":"yes","message":{"content":"x"}}` + "\n"},
		{"claude shell with no command", ReadClaude,
			`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t","name":"Bash","input":{}}]}}` + "\n"},
		{"codex not json", ReadCodex, meta + "not json\n"},
		{"codex first line not meta", ReadCodex, `{"type":"response_item","payload":{"type":"reasoning"}}` + "\n"},
		{"codex meta with no id", ReadCodex, `{"type":"session_meta","payload":{"cwd":"/w"}}` + "\n"},
		{"codex unknown line type", ReadCodex, meta + `{"type":"new_kind","payload":{}}` + "\n"},
		{"codex content not a list", ReadCodex, meta + `{"type":"response_item","payload":{"type":"message","role":"user","content":"x"}}` + "\n"},
		{"codex shell arguments not json", ReadCodex, meta + `{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"x"}}` + "\n"},
		{"codex shell with no command", ReadCodex, meta + `{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{}"}}` + "\n"},
		{"codex local shell with no action", ReadCodex, meta + `{"type":"response_item","payload":{"type":"local_shell_call"}}` + "\n"},
		{"codex item with no type", ReadCodex, meta + `{"type":"response_item","payload":{"role":"user"}}` + "\n"},
		{"codex item null", ReadCodex, meta + `{"type":"response_item","payload":null}` + "\n"},
		{"codex event with no type", ReadCodex, meta + `{"type":"event_msg","payload":{}}` + "\n"},
		{"codex info with no total", ReadCodex, meta + `{"type":"event_msg","payload":{"type":"token_count","info":{}}}` + "\n"},
		{"codex total a string", ReadCodex, meta + `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":"x"}}}}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := write(t, tc.text)
			if _, err := tc.read(path, noLimit); !errors.Is(err, ErrUnknownFormat) {
				t.Errorf("read(%q) error = %v, want %v", tc.text, err, ErrUnknownFormat)
			}
		})
	}
}

func TestList(t *testing.T) {
	home := t.TempDir()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	files := []struct {
		path string
		age  int // hours before base
	}{
		{".claude/projects/-work-app/s-old.jsonl", 5},
		{".claude/projects/-work-app/s-new.jsonl", 1},
		{".claude/projects/-work-app/s-old.jsonl.orphaned-1", 0},
		{".claude/projects/-work-app/s-set.superseded-2.jsonl", 0},
		{".claude/projects/-work-app/s-new/subagents/agent-1.jsonl", 0},
		{".claude/projects/-work-app/s-new/tool-results/out.jsonl", 0},
		{".codex/sessions/2026/09/01/rollout-2026-09-01T08-00-00-a.jsonl", 3},
		{".codex/sessions/2026/08/01/rollout-2026-08-01T08-00-00-b.jsonl.zst", 0},
		{".codex/sessions/2026/08/02/rollout-2026-08-02T08-00-00-c.jsonl.zst", 0},
		{".codex/archived_sessions/rollout-2026-07-01T08-00-00-d.jsonl", 0},
		{".codex/archived_sessions/2026/07/01/rollout-2026-07-01T08-00-00-e.jsonl", 0},
	}
	for _, f := range files {
		p := filepath.Join(home, f.path)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		changed := base.Add(-time.Duration(f.age) * time.Hour)
		if err := os.Chtimes(p, changed, changed); err != nil {
			t.Fatal(err)
		}
	}
	got, err := List(filepath.Join(home, ".claude"), filepath.Join(home, ".codex"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range got.Files {
		names = append(names, f.Harness+" "+strings.TrimPrefix(f.Path, home+string(filepath.Separator)))
	}
	want := []string{
		"claude .claude/projects/-work-app/s-new.jsonl",
		"codex .codex/sessions/2026/09/01/rollout-2026-09-01T08-00-00-a.jsonl",
		"claude .claude/projects/-work-app/s-old.jsonl",
	}
	if !reflect.DeepEqual(names, want) || got.Compressed != 2 {
		t.Errorf("List = %q, %d compressed; want %q, 2", names, got.Compressed, want)
	}
}

func TestListEmptyHome(t *testing.T) {
	home := t.TempDir()
	got, err := List(filepath.Join(home, ".claude"), filepath.Join(home, ".codex"))
	if err != nil || len(got.Files) != 0 || got.Compressed != 0 {
		t.Errorf("List(empty home) = %+v, %v; want nothing", got, err)
	}
}

func fuzz(f *testing.F, read func(string, int64) (Session, error), seeds ...string) {
	for _, s := range seeds {
		b, err := os.ReadFile(filepath.Join("testdata", s))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		path := filepath.Join(t.TempDir(), "s.jsonl")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = read(path, noLimit) // only a panic fails: any error is a fine answer
	})
}

func FuzzReadClaude(f *testing.F) {
	fuzz(f, ReadClaude, "claude-basic.jsonl", "claude-drops.jsonl")
}

func FuzzReadCodex(f *testing.F) {
	fuzz(f, ReadCodex, "codex-basic.jsonl", "codex-drops.jsonl", "codex-0160.jsonl")
}

// #407: codex-cli 0.160.1 adds the top-level lines world_state and token_usage_record, which
// the reader skips. Its shell commands are custom_tool_call exec scripts: no command record.
func TestReadCodex0160(t *testing.T) {
	path := filepath.Join("testdata", "codex-0160.jsonl")
	got, err := ReadCodex(path, noLimit)
	if err != nil {
		t.Fatal(err)
	}
	ts := func(s string) time.Time { return at(t, "2026-10-13T09:"+s+".000Z") }
	id := func(text string) string { return offset(t, path, "s-codex-0160", text) }
	want := Session{
		ID: "s-codex-0160", CWD: "/work/shop", Interactive: true,
		First: ts("00:00"), Last: ts("00:08"),
		Turns: []Turn{{
			Prompt: Text{ID: id(`"list the files"`), At: ts("00:03"), Text: "list the files"},
			Reply:  Text{ID: id("The folder holds"), At: ts("00:07"), Text: "The folder holds main.go."},
			Tokens: 720, TokensKnown: true,
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadCodex(%s) =\n%+v\nwant\n%+v", path, got, want)
	}
}
