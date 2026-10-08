package hook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/fingerprint"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// anthropicKey is a fake key in the shape that the secret filter finds.
var anthropicKey = "sk-" + "ant-api03-" + strings.Repeat("xY9_", 8)

// recordFixture is the fixture with an open claude capture session and segment of pane w14:p1
// in herdr session default, as brained keeps it.
func recordFixture(t *testing.T) *fixture {
	t.Helper()
	return recordFixtureOf(t, "claude")
}

// recordFixtureOf is recordFixture with a capture session of harness.
func recordFixtureOf(t *testing.T, harness string) *fixture {
	t.Helper()
	f := newFixture(t)
	s := openStore(t, f.mainStore)
	defer closeStore(t, s)
	ctx := context.Background()
	write(t, s, func(tx *sql.Tx) error {
		key := store.SessionKey{HerdrSession: "default", Harness: harness, Kind: store.KeyCapture, ID: "cap-1"}
		id, err := store.OpenSession(ctx, tx, key, "w14:p1", "ws_a14", testNow)
		if err != nil {
			return err
		}
		_, err = store.StartSegment(ctx, tx, id, "w14:p1", "ws_a14", "visible", testNow)
		return err
	})
	return f
}

// runRecord runs `brain hook --profile <p> --event <event>` with input raw in pane paneID of
// herdr session default, at a time one second after the last run.
func runRecord(t *testing.T, f *fixture, p *adapter.Profile, event, paneID, raw string, clock *time.Time) error {
	t.Helper()
	h := f.hook(p, environ(f.mainSock, "", paneID, strings.Split(paneID, ":")[0]))
	h.Event = event
	*clock = clock.Add(time.Second)
	at := *clock
	h.Now = func() time.Time { return at }
	var out bytes.Buffer
	err := h.run(context.Background(), strings.NewReader(raw), &out)
	if out.Len() != 0 {
		t.Errorf("hook --event %s printed %q, want nothing", event, out.String())
	}
	return err
}

func recordRows(t *testing.T, f *fixture) ([]store.Observation, []store.CommandRecord) {
	t.Helper()
	s := openStore(t, f.mainStore)
	defer closeStore(t, s)
	var obs []store.Observation
	var cmds []store.CommandRecord
	write(t, s, func(tx *sql.Tx) (err error) {
		ctx := context.Background()
		if obs, err = store.Observations(ctx, tx); err != nil {
			return err
		}
		cmds, err = store.CommandRecords(ctx, tx)
		return err
	})
	return obs, cmds
}

