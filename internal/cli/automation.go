package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// [NEW] the subcommands, their flags and their output.
const automationUsage = `usage:
  brain automation add <tool> --folder <dir> --cli claude|codex
                             propose to run an approved tool in a folder after each stop of the agent
  brain automation list      list the automations of this workspace
  brain automation pause <item> | resume <item>   stop new runs, or start them again
  brain automation remove <item>                  stop all future runs
  brain automation cancel <run>                   stop an active run
  brain automation launch --run <id> --binding <herdr session> --store <folder>
                             run one claimed run (brained starts it)`

// Automation runs `brain automation` with args after "automation" (§10.4, E17-S10).
func Automation(ctx context.Context, e Env, args []string) (err error) {
	if len(args) == 0 {
		return errors.New(automationUsage)
	}
	sub, rest := args[0], args[1:]
	var run func(caller) error
	switch sub {
	case "launch":
		l, err := brained.ParseLaunch(rest)
		if err != nil {
			return err
		}
		// brain automation cancel stops the run with SIGTERM: the tool's process group stops too.
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		return launcher{Env: e, pid: os.Getpid()}.run(ctx, l)
	case "add":
		a, err := parseAdd(rest)
		if err != nil {
			return fmt.Errorf("%w\n%s", err, automationUsage)
		}
		run = func(cl caller) error { return cl.addAutomation(ctx, a) }
	case "list":
		if len(rest) != 0 {
			return fmt.Errorf("list takes no arguments\n%s", automationUsage)
		}
		run = func(cl caller) error { return cl.listAutomations(ctx) }
	case "pause", "resume", "remove", "cancel":
		if len(rest) != 1 {
			return fmt.Errorf("want one id, got %d\n%s", len(rest), automationUsage)
		}
		id, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("bad id %q\n%s", rest[0], automationUsage)
		}
		run = func(cl caller) error {
			switch sub {
			case "remove":
				return cl.removeAutomation(ctx, id)
			case "cancel":
				return cl.cancelRun(ctx, id)
			}
			return cl.setAutomation(ctx, id, sub == "resume")
		}
	default:
		return fmt.Errorf("unknown subcommand %q\n%s", sub, automationUsage)
	}
	c, err := e.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	return run(newCaller(e, c))
}

// addArgs are the arguments of brain automation add.
type addArgs struct {
	tool, folder, cli string
}

func parseAdd(args []string) (addArgs, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return addArgs{}, errors.New("want the tool name first")
	}
	a := addArgs{tool: args[0]}
	fs := flag.NewFlagSet("automation add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.folder, "folder", "", "")
	fs.StringVar(&a.cli, "cli", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return addArgs{}, err
	}
	switch {
	case fs.NArg() != 0:
		return addArgs{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case a.folder == "" || a.cli == "":
		return addArgs{}, errors.New("want --folder and --cli")
	}
	return a, nil
}

// addAutomation proposes a binding of approved tool a.tool in folder a.folder on the stop
// trigger of CLI a.cli, in the caller's workspace (§10.4). The approval checks refuse it at
// once (store.CheckBinding). A binding of the same tool, folder and CLI that is active already
// gets a proposal to pin the new contract hash of the tool: you approve it again.
func (cl caller) addAutomation(ctx context.Context, a addArgs) error {
	if cl.keyErr != nil {
		return fmt.Errorf("an automation is a workspace item: %w", cl.keyErr)
	}
	trigger, ok := store.StopTrigger(a.cli)
	if !ok {
		return fmt.Errorf("%s has no tested stop condition, so it cannot get an automation: %w", a.cli, store.ErrBindingRefused)
	}
	// A leading ~ stays, as in a tool cwd; any other folder is absolute and clean.
	folder := a.folder
	if folder != "~" && !strings.HasPrefix(folder, "~/") {
		var err error
		if folder, err = filepath.Abs(folder); err != nil {
			return err
		}
	}
	var pid int64
	var b store.Binding
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		t, err := store.ToolByName(ctx, tx, a.tool, cl.key)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("tool %q: %w", a.tool, errNotFound)
		}
		if err != nil {
			return err
		}
		if t.ApprovedHash == nil || *t.ApprovedHash != t.ContractHash {
			return fmt.Errorf("tool %s (item %d) is not approved: approve it with brain review first", t.Name, t.ID)
		}
		b = store.Binding{Tool: t.Name, ToolID: t.ID, ContractHash: t.ContractHash, Folder: folder, CLI: a.cli,
			Trigger: trigger}
		if err := store.CheckBinding(ctx, tx, b, cl.key, lookupEnv(cl.Environ, "HOME")); err != nil {
			return err
		}
		pid, err = cl.proposeBinding(ctx, tx, b)
		return err
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "proposal %d: run tool %s in %s after each stop of a %s agent; review it with brain review %d\n",
		pid, b.Tool, b.Folder, b.CLI, pid)
	return err
}

