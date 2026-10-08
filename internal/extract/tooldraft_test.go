package extract

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/tool"
)

// shop is the folder of the commands of the tests.
const shop = "/src/shop"

// command is one command record of a test session: argv in the shop folder, with exit status
// 0, unless set otherwise.
func command(argv string) store.CommandRecord {
	exit := 0
	return store.CommandRecord{Argv: argv, CWD: shop, ExitStatus: &exit}
}

// turn is one prompt of you and the commands that the agent ran after it.
type turn struct {
	prompt string
	cmds   []store.CommandRecord
}

// commandSession stores an ended session of agent id in ws-A with turns: each prompt is a pane
// read of you, and each command a hook record of the tool with its command record, that a pane
// read of the same call enriches, as brained.Match does. The excerpt is cut at 500 characters,
// as the hook does. It returns the session id.
func (f *fixture) commandSession(id string, turns ...turn) int64 {
	f.t.Helper()
	return f.agentSession("claude", id, func(ctx context.Context, tx *sql.Tx, add adder) error {
		for _, tn := range turns {
			if _, err := add(store.Observation{Source: store.SourceHerdr, Event: "working", Excerpt: tn.prompt,
				Sender: store.SenderYou}); err != nil {
				return err
			}
			for _, c := range tn.cmds {
				excerpt := c.Argv[:min(len(c.Argv), 500)]
				o, err := add(store.Observation{Source: store.SourceHook, Event: "tool", Excerpt: excerpt,
					Sender: store.SenderTool})
				if err != nil {
					return err
				}
				c.ObservationID = o
				if err := store.AddCommandRecord(ctx, tx, c); err != nil {
					return err
				}
				pane, err := add(store.Observation{Source: store.SourceHerdr, Event: "working", Excerpt: excerpt,
					Sender: store.SenderAssistant})
				if err != nil {
					return err
				}
				if err := store.Enrich(ctx, tx, pane, o); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// adder adds one observation of a session and returns its id.
type adder func(store.Observation) (int64, error)

// agentSession stores an ended session of agent id of harness in ws-A, in a new pane, with the
// observations that fill adds, one second apart from t0. It returns the session id.
func (f *fixture) agentSession(harness, id string, fill func(context.Context, *sql.Tx, adder) error) int64 {
	f.t.Helper()
	key := store.SessionKey{HerdrSession: "default", Harness: harness, Kind: store.KeyAgent, ID: id}
	f.panes++
	pane := fmt.Sprintf("w1:p%d", f.panes)
	var sid int64
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		if sid, err = store.OpenSession(ctx, tx, key, pane, "ws-A", t0); err != nil {
			return err
		}
		seg, err := store.StartSegment(ctx, tx, sid, pane, "ws-A", "recent_unwrapped", t0)
		if err != nil {
			return err
		}
		at, n := t0, 0
		add := func(o store.Observation) (int64, error) {
			at, n = at.Add(time.Second), n+1
			o.CaptureIdentity, o.SegmentID, o.Status, o.Counts, o.At = fmt.Sprintf("%s-%d", id, n), seg,
				store.ObservationComplete, true, at
			return store.AddObservation(ctx, tx, o)
		}
		if err := fill(ctx, tx, add); err != nil {
			return err
		}
		return store.EndSession(ctx, tx, key, at)
	})
	return sid
}

// rec is one record of a Codex session with hook records: a hook prompt record, a pane line
// that capture keeps as unconfirmed text, or a hook tool record with its command record.
type rec struct {
	o   store.Observation
	cmd *store.CommandRecord
}

// promptRec is the hook prompt record of text.
func promptRec(text string) rec {
	return rec{o: store.Observation{Source: store.SourceHook, Event: "prompt", Excerpt: text,
		Sender: store.SenderUnconfirmed}}
}

// paneRec is a pane line of text that capture keeps as unconfirmed, as tool output with no
// tool mark above it in the same read (#337).
func paneRec(text string) rec {
	return rec{o: store.Observation{Source: store.SourceHerdr, Event: "working", Excerpt: text,
		Sender: store.SenderUnconfirmed}}
}

// codexRec is the hook tool record of command argv in the shop folder. It has no exit status:
// a Codex hook record has none.
func codexRec(argv string) rec {
	return rec{o: store.Observation{Source: store.SourceHook, Event: "tool", Excerpt: argv, Sender: store.SenderTool},
		cmd: &store.CommandRecord{Argv: argv, CWD: shop}}
}

// codexSession stores an ended Codex session of agent id in ws-A with recs, in order. It
// returns the session id.
func (f *fixture) codexSession(id string, recs ...rec) int64 {
	f.t.Helper()
	return f.agentSession("codex", id, func(ctx context.Context, tx *sql.Tx, add adder) error {
		for _, r := range recs {
			o, err := add(r.o)
			if err != nil {
				return err
			}
			if r.cmd == nil {
				continue
			}
			c := *r.cmd
			c.ObservationID = o
			if err := store.AddCommandRecord(ctx, tx, c); err != nil {
				return err
			}
		}
		return nil
	})
}

// toolProposals returns the pending proposals of tool items.
func (f *fixture) toolProposals() []store.Proposal {
	f.t.Helper()
	var ps []store.Proposal
	for _, p := range f.proposals() {
		if p.Status != store.ProposalPending || p.ItemID == nil {
			continue
		}
		var kind store.Kind
		f.write(func(ctx context.Context, tx *sql.Tx) error {
			it, err := store.ItemByID(ctx, tx, *p.ItemID)
			kind = it.Kind
			return err
		})
		if kind == store.KindTool {
			ps = append(ps, p)
		}
	}
	return ps
}

// toolReply answers each prompt with one tool claim over each ref of the prompt, with the
// package input in place of the filter, set over the default fields.
func toolReply(set map[string]any) func(context.Context, string) ([]byte, error) {
	return func(_ context.Context, prompt string) ([]byte, error) {
		c := replyOf(map[string]any{"kind": "tool", "situation": "testing a package", "rule": "use the tool",
			"evidence_refs": promptRefs(prompt), "name": "test-package", "description": "Run the tests of one package",
			"commands":  [][]string{{"pnpm", "test", "--filter", "{package}"}},
			"inputs":    []any{map[string]any{"name": "package", "type": "string", "pattern": "^[a-z0-9@/._-]{1,64}$"}},
			"timeout_s": 600, "effects": "runs tests; writes nothing outside the repo", "read_only": false})
		for k, v := range set {
			c[k] = v
		}
		return claudeReply(map[string]any{"claims": []any{c}})
	}
}

// AC: pnpm test --filter api and pnpm test --filter web in 3 sessions give one tool draft with a
// {package} input and a pattern. The model gives the placeholder; Brain gives the folder, and
// the claim key of the commands joins the 3 sessions.
func TestPlaceholderTool(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = toolReply(nil)
	for i, pkg := range []string{"api", "web", "api"} {
		f.commandSession(fmt.Sprintf("s%d", i), turn{"run the tests of " + pkg, []store.CommandRecord{
			command("pnpm test --filter " + pkg)}})
		f.mustRun()
		if ps := f.toolProposals(); i < 2 && len(ps) != 0 {
			t.Fatalf("tool drafts after session %d = %+v, want none below the gate", i+1, ps)
		}
	}
	if !strings.Contains(f.cli.prompts[0], toolHelp) {
		t.Errorf("prompt lacks the tool help:\n%s", f.cli.prompts[0])
	}
	ps := f.toolProposals()
	if len(ps) != 1 {
		t.Fatalf("tool drafts = %+v, want one", ps)
	}
	c, err := tool.Parse(ps[0].Text)
	if err != nil {
		t.Fatalf("Parse(draft) = %v", err)
	}
	if !slices.Equal(c.Argv, []string{"pnpm", "test", "--filter", "{package}"}) || c.Cwd != shop ||
		c.Inputs["package"].Pattern != "^[a-z0-9@/._-]{1,64}$" {
		t.Errorf("draft = %+v, want argv with {package}, cwd %s and the pattern", c, shop)
	}
	if ps[0].Scope != store.ScopeWorkspace || ps[0].Diff != store.SkillDiff("", ps[0].Text) {
		t.Errorf("draft scope %s, diff %q, want workspace and the whole contract added", ps[0].Scope, ps[0].Diff)
	}
	if n := f.count(`SELECT COUNT(*) FROM tools WHERE approved_hash IS NOT NULL`); n != 0 {
		t.Errorf("approved tools = %d, want none: nothing runs before approval", n)
	}
	// #314 AC 3: the chain counts from its first run, as before. The prompt of session 3 repeats
	// the one of session 1, but a tool candidate refuses your text, so its first telling adds
	// nothing either.
	if n := f.count(`SELECT COUNT(*) FROM candidates WHERE kind = 'command_chain' AND count = 3 AND sessions = 3`); n != 1 {
		t.Errorf("command candidates with 3 occurrences in 3 sessions = %d, want 1", n)
	}
}

// Exact repeats come first: the same commands in 3 sessions give one tool draft of the exact
// commands, with no model claim. A failed chain and a one-session chain stay as evidence: they
// do not reach the gate alone.
func TestExactRepeatTool(t *testing.T) {
	failed := command("go test ./...")
	exit := 1
	failed.ExitStatus = &exit
	f := newFixture(t)
	f.cli.reply = noClaims
	chain := []store.CommandRecord{command("go vet ./..."), command("go test ./...")}
	f.commandSession("s1", turn{"check the code", chain})
	f.commandSession("s2", turn{"check the code", []store.CommandRecord{command("go vet ./..."), failed}})
	f.commandSession("s3", turn{"check the code", chain})
	f.mustRun()
	if ps := f.toolProposals(); len(ps) != 0 {
		t.Fatalf("tool drafts = %+v, want none: the failed chain does not count", ps)
	}
	f.commandSession("s4", turn{"check the code", chain})
	f.mustRun()
	f.mustRun() // a second pass adds nothing
	ps := f.toolProposals()
	if len(ps) != 1 {
		t.Fatalf("tool drafts = %+v, want one", ps)
	}
	c, err := tool.Parse(ps[0].Text)
	if err != nil {
		t.Fatalf("Parse(draft) = %v", err)
	}
	want := [][]string{{"go", "vet", "./..."}, {"go", "test", "./..."}}
	if !slices.EqualFunc(c.Steps, want, slices.Equal) || c.Cwd != shop || c.Name != "go-vet" || len(c.Inputs) != 0 ||
		c.ReadOnly || c.TimeoutS != draftTimeout {
		t.Errorf("draft = %+v, want the exact steps %q in %s", c, want, shop)
	}
	if n := f.count(`SELECT COUNT(*) FROM candidates WHERE kind = 'command_chain' AND sessions = 3`); n != 1 {
		t.Errorf("command candidates with 3 sessions = %d, want 1", n)
	}
}

// AC: a command record with secret found or truncated gives no tool draft: no exact draft, and
// a model tool claim whose evidence holds one adds nothing.
func TestToolHeldBack(t *testing.T) {
	for name, set := range map[string]func(*store.CommandRecord){
		"secret found": func(c *store.CommandRecord) { c.SecretFound, c.Argv = true, "" },
		"truncated":    func(c *store.CommandRecord) { c.Truncated = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.cli.reply = noClaims
			held := command("pnpm test --filter api")
			set(&held)
			for i := range 3 {
				f.commandSession(fmt.Sprintf("s%d", i), turn{"run the api tests", []store.CommandRecord{held}})
			}
			f.mustRun()
			if ps := f.toolProposals(); len(ps) != 0 {
				t.Fatalf("tool drafts = %+v, want none", ps)
			}

			// A failed held-back command is a failure ref; a tool claim over it adds nothing.
			exit := 1
			held.ExitStatus = &exit
			f.cli.reply = toolReply(nil)
			for _, pkg := range []string{"api", "web", "ui"} {
				f.commandSession("m"+pkg, turn{"run the tests", []store.CommandRecord{command("pnpm test --filter " + pkg)}},
					turn{"run them again", []store.CommandRecord{held}})
			}
			f.mustRun()
			if ps := f.toolProposals(); len(ps) != 0 {
				t.Errorf("tool drafts = %+v, want none from a claim over a held-back command", ps)
			}
			for _, b := range f.batches() {
				if b.Status == store.BatchInvalid || b.Status == store.BatchFailed {
					t.Errorf("batch %+v, want the batches committed", b)
				}
			}
		})
	}
}

// AC: offline tests with a fake CLI: a chain with a judgment step gives a skill draft, not a
// tool draft. The same commands repeat in 3 sessions, but the model says skill: the restart
// needs a judgment. So the exact repeat gives no tool draft either.
func TestJudgmentChainIsSkill(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = skillReply(nil)
	for i := range 3 {
		f.commandSession(fmt.Sprintf("s%d", i), turn{"check the service and restart it when it is down", []store.CommandRecord{
			command("systemctl status shop"), command("systemctl restart shop")}})
	}
	f.mustRun()
	if ps := f.toolProposals(); len(ps) != 0 {
		t.Errorf("tool drafts = %+v, want none", ps)
	}
	if n := f.count(`SELECT COUNT(*) FROM items WHERE kind = 'skill' AND state = 'draft'`); n != 1 {
		t.Errorf("skill drafts = %d, want 1", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM items WHERE kind = 'tool'`); n != 0 {
		t.Errorf("tool items = %d, want none", n)
	}
}

// A tool claim that breaks a rule is refused, and gives no draft: Brain never writes new
// program logic, and the E17-S3 contract rules apply.
func TestToolReplyRefused(t *testing.T) {
	for name, set := range map[string]map[string]any{
		"a new step":           {"commands": [][]string{{"pnpm", "test", "--filter", "{package}"}, {"rm", "-rf", "dist"}}},
		"another program":      {"commands": [][]string{{"npm", "test", "--filter", "{package}"}}},
		"input in the program": {"commands": [][]string{{"{package}", "test", "--filter", "api"}}},
		"part of a word":       {"commands": [][]string{{"pnpm", "test", "--filter", "@shop/{package}"}}},
		"no pattern":           {"inputs": []any{map[string]any{"name": "package", "type": "string", "pattern": ""}}},
		"no input":             {"inputs": nil},
		"bad name":             {"name": "Test Package"},
		"no effects":           {"effects": nil},
		"no commands":          {"commands": nil},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.cli.reply = toolReply(set)
			f.commandSession("s1", turn{"run the api tests", []store.CommandRecord{command("pnpm test --filter api")}})
			f.mustRun()
			if bs := f.batches(); len(bs) != 1 || bs[0].Status != store.BatchInvalid {
				t.Errorf("batches = %+v, want one invalid batch", bs)
			}
			if n := f.count(`SELECT COUNT(*) FROM proposals`) + f.count(`SELECT COUNT(*) FROM items`); n != 0 {
				t.Errorf("proposals and items = %d, want none", n)
			}
		})
	}
}

// makeTool runs the review action "make this a tool" on candidate id.
func (f *fixture) makeTool(id int64) (int64, error) {
	f.t.Helper()
	var pid int64
	err := f.s.Write(context.Background(), func(tx *sql.Tx) (err error) {
		pid, err = MakeTool(context.Background(), tx, id, f.now)
		return err
	})
	return pid, err
}

// commandCandidate returns the id of the one command candidate.
func (f *fixture) commandCandidate() int64 {
	f.t.Helper()
	var id int64
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT id FROM candidates WHERE kind = 'command_chain'`).Scan(&id)
	})
	return id
}

// AC: "make this a tool" after one example gives a draft: the exact commands, with no model
// call, no new evidence and no watermark change. A model draft with an input then comes through
// the E13-S5 request route: ask for a change of the draft.
func TestMakeTool(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	sid := f.commandSession("s1", turn{"run the api tests", []store.CommandRecord{command("pnpm test --filter api")}})
	f.mustRun()
	id := f.commandCandidate()
	wm, calls := f.watermark(sid, "ws-A"), f.cli.calls()
	pid, err := f.makeTool(id)
	if err != nil {
		t.Fatalf("MakeTool(%d) = %v", id, err)
	}
	ps := f.toolProposals()
	if len(ps) != 1 || ps[0].ID != pid || ps[0].CandidateID == nil || *ps[0].CandidateID != id {
		t.Fatalf("tool drafts = %+v, want proposal %d of candidate %d", ps, pid, id)
	}
	c, err := tool.Parse(ps[0].Text)
	if err != nil || !slices.Equal(c.Argv, []string{"pnpm", "test", "--filter", "api"}) || c.Name != "pnpm-test-filter-api" {
		t.Errorf("Parse(draft) = %+v, %v, want the exact command", c, err)
	}
	if f.cli.calls() != calls || f.watermark(sid, "ws-A") != wm ||
		f.count(`SELECT count FROM candidates WHERE id = `+fmt.Sprint(id)) != 1 {
		t.Errorf("make this a tool called the model, moved the watermark or added evidence")
	}
	if _, err := f.makeTool(id); err == nil {
		t.Error("a second MakeTool() = nil, want a refusal: the draft waits for review")
	}

	f.ask(f.proposed(), "use an input for the package")
	f.cli.reply = toolReply(nil)
	f.mustRun()
	ps = f.toolProposals()
	if len(ps) != 1 || ps[0].ID == pid {
		t.Fatalf("tool drafts = %+v, want one new draft", ps)
	}
	if c, err := tool.Parse(ps[0].Text); err != nil || c.Inputs["package"].Pattern == "" {
		t.Errorf("Parse(redraft) = %+v, %v, want the package input", c, err)
	}
	if !strings.Contains(f.cli.prompts[len(f.cli.prompts)-1], redraftKind(store.KindTool)) {
		t.Error("the redraft prompt does not ask for a tool")
	}
	if f.watermark(sid, "ws-A") != wm || f.count(`SELECT count FROM candidates WHERE id = `+fmt.Sprint(id)) != 1 {
		t.Error("the request route moved the watermark or added evidence")
	}
}

// "Make this a tool" refuses an example that Brain never drafts a tool from, a candidate that
// is no command candidate, and the learned automation case, whose evidence is stops.
func TestMakeToolRefused(t *testing.T) {
	for name, cmd := range map[string]store.CommandRecord{
		"secret found": {Argv: "", CWD: shop, SecretFound: true},
		"truncated":    {Argv: "pnpm test --filter api", CWD: shop, Truncated: true},
		"shell":        {Argv: "cd api && pnpm test", CWD: shop},
		"brain run":    {Argv: "brain run test-package --package api", CWD: shop},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.commandSession("s1", turn{"run the api tests", []store.CommandRecord{cmd}})
			var id int64
			f.write(func(ctx context.Context, tx *sql.Tx) error {
				var obs int64
				if err := tx.QueryRowContext(ctx, `SELECT observation_id FROM command_records`).Scan(&obs); err != nil {
					return err
				}
				claim, err := store.ClaimID(ctx, tx, store.ClaimKey{Situation: "testing", Rule: "run the tests"}, t0)
				if err != nil {
					return err
				}
				id, err = store.Attach(ctx, tx, store.Candidate{ClaimID: claim, Scope: store.ScopeWorkspace, WorkspaceKey: "ws-A",
					Kind: store.CandidateCommandChain}, obs, store.DefaultGates(), signal.Supports)
				return err
			})
			if _, err := f.makeTool(id); err == nil {
				t.Error("MakeTool() = nil, want a refusal")
			}
			if ps := f.toolProposals(); len(ps) != 0 {
				t.Errorf("tool drafts = %+v, want none", ps)
			}
		})
	}
	t.Run("not a command candidate", func(t *testing.T) {
		f := newFixture(t)
		f.session("c", true, line{"ws-A", pnpm, t0})
		f.mustRun()
		var id int64
		f.write(func(ctx context.Context, tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT id FROM candidates`).Scan(&id)
		})
		if _, err := f.makeTool(id); err == nil || !strings.Contains(err.Error(), "only a command candidate") {
			t.Errorf("MakeTool() = %v, want a refusal of a correction candidate", err)
		}
		// The learned automation case is a command chain candidate with no command evidence.
		f.write(func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE candidates SET kind = 'command_chain'`)
			return err
		})
		if _, err := f.makeTool(id); err == nil || !strings.Contains(err.Error(), "no command") {
			t.Errorf("MakeTool() = %v, want a refusal of a candidate with no command", err)
		}
	})
}