// Each hook record event stores one observation, and a tool one command record too (E11-S5).
// A command with a secret stores no text: only secret found = yes.
func TestRecord(t *testing.T) {
	f := recordFixture(t)
	p := claude(t)
	clock := testNow
	long := "echo " + strings.Repeat("é", 3000)
	for _, r := range []struct{ event, raw string }{
		{adapter.EventTool, `{"session_id":"s1","cwd":"/work","tool_input":{"command":"ls nosuchfile-zq"},
			"error":"Exit code 1\nls: nosuchfile-zq: No such file or directory"}`},
		{adapter.EventTool, `{"session_id":"s1","cwd":"/work","tool_input":{"command":"curl -H 'x-api-key: ` +
			anthropicKey + `' https://api.example.com"}}`},
		{adapter.EventTool, `{"session_id":"s1","tool_input":{"command":"` + long + `"}}`},
		{adapter.EventPrompt, `{"session_id":"s1","prompt":"Reply with only the word ready."}`},
		{adapter.EventStop, `{"session_id":"s1","last_assistant_message":"ready"}`},
	} {
		if err := runRecord(t, f, p, r.event, "w14:p1", r.raw, &clock); err != nil {
			t.Fatalf("hook --event %s: %v", r.event, err)
		}
	}
	obs, cmds := recordRows(t, f)
	if len(obs) != 5 || len(cmds) != 3 {
		t.Fatalf("got %d observations and %d command records, want 5 and 3:\n%+v\n%+v", len(obs), len(cmds), obs, cmds)
	}
	for i, want := range []struct {
		event   string
		sender  store.Sender
		excerpt string
	}{
		{"tool", store.SenderTool, "ls nosuchfile-zq"},
		{"tool", store.SenderTool, ""},
		{"tool", store.SenderTool, ""}, // checked below
		{"prompt", store.SenderUnconfirmed, "Reply with only the word ready."},
		{"stop", store.SenderAssistant, "ready"},
	} {
		o := obs[i]
		if o.Source != store.SourceHook || o.Event != want.event || o.Sender != want.sender || !o.Counts ||
			i != 2 && o.Excerpt != want.excerpt {
			t.Errorf("observation %d = %+v, want a hook %s by %s with %q", i, o, want.event, want.sender, want.excerpt)
		}
	}
	if c := cmds[0]; c.Argv != "ls nosuchfile-zq" || c.CWD != "/work" || c.ExitStatus == nil || *c.ExitStatus != 1 ||
		c.Truncated || c.SecretFound {
		t.Errorf("command record = %+v, want ls with exit status 1", c)
	}
	// AC: a command with a secret gives secret found = yes and no stored text.
	if c := cmds[1]; !c.SecretFound || c.Argv != "" || c.ExitStatus != nil || obs[1].Excerpt != "" {
		t.Errorf("secret command = %+v, excerpt %q, want secret found and no text", c, obs[1].Excerpt)
	}
	if c := cmds[2]; !c.Truncated || len(c.Argv) > argvLimit || len(c.Argv) < argvLimit-1 || !utf8.ValidString(c.Argv) ||
		c.CWD != "" {
		t.Errorf("long command: %d bytes, truncated %v, cwd %q, want ≤ %d valid bytes, truncated", len(c.Argv), c.Truncated,
			c.CWD, argvLimit)
	}
	for _, o := range obs {
		if strings.Contains(o.Excerpt, anthropicKey) {
			t.Errorf("observation %d holds the secret", o.ID)
		}
	}
}