// proposeBinding writes a pending proposal of binding b in the caller's workspace and returns
// its id: a change of the active binding of the same tool, folder and CLI, or a new draft item.
func (cl caller) proposeBinding(ctx context.Context, tx *sql.Tx, b store.Binding) (int64, error) {
	as, err := store.ActiveAutomations(ctx, tx)
	if err != nil {
		return 0, err
	}
	i := slices.IndexFunc(as, func(a store.Automation) bool {
		return a.WorkspaceKey == cl.key && a.ToolID == b.ToolID && a.Folder == b.Folder && a.CLI == b.CLI
	})
	text, now := b.Text(), cl.Now()
	p := store.Proposal{Text: text, Scope: store.ScopeWorkspace, Target: cl.target(store.ScopeWorkspace),
		ExpectedBenefit: b.Benefit(), Status: store.ProposalPending, Created: now}
	if i >= 0 {
		if as[i].ContractHash == b.ContractHash {
			return 0, fmt.Errorf("automation %d binds this tool in this folder already", as[i].ID)
		}
		it, cur, err := cl.item(ctx, tx, as[i].ID)
		if err != nil {
			return 0, err
		}
		p.ItemID, p.BaseRevision, p.Diff = &it.ID, &it.CurrentRevision, lineDiff(cur.Text, text)
		return store.InsertProposal(ctx, tx, p)
	}
	claim, err := newClaimID()
	if err != nil {
		return 0, err
	}
	item, err := store.InsertItem(ctx, tx, store.Item{ClaimID: claim, CurrentRevision: 1, Kind: store.KindAutomation,
		Scope: store.ScopeWorkspace, WorkspaceKey: &cl.key, State: store.StateDraft, Created: now})
	if err != nil {
		return 0, err
	}
	base := int64(1)
	p.ItemID, p.BaseRevision, p.Diff = &item, &base, lineDiff("", text)
	pid, err := store.InsertProposal(ctx, tx, p)
	if err != nil {
		return 0, err
	}
	return pid, store.InsertItemRevision(ctx, tx, store.ItemRevision{ItemID: item, Revision: 1, Text: text,
		Scope: store.ScopeWorkspace, WorkspaceKey: &cl.key, State: store.StateDraft, Author: store.AuthorBrain,
		Origin: store.OriginRemember, ProposalID: &pid, Created: now})
}

// lineDiff is the diff of text before to text after: "- " and each line that after lacks,
// then "+ " and each line that before lacks.
func lineDiff(before, after string) string {
	old, next := strings.Split(strings.TrimSpace(before), "\n"), strings.Split(strings.TrimSpace(after), "\n")
	var d []string
	for _, l := range old {
		if l != "" && !slices.Contains(next, l) {
			d = append(d, "- "+l)
		}
	}
	for _, l := range next {
		if !slices.Contains(old, l) {
			d = append(d, "+ "+l)
		}
	}
	return strings.Join(d, "\n")
}