func TestDraftName(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"pnpm", "test", "--filter", "api"}, "pnpm-test-filter-api"},
		{[]string{"/usr/local/bin/go", "test", "./..."}, "go-test"},
		{[]string{"make", "check-all-the-things-in-this-very-long-target-name"}, "make-check-all-the-things-in-this-very"},
		{[]string{"make", strings.Repeat("a", 50)}, "make"},
	} {
		t.Run(c.want, func(t *testing.T) {
			if got := draftName(c.argv); got != c.want {
				t.Errorf("draftName(%q) = %q, want %q", c.argv, got, c.want)
			}
		})
	}
}

// A forget stops the exact repeat: the tombstone of a source stops its chain, though the claim
// key text is gone (§6.7).
func TestExactRepeatForgotten(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		return store.InsertTombstone(ctx, tx, store.Tombstone{ClaimID: "cl_old", SourceKeys: []string{"s0-2"}, ForgottenAt: t0})
	})
	for i := range 3 {
		f.commandSession(fmt.Sprintf("s%d", i), turn{"run the tests", []store.CommandRecord{command("go test ./...")}})
	}
	f.mustRun()
	if ps := f.toolProposals(); len(ps) != 0 {
		t.Errorf("tool drafts = %+v, want none: one source is forgotten", ps)
	}
}