// AC #285: each codex-cli 0.160.1 payload of the live hook test that reaches `brain hook`
// (spike/m0/data/e11s1c-codex-hooks.txt), in the order of the test, stores the record that
// Brain needs. A prompt keeps an @-mention as typed. A command record has the whole command and
// the turn cwd, also when the command ran in another folder, and no exit status. A stop record
// has the reply.
func TestRecordCodex(t *testing.T) {
	p, err := adapter.Load("codex")
	if err != nil {
		t.Fatal(err)
	}
	payload := func(name string) []byte { return codexPayload(t, name) }
	echo := "Repeat this whole message back to me exactly, character for character, and nothing else. " +
		"Do not use any tool: qx-4410 blue river 7."
	want := []struct {
		event, file string
		excerpt     string // the stored text; for a tool, also the command of its record
	}{
		{adapter.EventPrompt, "prompt.json", echo},
		{adapter.EventStop, "stop-echo.json", echo},
		{adapter.EventPrompt, "prompt-mention.json", "Read @notes-zk.txt and quote its marker line. Do not run any shell command."},
		{adapter.EventStop, "stop-mention.json", "> marker: qz-6612"},
		{adapter.EventPrompt, "prompt-commands.json", "Run these shell commands, one tool call each, in this order. " +
			"1: cd sub && pwd && ls nosuchfile-zq . 2: pwd, with the working directory of the tool call set to the sub " +
			"folder (use the workdir option of the tool, not cd). 3: git status --short. Then reply with the word " +
			"zr-5531 and one short sentence."},
		{adapter.EventTool, "tool-cd.json", "cd sub && pwd && ls nosuchfile-zq ."},
		// pwd ran in the sub folder: the cwd is still the turn cwd.
		{adapter.EventTool, "tool-workdir.json", "pwd"},
		{adapter.EventTool, "tool-git-status.json", "git status --short"},
		// The excerpt drops the blank line of the reply.
		{adapter.EventStop, "stop-commands.json", "zr-5531\nAll three commands ran in order; the first reported the " +
			"missing file, and Git status was clean."},
		{adapter.EventPrompt, "prompt-long.json", "Run this exact shell command as one tool call: sleep 15 && echo " +
			"long-zq-done . Then reply with only the word zr-6620."},
		{adapter.EventTool, "tool-long.json", "sleep 15 && echo long-zq-done ."},
		{adapter.EventStop, "stop-long.json", "zr-6620"},
		// A turn stopped with Esc: its prompt is the last hook run.
		{adapter.EventPrompt, "prompt-stopped.json", "Run this exact shell command as one tool call: sleep 40 && echo " +
			"late-zq . Then reply with only the word zr-7730."},
	}
	f := recordFixtureOf(t, "codex")
	clock := testNow
	for _, w := range want {
		if err := runRecord(t, f, p, w.event, "w14:p1", string(payload(w.file)), &clock); err != nil {
			t.Fatalf("hook --event %s with %s: %v", w.event, w.file, err)
		}
	}
	obs, cmds := recordRows(t, f)
	if len(obs) != len(want) || len(cmds) != 4 {
		t.Fatalf("got %d observations and %d command records, want %d and 4:\n%+v\n%+v", len(obs), len(cmds),
			len(want), obs, cmds)
	}
	var tools []store.Observation
	for i, w := range want {
		o := obs[i]
		if o.Source != store.SourceHook || o.Event != w.event || o.Sender != senders[w.event] || !o.Counts ||
			o.Excerpt != w.excerpt {
			t.Errorf("%s: observation = %+v, want a hook %s by %s with %q", w.file, o, w.event, senders[w.event], w.excerpt)
		}
		if w.event == adapter.EventTool {
			tools = append(tools, o)
		}
	}
	for i, c := range cmds {
		if c.Argv != tools[i].Excerpt || c.CWD != "~/brain-eval/t0134/work" || c.ExitStatus != nil || c.Truncated ||
			c.SecretFound || c.ObservationID != tools[i].ID {
			t.Errorf("command record %d = %+v, want %q in the turn cwd with no exit status", i, c, tools[i].Excerpt)
		}
	}

	// The prompt hook with no output left the prompt unchanged: Codex echoed it word for word.
	prompt, err := p.ParseRecord(adapter.EventPrompt, payload("prompt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if reply, err := p.ParseRecord(adapter.EventStop, payload("stop-echo.json")); err != nil || reply.Text != prompt.Text {
		t.Errorf("echo reply = %q, %v, want the prompt %q", reply.Text, err, prompt.Text)
	}
}

// #341: the prompt record of your answer to a question of Codex 0.160.1 (request_user_input)
// holds only the answer, also in the prompt index: not the tag, the JSON or the question of
// Codex. prompt-question.json is made in the shape of the M2 eval run 3 report, with no real
// session data.
func TestRecordCodexQuestion(t *testing.T) {
	p, err := adapter.Load("codex")
	if err != nil {
		t.Fatal(err)
	}
	f := recordFixtureOf(t, "codex")
	clock := testNow
	if err := runRecord(t, f, p, adapter.EventPrompt, "w14:p1", string(codexPayload(t, "prompt-question.json")), &clock); err != nil {
		t.Fatal(err)
	}
	const answer = "Create it from the seed with ./scripts/reset-db.sh, then measure."
	obs, _ := recordRows(t, f)
	if len(obs) != 1 || obs[0].Excerpt != answer || obs[0].Sender != store.SenderUnconfirmed || !obs[0].Counts {
		t.Fatalf("observations = %+v, want one counted prompt %q", obs, answer)
	}
	if es := promptEntries(t, f); len(es) != 1 || es[0].Text != answer || es[0].Hash != fingerprint.Prompt(answer) {
		t.Errorf("prompt index = %+v, want one row of %q", es, answer)
	}
}

// codexPayload reads the payload file name of the codex-cli 0.160.1 live hook test (#285).
func codexPayload(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "codex-0.160.1", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Every Codex tool fires PostToolUse; an MCP tool has no command. So the codex tool entry
// matches only the shell tool, whose tool_name the profile names (#285).
func TestCodexToolMatcher(t *testing.T) {
	p, err := adapter.Load("codex")
	if err != nil {
		t.Fatal(err)
	}
	payload := func(name string) []byte { return codexPayload(t, name) }
	var mcp, shell struct {
		ToolName string `json:"tool_name"`
	}
	if err := json.Unmarshal(payload("tool-mcp.json"), &mcp); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload("tool-cd.json"), &shell); err != nil {
		t.Fatal(err)
	}
	if shell.ToolName != p.Events.ToolMatcher || mcp.ToolName == p.Events.ToolMatcher {
		t.Errorf("tool matcher %q: shell tool %q, MCP tool %q, want only the shell tool", p.Events.ToolMatcher,
			shell.ToolName, mcp.ToolName)
	}
	if _, err := p.ParseRecord(adapter.EventTool, payload("tool-mcp.json")); !errors.Is(err, adapter.ErrRetest) {
		t.Errorf("MCP tool input = %v, want ErrRetest: it has no command", err)
	}
}

// A hook record stores nothing when capture has no open segment of the pane, when learning is
// off, or when the profile keeps the event off. Input of an unknown shape marks a retest.
func TestRecordStoresNothing(t *testing.T) {
	stop := `{"session_id":"s1","last_assistant_message":"ready"}`
	agy, err := adapter.Load("agy")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, pane string
		profile    *adapter.Profile
		prep       func(t *testing.T, f *fixture)
	}{
		{"no capture segment", "w15:p1", nil, nil},
		{"pane not live", "w14:p7", nil, nil},
		{"agy stop record off", "w14:p1", agy, nil},
		{"learning off", "w14:p1", nil, func(t *testing.T, f *fixture) {
			t.Helper()
			s := openStore(t, f.mainStore)
			defer closeStore(t, s)
			write(t, s, func(tx *sql.Tx) error {
				return store.SetLearning(context.Background(), tx,
					store.LearningSetting{Scope: store.LearningStore, On: false, Changed: testNow})
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := recordFixture(t)
			if tc.prep != nil {
				tc.prep(t, f)
			}
			p := tc.profile
			if p == nil {
				p = claude(t)
			}
			clock := testNow
			if err := runRecord(t, f, p, adapter.EventStop, tc.pane, stop, &clock); err != nil {
				t.Fatal(err)
			}
			if obs, cmds := recordRows(t, f); len(obs) != 0 || len(cmds) != 0 {
				t.Errorf("stored %+v, %+v, want nothing", obs, cmds)
			}
		})
	}

	f := recordFixture(t)
	clock := testNow
	err = runRecord(t, f, claude(t), adapter.EventTool, "w14:p1", `{"session_id":"s1","tool_input":{}}`, &clock)
	if !errors.Is(err, adapter.ErrRetest) {
		t.Fatalf("tool with no command = %v, want ErrRetest", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "data", "retest")); err != nil {
		t.Errorf("no retest marker: %v", err)
	}
}

// A brain forget --session after a hook record noted the capture generation, while it reads
// its input or before its write, leaves the forgotten text absent. A replay of a stored record
// stores nothing, and a new record after the forget is stored (E11-S6).
func TestRecordForgotten(t *testing.T) {
	f := recordFixture(t)
	p := claude(t)
	stop := func(text string) string { return `{"session_id":"s1","last_assistant_message":"` + text + `"}` }
	run := func(raw string, at time.Time, noted func(), in io.Reader) {
		t.Helper()
		if in == nil {
			in = strings.NewReader(raw)
		}
		h := f.hook(p, environ(f.mainSock, "", "w14:p1", "w14"))
		h.Event = adapter.EventStop
		h.Now = func() time.Time { return at }
		h.noted = noted
		var out bytes.Buffer
		if err := h.run(context.Background(), in, &out); err != nil || out.Len() != 0 {
			t.Fatalf("hook = %v, printed %q", err, out.String())
		}
	}
	forget := func() {
		s := openStore(t, f.mainStore)
		defer closeStore(t, s)
		write(t, s, func(tx *sql.Tx) error {
			ss, err := store.SessionsByID(context.Background(), tx, "default", "cap-1")
			if err != nil || len(ss) != 1 {
				t.Fatalf("SessionsByID = %+v, %v", ss, err)
			}
			return store.ForgetSession(context.Background(), tx, ss[0].ID)
		})
	}
	first := testNow.Add(time.Second)
	run(stop("stored before the forget"), first, nil, nil)
	forget()
	run(stop("forgotten while it waits"), first.Add(time.Second), forget, nil)
	run("", first.Add(2*time.Second), nil, &forgetOnRead{r: strings.NewReader(stop("forgotten while it is read")), fn: forget})
	run(stop("stored before the forget"), first, nil, nil) // the replay after a restart
	if obs, cmds := recordRows(t, f); len(obs) != 0 || len(cmds) != 0 {
		t.Fatalf("after the forget: %+v, %+v, want nothing", obs, cmds)
	}
	run(stop("new text after the forget"), first.Add(3*time.Second), nil, nil)
	if obs, _ := recordRows(t, f); len(obs) != 1 || obs[0].Excerpt != "new text after the forget" {
		t.Errorf("after the new record: %+v, want only its text", obs)
	}
}

// forgetOnRead is a reader of r that calls fn once, at the first read.
type forgetOnRead struct {
	r  io.Reader
	fn func()
}

func (f *forgetOnRead) Read(p []byte) (int, error) {
	if f.fn != nil {
		f.fn()
		f.fn = nil
	}
	return f.r.Read(p)
}

// promptEntries reads the prompt index of the fixture store.
func promptEntries(t *testing.T, f *fixture) []store.PromptEntry {
	t.Helper()
	s := openStore(t, f.mainStore)
	defer closeStore(t, s)
	var es []store.PromptEntry
	write(t, s, func(tx *sql.Tx) (err error) {
		es, err = store.PromptEntries(context.Background(), tx)
		return err
	})
	return es
}

// A prompt record writes one prompt index row in the transaction of its observation. Its hash
// is the fingerprint of the full prompt with the secret masked, blank lines and length kept;
// its text is filtered and cut to 500 characters. A tool or a stop record writes no row
// (E14-S1).
func TestRecordPromptIndex(t *testing.T) {
	f := recordFixture(t)
	p := claude(t)
	clock := testNow
	full := "deploy with KEY=" + anthropicKey + "\n\n" + strings.Repeat("then check the logs ", 40)
	raw, err := json.Marshal(map[string]string{"session_id": "s1", "prompt": full})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ event, raw string }{
		{adapter.EventPrompt, string(raw)},
		{adapter.EventTool, `{"session_id":"s1","tool_input":{"command":"ls"}}`},
		{adapter.EventStop, `{"session_id":"s1","last_assistant_message":"done"}`},
	} {
		if err := runRecord(t, f, p, r.event, "w14:p1", r.raw, &clock); err != nil {
			t.Fatalf("hook --event %s: %v", r.event, err)
		}
	}
	es := promptEntries(t, f)
	obs, _ := recordRows(t, f)
	if len(es) != 1 || len(obs) != 3 {
		t.Fatalf("prompt index = %+v for %d observations, want 1 row", es, len(obs))
	}
	masked := strings.Replace(full, anthropicKey, "[secret]", 1)
	e := es[0]
	if e.Hash != fingerprint.Prompt(masked) || e.ObservationID != obs[0].ID || e.WorkspaceKey != "ws_a14" {
		t.Errorf("row = %+v, want the hash of the masked full prompt for observation %d", e, obs[0].ID)
	}
	if e.Hash == fingerprint.Prompt(obs[0].Excerpt) {
		t.Error("the hash is the hash of the cut excerpt, want the full prompt")
	}
	if strings.Contains(e.Text, anthropicKey) || utf8.RuneCountInString(e.Text) != 500 || !strings.HasPrefix(e.Text, "deploy with KEY=[secret]") {
		t.Errorf("row text = %q, want the masked prompt cut to 500 characters", e.Text)
	}
}

// AC: an index write that waits after its note, then a brain forget --session of its source,
// an item forget or a learn off, then the release: the forgotten prompt index rows do not come
// back, and the waiting record adds no row: an item forget raises the capture generation of
// the sessions of its rows, as a session forget does. A learn off deletes nothing: it only
// stops capture (§6.7).
func TestRecordPromptIndexWaits(t *testing.T) {
	const prompt = `{"session_id":"s1","prompt":"run make Foo"}`
	hash := fingerprint.Prompt("run make Foo")
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		fn   func(tx *sql.Tx, sessionID, item int64) error
		rows int // the rows after the release
	}{
		{"forget session", func(tx *sql.Tx, sessionID, _ int64) error {
			return store.ForgetSession(ctx, tx, sessionID)
		}, 0},
		{"forget item", func(tx *sql.Tx, _, item int64) error {
			return store.EraseItem(ctx, tx, item, testNow)
		}, 0},
		{"learn off", func(tx *sql.Tx, _, _ int64) error {
			return store.SetLearning(ctx, tx, store.LearningSetting{Scope: store.LearningStore, On: false, Changed: testNow})
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := recordFixture(t)
			p := claude(t)
			clock := testNow
			if err := runRecord(t, f, p, adapter.EventPrompt, "w14:p1", prompt, &clock); err != nil {
				t.Fatal(err)
			}
			sessionID, item := proposeFrom(t, f, promptEntries(t, f)[0].ObservationID)
			h := f.hook(p, environ(f.mainSock, "", "w14:p1", "w14"))
			h.Event = adapter.EventPrompt
			at := clock.Add(time.Second)
			h.Now = func() time.Time { return at }
			h.noted = func() {
				s := openStore(t, f.mainStore)
				defer closeStore(t, s)
				write(t, s, func(tx *sql.Tx) error { return tc.fn(tx, sessionID, item) })
			}
			var out bytes.Buffer
			if err := h.run(ctx, strings.NewReader(prompt), &out); err != nil {
				t.Fatal(err)
			}
			es := promptEntries(t, f)
			if len(es) != tc.rows {
				t.Fatalf("prompt index after the release = %+v, want %d rows", es, tc.rows)
			}
			for _, e := range es {
				if e.ObservationID != 1 {
					t.Errorf("new row %+v after the release, want none", e)
				}
			}
			if tc.rows != 0 {
				return // learn off keeps the stored prompt and its proposal
			}
			s := openStore(t, f.mainStore)
			defer closeStore(t, s)
			write(t, s, func(tx *sql.Tx) error {
				ids, err := store.MatchPrompt(ctx, tx, hash, "ws_a14")
				if len(ids) != 0 {
					t.Errorf("MatchPrompt after the release = %v, want none", ids)
				}
				return err
			})
		})
	}
}

