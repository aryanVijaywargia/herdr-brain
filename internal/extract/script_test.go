package extract

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// scriptProposals returns the script proposals.
func (f *fixture) scriptProposals() []store.Proposal {
	f.t.Helper()
	var ps []store.Proposal
	for _, p := range f.proposals() {
		if p.Script() {
			ps = append(ps, p)
		}
	}
	return ps
}

// checkChain is the chain of the script tests: the agent checks the code.
var checkChain = []store.CommandRecord{command("go vet ./..."), command("go test ./...")}

// AC: a chain in 3 sessions gives one script proposal, beside the tool draft of the same chain:
// a proposal with no item, the script text and its diff from no file.
func TestScriptProposal(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	for i := range 2 {
		f.commandSession(fmt.Sprintf("s%d", i), turn{"check the code", checkChain})
	}
	f.mustRun()
	if ps := f.scriptProposals(); len(ps) != 0 {
		t.Fatalf("script proposals = %+v, want none below the gate", ps)
	}
	f.commandSession("s2", turn{"check the code", checkChain})
	f.mustRun()
	f.mustRun() // a second pass adds nothing
	ps := f.scriptProposals()
	if len(ps) != 1 {
		t.Fatalf("script proposals = %+v, want one", ps)
	}
	want := "#!/bin/sh\n# go-vet: run go vet ./..., then go test ./...\n" +
		"# Brain does not install this script: save it where you want it and make it executable.\n" +
		"set -e\ncd '/src/shop'\ngo vet ./...\ngo test ./...\n"
	p := ps[0]
	if p.Text != want || p.Diff != store.SkillDiff("", want) || p.Status != store.ProposalPending ||
		p.Scope != store.ScopeWorkspace || p.BaseRevision != nil || p.PermissionWarning {
		t.Errorf("script proposal = %+v, want the pending workspace script\n%s", p, want)
	}
	tools := f.toolProposals()
	if len(tools) != 1 || p.CandidateID == nil || tools[0].CandidateID == nil || *tools[0].CandidateID != *p.CandidateID {
		t.Errorf("tool drafts = %+v, want one of the candidate of the script", tools)
	}
	if n := f.count(`SELECT COUNT(*) FROM items`); n != 1 {
		t.Errorf("items = %d, want the tool draft only: a script has no item", n)
	}
}

// A chain of one command is no chain: it gives a tool draft, and no script.
func TestScriptOneCommand(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	for i := range 3 {
		f.commandSession(fmt.Sprintf("s%d", i), turn{"run the tests", []store.CommandRecord{command("go test ./...")}})
	}
	f.mustRun()
	if ps := f.scriptProposals(); len(ps) != 0 || len(f.toolProposals()) != 1 {
		t.Errorf("script proposals = %+v, tool drafts = %d, want no script and one tool draft", ps, len(f.toolProposals()))
	}
}

// AC: a chain with a secret or a truncated command gives no proposal.
func TestScriptHeldBack(t *testing.T) {
	for name, set := range map[string]func(*store.CommandRecord){
		"secret found": func(c *store.CommandRecord) { c.SecretFound, c.Argv = true, "" },
		"truncated":    func(c *store.CommandRecord) { c.Truncated = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.cli.reply = noClaims
			held := command("go test ./...")
			set(&held)
			for i := range 3 {
				f.commandSession(fmt.Sprintf("s%d", i), turn{"check the code", []store.CommandRecord{command("go vet ./..."), held}})
			}
			f.mustRun()
			if n := f.count(`SELECT COUNT(*) FROM proposals`); n != 0 {
				t.Errorf("proposals = %d, want none", n)
			}
		})
	}
}

// A rejection of the claim in its scope stops a new script proposal (E12-S3).
func TestScriptRejected(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	for i := range 2 {
		f.commandSession(fmt.Sprintf("s%d", i), turn{"check the code", checkChain})
	}
	f.mustRun()
	id := f.commandCandidate()
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		c, err := store.CandidateByID(ctx, tx, id)
		if err != nil {
			return err
		}
		_, err = store.Reject(ctx, tx, c, "I run them by hand", f.now)
		return err
	})
	f.commandSession("s2", turn{"check the code", checkChain})
	f.mustRun()
	if n := f.count(`SELECT COUNT(*) FROM proposals`); n != 0 {
		t.Errorf("proposals = %d, want none after the rejection", n)
	}
}