// The exact repeat keeps the case and the folder of the commands: make Foo and make foo, or
// one command in two folders, are not the same repeat.
func TestExactRepeatKey(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	other := command("make Foo")
	other.CWD = "/src/blog"
	for i, c := range []store.CommandRecord{command("make Foo"), command("make foo"), other, command("make Foo")} {
		f.commandSession(fmt.Sprintf("s%d", i), turn{"build it", []store.CommandRecord{c}})
	}
	f.mustRun()
	if ps := f.toolProposals(); len(ps) != 0 {
		t.Fatalf("tool drafts = %+v, want none: only 2 sessions ran make Foo in %s", ps, shop)
	}
	f.commandSession("s4", turn{"build it", []store.CommandRecord{command("make Foo")}})
	f.mustRun()
	if ps := f.toolProposals(); len(ps) != 1 {
		t.Errorf("tool drafts = %+v, want one after 3 sessions", ps)
	}
}

// "Make this a tool" takes one chain: the same command in two turns of one session is one
// command, not two steps.
func TestMakeToolOneChain(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	run := turn{"run the api tests", []store.CommandRecord{command("pnpm test --filter api")}}
	f.commandSession("s1", run, run)
	f.mustRun()
	if _, err := f.makeTool(f.commandCandidate()); err != nil {
		t.Fatal(err)
	}
	ps := f.toolProposals()
	if len(ps) != 1 {
		t.Fatalf("tool drafts = %+v, want one", ps)
	}
	if c, err := tool.Parse(ps[0].Text); err != nil || c.Argv == nil || c.Steps != nil {
		t.Errorf("Parse(draft) = %+v, %v, want one argv", c, err)
	}
}