// proposeFrom makes a pending global proposal whose trigger prompt is observation obs, and
// returns the capture session of obs and the item of the proposal.
func proposeFrom(t *testing.T, f *fixture, obs int64) (sessionID, item int64) {
	t.Helper()
	ctx := context.Background()
	s := openStore(t, f.mainStore)
	defer closeStore(t, s)
	write(t, s, func(tx *sql.Tx) error {
		ss, err := store.SessionsByID(ctx, tx, "default", "cap-1")
		if err != nil || len(ss) != 1 {
			t.Fatalf("SessionsByID = %+v, %v", ss, err)
		}
		sessionID = ss[0].ID
		claim, err := store.ClaimID(ctx, tx, store.ClaimKey{Situation: "make", Rule: "run make Foo"}, testNow)
		if err != nil {
			return err
		}
		cand, err := store.Attach(ctx, tx, store.Candidate{ClaimID: claim, Scope: store.ScopeGlobal,
			Kind: store.CandidateProcedure}, obs, store.DefaultGates(), signal.Supports)
		if err != nil {
			return err
		}
		if item, err = store.InsertItem(ctx, tx, store.Item{ClaimID: claim, CurrentRevision: 1, Kind: store.KindPreference,
			Scope: store.ScopeGlobal, State: store.StateDraft, Created: testNow}); err != nil {
			return err
		}
		base := int64(1)
		id, err := store.ProposeCandidate(ctx, tx, cand, store.Proposal{ItemID: &item, BaseRevision: &base,
			Text: "make: run make Foo", Scope: store.ScopeGlobal, Target: "global", Diff: "+ make: run make Foo",
			Status: store.ProposalPending, Created: testNow})
		if err != nil {
			return err
		}
		ids, err := store.MatchPrompt(ctx, tx, fingerprint.Prompt("run make Foo"), "ws_a14")
		if len(ids) != 1 || ids[0] != id {
			t.Fatalf("MatchPrompt before the forget = %v, want [%d]", ids, id)
		}
		return err
	})
	return sessionID, item
}

