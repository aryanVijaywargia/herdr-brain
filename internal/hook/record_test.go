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
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/fingerprint"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// anthropicKey is a fake key in the shape that the secret filter finds.
var anthropicKey = "sk-" + "ant-api03-" + strings.Repeat("xY9_", 8)

// recordFixture is the fixture with an open capture session and segment of pane w14:p1 in
// herdr session default, as brained keeps it.
func recordFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	s := openStore(t, f.mainStore)
	defer closeStore(t, s)
	ctx := context.Background()
	write(t, s, func(tx *sql.Tx) error {
		key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyCapture, ID: "cap-1"}
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

// A hook record stores nothing when capture has no open segment of the pane, when learning is
// off, or when the profile keeps the event off. Input of an unknown shape marks a retest.
func TestRecordStoresNothing(t *testing.T) {
	stop := `{"session_id":"s1","last_assistant_message":"ready"}`
	codex, err := adapter.Load("codex")
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
		{"codex records off", "w14:p1", codex, nil},
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
// back, and the waiting record adds no row. After an item forget the new prompt is new
// evidence: its row gets no link to the forgotten proposal, and nothing matches it. A learn off
// deletes nothing: it only stops capture (§6.7).
func TestRecordPromptIndexWaits(t *testing.T) {
	const prompt = `{"session_id":"s1","prompt":"run make Foo"}`
	hash := fingerprint.Prompt("run make Foo")
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		fn     func(tx *sql.Tx, sessionID, item int64) error
		rows   int // the rows of the old and the new prompt after the release
		newRow bool
	}{
		{"forget session", func(tx *sql.Tx, sessionID, _ int64) error {
			return store.ForgetSession(ctx, tx, sessionID)
		}, 0, false},
		{"forget item", func(tx *sql.Tx, _, item int64) error {
			return store.EraseItem(ctx, tx, item, testNow)
		}, 1, true},
		{"learn off", func(tx *sql.Tx, _, _ int64) error {
			return store.SetLearning(ctx, tx, store.LearningSetting{Scope: store.LearningStore, On: false, Changed: testNow})
		}, 1, false},
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
				if tc.newRow && e.ObservationID == 1 {
					t.Errorf("forgotten row %+v came back", e)
				}
				if e.ObservationID != 1 && (!tc.newRow || len(e.ProposalIDs) != 0) {
					t.Errorf("new row %+v after the release, want none, or one with no link after an item forget", e)
				}
			}
			if !tc.newRow && tc.rows != 0 {
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