// The model copies the whole command: the prompt holds the command record, not the excerpt
// that the hook cut at 500 characters.
func TestToolPromptWholeCommand(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	long := "pnpm test --filter api " + strings.Repeat("--reporter-option=x ", 30)
	f.commandSession("s1", turn{"run the api tests", []store.CommandRecord{command(strings.TrimSpace(long))}})
	f.mustRun()
	if p := f.cli.prompts[0]; !strings.Contains(p, strings.TrimSpace(long)) {
		t.Errorf("prompt lacks the whole command:\n%s", p)
	}
}

// The secret filter runs on the input fields of a model tool before Brain stores them.
func TestToolInputSecret(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = toolReply(map[string]any{"inputs": []any{map[string]any{"name": "package", "type": "string",
		"pattern": "^(api|sk-ant-abcdefghijklmnopqrstuvwxyz)$"}}})
	for i, pkg := range []string{"api", "web", "ui"} {
		f.commandSession(fmt.Sprintf("s%d", i), turn{"run the tests", []store.CommandRecord{command("pnpm test --filter " + pkg)}})
	}
	f.mustRun()
	ps := f.toolProposals()
	if len(ps) != 1 || strings.Contains(ps[0].Text+ps[0].Diff, "sk-ant") || !strings.Contains(ps[0].Text, "[secret]") {
		t.Errorf("tool drafts = %+v, want one with the secret replaced", ps)
	}
}