// listAutomations lists the active automations of the caller's workspace, each with its state:
// on, paused, or stopped with the reason (output.AutomationMayRun).
func (cl caller) listAutomations(ctx context.Context) error {
	var lines []string
	out := filepath.Join(cl.Binding.Store, outDir)
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		as, err := store.ActiveAutomations(ctx, tx)
		if err != nil {
			return err
		}
		for _, a := range as {
			if a.WorkspaceKey != cl.key {
				continue
			}
			t, err := store.ToolByID(ctx, tx, a.ToolID)
			if err != nil {
				return err
			}
			state := "on"
			switch _, err := output.AutomationMayRun(ctx, tx, out, a.ID); {
			case errors.Is(err, output.ErrMayNotRun):
				state = "stopped: " + err.Error()
			case err != nil:
				return err
			}
			if !a.Enabled {
				state = "paused"
			}
			last := "none"
			if !a.LastRun.IsZero() {
				last = fmt.Sprintf("%s at %s", a.LastResult, a.LastRun.Format("2006-01-02 15:04"))
			}
			lines = append(lines, fmt.Sprintf("automation %d: %s\n  tool %s (item %d) in %s after each stop of a %s agent (%s); last run: %s",
				a.ID, state, t.Name, t.ID, a.Folder, a.CLI, a.Trigger, last))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		_, err = fmt.Fprintf(cl.Stdout, "no automations in %s\n", cl.target(store.ScopeWorkspace))
		return err
	}
	_, err = fmt.Fprintln(cl.Stdout, strings.Join(lines, "\n"))
	return err
}

// automation reads automation item id and its current revision, when the caller sees it and
// it is active.
func (cl caller) automation(ctx context.Context, tx *sql.Tx, id int64) (store.Item, store.ItemRevision, error) {
	it, cur, err := cl.item(ctx, tx, id)
	switch {
	case err != nil:
		return store.Item{}, store.ItemRevision{}, err
	case it.Kind != store.KindAutomation:
		return store.Item{}, store.ItemRevision{}, fmt.Errorf("item %d is a %s, not an automation", id, it.Kind)
	case it.State != store.StateActive:
		return store.Item{}, store.ItemRevision{}, fmt.Errorf("automation %d is %s", id, it.State)
	}
	return it, cur, nil
}

// setAutomation turns automation id on, or pauses it, in one transaction (§10.4): a paused
// binding claims nothing, and a claimed run with no launch mark does not start (E17-S11).
func (cl caller) setAutomation(ctx context.Context, id int64, on bool) error {
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		if _, _, err := cl.automation(ctx, tx, id); err != nil {
			return err
		}
		return store.SetAutomationEnabled(ctx, tx, id, on)
	})
	if err != nil {
		return err
	}
	msg := "paused: it starts no new run (brain automation resume %d turns it on)"
	if on {
		msg = "on: it runs after the next stop (brain automation pause %d stops new runs)"
	}
	_, err = fmt.Fprintf(cl.Stdout, "automation %d is "+msg+"\n", id, id)
	return err
}

// removeAutomation retires automation id in one transaction, so it starts no run again
// (§10.4). Effects of runs that happened stay.
func (cl caller) removeAutomation(ctx context.Context, id int64) error {
	var next store.ItemRevision
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		it, cur, err := cl.automation(ctx, tx, id)
		if err != nil {
			return err
		}
		next = nextRevision(cur, cl.Now())
		next.State = store.StateRetired
		if _, err := store.WriteRevision(ctx, tx, it.CurrentRevision, next); err != nil {
			return err
		}
		next.Revision = it.CurrentRevision + 1
		return nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cl.Stdout, "automation %d removed at revision %d: it starts no run again; effects that already happened stay\n",
		id, next.Revision)
	return errors.Join(err, cl.deliver(ctx, nil))
}