// AC #355: a tool draft and its script count as one proposal of the daily cap, so a cap of one
// proposal fits both at once, and the use of the day is 1 (brain status prints it). This holds
// for the exact draft, and for the extractor's draft of the same commands, which uses the cap
// before the exact repeats run: the script beside it needs no room.
func TestScriptCap(t *testing.T) {
	exact := toolReply(map[string]any{"name": "go-vet", "commands": [][]string{{"go", "vet", "./..."}, {"go", "test", "./..."}},
		"inputs": nil})
	for name, reply := range map[string]func(context.Context, string) ([]byte, error){
		"exact draft": noClaims, "extractor draft": exact,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.cli.reply = reply
			for i := range 3 {
				f.commandSession(fmt.Sprintf("s%d", i), turn{"check the code", checkChain})
			}
			p := f.pass()
			p.Caps = Caps{Calls: 10, Proposals: 1, InputBytes: 32 << 10}
			if err := p.Run(context.Background()); err != nil {
				t.Fatalf("Run() = %v", err)
			}
			if len(f.toolProposals()) != 1 || len(f.scriptProposals()) != 1 {
				t.Fatalf("tool drafts = %d, scripts = %d, want both at a cap of 1", len(f.toolProposals()), len(f.scriptProposals()))
			}
			var d Day
			f.write(func(ctx context.Context, tx *sql.Tx) (err error) {
				d, err = p.Day(ctx, tx)
				return err
			})
			if d.Proposals != 1 || d.Full() == "" {
				t.Errorf("Day() = %+v, want 1 proposal and a full cap", d)
			}
		})
	}
}

// Forget of the tool draft item erases the script of the same chain too, and supersedes it
// (§6.7).
func TestScriptForgotten(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	for i := range 3 {
		f.commandSession(fmt.Sprintf("s%d", i), turn{"check the code", checkChain})
	}
	f.mustRun()
	tool, script := f.toolProposals()[0], f.scriptProposals()[0]
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		if err := store.EraseItem(ctx, tx, *tool.ItemID, f.now); err != nil {
			return err
		}
		left, err := store.ItemTextLeft(ctx, tx, *tool.ItemID)
		if err == nil && len(left) != 0 {
			t.Errorf("ItemTextLeft = %v, want none", left)
		}
		return err
	})
	// The target goes too, so the row is no script any more.
	for _, p := range f.proposals() {
		if p.ID == script.ID && (p.Text != "" || p.Diff != "" || p.Target != "" || p.Status != store.ProposalSuperseded) {
			t.Errorf("script proposal = %+v, want it erased and superseded", p)
		}
	}
}

// AC #355: a new tool draft and its script need room for one proposal under the daily cap, so
// the review offers both at once: at a cap of 1 the pair of the second chain waits for a later
// day, and other proposals count as before.
func TestScriptCapPair(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	for i := range 3 {
		f.commandSession(fmt.Sprintf("s%d", i), turn{"check the code", checkChain},
			turn{"build it", []store.CommandRecord{command("go build ./..."), command("go install ./...")}})
	}
	p := f.pass()
	p.Caps = Caps{Calls: 10, Proposals: 1, InputBytes: 32 << 10}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if tools, scripts := len(f.toolProposals()), len(f.scriptProposals()); tools != 1 || scripts != 1 {
		t.Fatalf("tool drafts = %d, scripts = %d, want one pair at a cap of 1", tools, scripts)
	}
	f.now = f.now.Add(25 * time.Hour)
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if tools, scripts := len(f.toolProposals()), len(f.scriptProposals()); tools != 2 || scripts != 2 {
		t.Errorf("tool drafts = %d, scripts = %d, want two pairs a day later", tools, scripts)
	}
}

// Forget of every session of the evidence deletes the candidate: its script loses its text and
// its candidate, so no later forget or scope check depends on it (§6.7).
func TestScriptSessionsForgotten(t *testing.T) {
	f := newFixture(t)
	f.cli.reply = noClaims
	var sids []int64
	for i := range 3 {
		sids = append(sids, f.commandSession(fmt.Sprintf("s%d", i), turn{"check the code", checkChain}))
	}
	f.mustRun()
	script := f.scriptProposals()[0]
	f.write(func(ctx context.Context, tx *sql.Tx) error {
		for _, sid := range sids {
			if err := store.ForgetSession(ctx, tx, sid); err != nil {
				return err
			}
		}
		return nil
	})
	for _, p := range f.proposals() {
		if p.ID == script.ID && (p.Text != "" || p.Diff != "" || p.CandidateID != nil || p.Status != store.ProposalSuperseded) {
			t.Errorf("script proposal = %+v, want it erased, with no candidate, and superseded", p)
		}
	}
}