// A model tool runs in one folder: commands that ran in two folders give none, and the newest
// folder in which all of them ran wins.
func TestToolFolder(t *testing.T) {
	cmdRef := func(obs int64, argv, cwd string) ref {
		return ref{obs: obs, cmd: &store.CommandRecord{Argv: argv, CWD: cwd}}
	}
	steps := [][]string{{"pnpm", "test", "--filter", "{package}"}, {"pnpm", "lint"}}
	for _, c := range []struct {
		name string
		refs []ref
		want string
	}{
		{"two folders", []ref{cmdRef(1, "pnpm test --filter api", "/src/api"), cmdRef(2, "pnpm lint", "/src/web")}, ""},
		{"one folder", []ref{cmdRef(1, "pnpm test --filter api", "/a"), cmdRef(2, "pnpm lint", "/a")}, "/a"},
		{"newest", []ref{cmdRef(1, "pnpm test --filter api", "/a"), cmdRef(2, "pnpm lint", "/a"),
			cmdRef(3, "pnpm test --filter web", "/b"), cmdRef(4, "pnpm lint", "/b")}, "/b"},
		{"held back", []ref{cmdRef(1, "pnpm test --filter api", "/a"), {obs: 2, cmd: &store.CommandRecord{Argv: "pnpm lint",
			CWD: "/a", Truncated: true}}}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := toolFolder(steps, c.refs); got != c.want {
				t.Errorf("toolFolder() = %q, want %q", got, c.want)
			}
		})
	}
}