// dispatch is one start of a suggestion process by the hook.
type dispatch struct {
	environ []string
	d       brained.Dispatch
}

// E14-S3 fastest path: a prompt record that matches a ready proposal starts one detached
// suggestion process with ids only, in the pane's environment, and prints nothing. A record
// with no match, or of a tool, starts none. The hook does not wait for the process: Dispatch
// returns at once (brained.Start waits only for the shell).
func TestRecordSuggestion(t *testing.T) {
	const prompt = `{"session_id":"s1","prompt":"run make Foo"}`
	f := recordFixture(t)
	p := claude(t)
	clock := testNow
	if err := runRecord(t, f, p, adapter.EventPrompt, "w14:p1", prompt, &clock); err != nil {
		t.Fatal(err)
	}
	sessionID, _ := proposeFrom(t, f, promptEntries(t, f)[0].ObservationID)
	env := environ(f.mainSock, "", "w14:p1", "w14")
	var got []dispatch
	run := func(event, raw string) {
		t.Helper()
		h := f.hook(p, env)
		h.Event = event
		clock = clock.Add(time.Second)
		at := clock
		h.Now = func() time.Time { return at }
		h.Dispatch = func(_ context.Context, environ []string, d brained.Dispatch) error {
			got = append(got, dispatch{environ, d})
			return nil
		}
		var out bytes.Buffer
		if err := h.run(context.Background(), strings.NewReader(raw), &out); err != nil {
			t.Fatalf("hook --event %s: %v", event, err)
		}
		if out.Len() != 0 {
			t.Errorf("hook --event %s printed %q, want nothing", event, out.String())
		}
	}
	run(adapter.EventPrompt, prompt)
	run(adapter.EventTool, `{"session_id":"s1","tool_input":{"command":"run make Foo"}}`)
	run(adapter.EventPrompt, `{"session_id":"s1","prompt":"run make Bar"}`)
	obs, _ := recordRows(t, f)
	want := brained.Dispatch{Suggestion: 1, Binding: "default", Store: f.mainStore, Workspace: "ws_a14",
		Session: sessionID, Occurrence: obs[1].ID, Generation: 0, Proposal: 1}
	if len(got) != 1 || got[0].d != want || !slices.Equal(got[0].environ, env) {
		t.Fatalf("dispatches = %+v, want one of %+v in the pane's environment", got, want)
	}
	// No rate cap: the next matching prompt gets its own suggestion.
	run(adapter.EventPrompt, prompt)
	if len(got) != 2 || got[1].d.Suggestion != 2 {
		t.Errorf("dispatches = %+v, want a second suggestion", got)
	}
}
