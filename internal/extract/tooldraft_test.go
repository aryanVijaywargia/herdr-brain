package extract

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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
// read of you, and each command a hook record of the tool with its command record. It returns
// the session id.
func (f *fixture) commandSession(id string, turns ...turn) int64 {
	f.t.Helper()
	key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyAgent, ID: id}
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
		for _, tn := range turns {
			if _, err := add(store.Observation{Source: store.SourceHerdr, Event: "working", Excerpt: tn.prompt,
				Sender: store.SenderYou}); err != nil {
				return err
			}
			for _, c := range tn.cmds {
				o, err := add(store.Observation{Source: store.SourceHook, Event: "tool", Excerpt: c.Argv,
					Sender: store.SenderTool})
				if err != nil {
					return err
				}
				c.ObservationID = o
				if err := store.AddCommandRecord(ctx, tx, c); err != nil {
					return err
				}
			}
		}
		return store.EndSession(ctx, tx, key, at)
	})
	return sid
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
// tool draft. The step that changes each time is the judgment: the model says skill.
func TestJudgmentChainIsSkill(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = skillReply(nil)
	for i, fix := range []string{"go fix ./api", "gofmt -w api", "go mod tidy"} {
		f.commandSession(fmt.Sprintf("s%d", i), turn{"make the build green", []store.CommandRecord{
			command("go build ./..."), command(fix), command("go test ./...")}})
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
			exit := 1 // a failure ref: the model can name it
			cmd.ExitStatus = &exit
			f.cli.reply = func(_ context.Context, prompt string) ([]byte, error) {
				return claudeReply(map[string]any{"claims": []any{replyOf(map[string]any{"kind": "procedure",
					"situation": "testing", "rule": "run the tests", "evidence_refs": promptRefs(prompt)})}})
			}
			f.commandSession("s1", turn{"run the api tests", []store.CommandRecord{cmd}})
			f.mustRun()
			var id int64
			f.write(func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE candidates SET kind = 'command_chain'`)
				if err != nil {
					return err
				}
				return tx.QueryRowContext(ctx, `SELECT id FROM candidates`).Scan(&id)
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
		if got := draftName(c.argv); got != c.want {
			t.Errorf("draftName(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
}