// The task of the M2 eval run 3 (#337): reset the local db, then run the tests.
const (
	resetTask = "Reset the local db and run the tests: run exactly ./scripts/reset-db.sh and then exactly npm test"
	resetCmd  = "./scripts/reset-db.sh"
	testCmd   = "npm test"
)

// #337 AC: the 3 chain shapes of one task in Codex give one chain key, so 3 sessions give one
// tool proposal: a sandbox retry (npm test twice, with no exit status), an approval first (npm
// test once), and a line of git status output between the two runs, which capture kept as
// unconfirmed text. Only the hook prompt record ends a turn.
func TestToolFromCodexRuns(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	f.codexSession("s5", promptRec(resetTask), codexRec(resetCmd), codexRec(testCmd), codexRec(testCmd))
	f.codexSession("s7", promptRec(resetTask), codexRec(resetCmd), codexRec(testCmd))
	f.mustRun()
	if ps := f.toolProposals(); len(ps) != 0 {
		t.Fatalf("tool drafts after 2 sessions = %+v, want none below the gate", ps)
	}
	f.codexSession("s9", promptRec(resetTask), codexRec(resetCmd), codexRec(testCmd), paneRec("?? src/routes/"),
		codexRec(testCmd))
	f.mustRun()
	ps := f.toolProposals()
	if len(ps) != 1 {
		t.Fatalf("tool drafts = %+v, want one", ps)
	}
	c, err := tool.Parse(ps[0].Text)
	want := [][]string{{resetCmd}, {"npm", "test"}}
	if err != nil || !slices.EqualFunc(c.Steps, want, slices.Equal) || c.Name != "reset-db-sh" || c.Cwd != shop {
		t.Errorf("draft = %+v, %v; want reset-db-sh with the steps %q in %s", c, err, want, shop)
	}
	if n := f.count(`SELECT COUNT(*) FROM candidates WHERE kind = 'command_chain'`); n != 1 {
		t.Errorf("command candidates = %d, want 1: one task gives one chain", n)
	}
}

// yourTool is a tool item in a workspace that you made: its claim is no claim of a chain.
type yourTool struct {
	key, name, cwd string
	pending        bool // a draft item with a pending proposal; else an active item
	steps          [][]string
}

// addTool stores tool y and returns its item id.
func (f *fixture) addTool(y yourTool) int64 {
	f.t.Helper()
	text, err := draftToml{Name: y.name, Description: "Run the recorded commands", Cwd: y.cwd, TimeoutS: draftTimeout,
		Effects: draftEffects}.text(y.steps)
	if err != nil {
		f.t.Fatal(err)
	}
	state := store.StateActive
	if y.pending {
		state = store.StateDraft
	}
	var id int64
	f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		id, _, err = store.CreateItem(ctx, tx, store.Item{ClaimID: "cl_" + y.key + "_" + y.name, Kind: store.KindTool,
			Created: t0}, store.ItemRevision{Text: text, Scope: store.ScopeWorkspace, WorkspaceKey: &y.key, State: state,
			Author: store.AuthorYou, Origin: store.OriginReview, Created: t0})
		if err != nil || !y.pending {
			return err
		}
		base := int64(1)
		_, err = store.InsertProposal(ctx, tx, store.Proposal{ItemID: &id, BaseRevision: &base, Text: text,
			Scope: store.ScopeWorkspace, Target: y.key, Status: store.ProposalPending, Created: t0})
		return err
	})
	return id
}

// #337 AC: a chain that only adds a rerun to the steps of an active or pending tool makes no new
// tool proposal and uses no cap, also when the tool has another claim, as a tool that you made.
// A tool whose own steps hold the rerun, as the duplicate draft of the M2 eval run 3, is the
// same tool too. The same commands in another folder are another tool.
func TestNoDuplicateTool(t *testing.T) {
	reset, test := []string{resetCmd}, []string{"npm", "test"}
	for _, tc := range []struct {
		name      string
		tool      yourTool
		proposals int
	}{
		{"active", yourTool{"ws-A", "reset-and-test", shop, false, [][]string{reset, test}}, 0},
		{"pending", yourTool{"ws-A", "reset-and-test", shop, true, [][]string{reset, test}}, 0},
		{"rerun in the tool", yourTool{"ws-A", "reset-db-sh", shop, false, [][]string{reset, test, test}}, 0},
		{"other folder", yourTool{"ws-A", "reset-and-test", "/src/blog", false, [][]string{reset, test}}, 2},
		{"other workspace", yourTool{"ws-B", "reset-and-test", shop, false, [][]string{reset, test}}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.cli.reply = noClaims
			f.addTool(tc.tool)
			for i := range 3 {
				f.commandSession(fmt.Sprintf("s%d", i), turn{"reset the db and test", []store.CommandRecord{
					command(resetCmd), command(testCmd), command(testCmd)}})
			}
			f.mustRun()
			if n := f.count(`SELECT COUNT(*) FROM candidates WHERE kind = 'command_chain' AND sessions = 3`); n != 1 {
				t.Errorf("command candidates with 3 sessions = %d, want 1: the chain stays evidence", n)
			}
			var d Day
			f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
				d, err = f.pass().Day(ctx, tx)
				return err
			})
			// A new tool draft comes with its script proposal.
			if d.Proposals != tc.proposals {
				t.Errorf("proposals of the day = %d, want %d", d.Proposals, tc.proposals)
			}
		})
	}
}

// #339 AC: a tool draft never gets the name of an active or pending tool that a pane of its
// workspace sees: it gets that name with the first free number. A tool of another workspace
// takes no name from it.
func TestToolDraftFreeName(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	f.addTool(yourTool{"ws-A", "reset-db-sh", shop, false, [][]string{{resetCmd}, {"npm", "test"}}})
	f.addTool(yourTool{"ws-A", "reset-db-sh-2", shop, true, [][]string{{"npm", "run", "seed"}}})
	f.addTool(yourTool{"ws-B", "reset-db-sh-3", shop, false, [][]string{{"npm", "run", "seed"}}})
	for i := range 3 {
		f.commandSession(fmt.Sprintf("s%d", i), turn{"reset the db", []store.CommandRecord{command(resetCmd)}})
	}
	f.mustRun()
	var names []string
	for _, p := range f.toolProposals() {
		c, err := tool.Parse(p.Text)
		if err != nil {
			t.Fatal(err)
		}
		if p.CandidateID != nil {
			names = append(names, c.Name)
		}
	}
	if want := []string{"reset-db-sh-3"}; !slices.Equal(names, want) {
		t.Errorf("names of the new tool drafts = %q, want %q", names, want)
	}
}

// #339: the model's draft gets a free name too, and a new draft of a tool item keeps its own
// name (a change request): its own texts are no clash.
func TestFreeName(t *testing.T) {
	f := newFixture(t)
	own := f.addTool(yourTool{"ws-A", "test-package", shop, true, [][]string{{"pnpm", "test"}}})
	f.addTool(yourTool{"ws-B", "test-package-2", shop, false, [][]string{{"pnpm", "test"}}})
	text, err := draftToml{Name: "test-package", Description: "Run the tests", Cwd: shop, TimeoutS: draftTimeout,
		Effects: draftEffects}.text([][]string{{"pnpm", "test", "--filter", "api"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		key  string
		item int64
		want string
	}{{"new draft", "ws-A", 0, "test-package-2"}, {"its own item", "ws-A", own, "test-package"},
		{"global", "", 0, "test-package"}} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
				got, err = freeName(ctx, tx, text, tc.key, tc.item)
				return err
			})
			c, err := tool.Parse(got)
			if err != nil || c.Name != tc.want || c.Argv[3] != "api" {
				t.Errorf("freeName(%s) = %+v, %v; want the draft named %s", tc.name, c, err, tc.want)
			}
		})
	}
}

// #339: the extractor's tool draft gets a free name too.
func TestPlaceholderToolFreeName(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = toolReply(nil)
	f.addTool(yourTool{"ws-A", "test-package", shop, false, [][]string{{"pnpm", "test"}}})
	for i, pkg := range []string{"api", "web", "api"} {
		f.commandSession(fmt.Sprintf("s%d", i), turn{"run the tests of " + pkg, []store.CommandRecord{
			command("pnpm test --filter " + pkg)}})
	}
	f.mustRun()
	ps := f.toolProposals()
	if len(ps) != 1 {
		t.Fatalf("tool drafts = %+v, want one", ps)
	}
	if c, err := tool.Parse(ps[0].Text); err != nil || c.Name != "test-package-2" {
		t.Errorf("draft = %+v, %v; want the name test-package-2", c, err)
	}
}
