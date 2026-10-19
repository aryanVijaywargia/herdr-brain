package brained

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// Launch is what the `brain automation launch` process of one claimed run gets: ids only. It
// resolves the pane of the run again from its environment before it runs anything.
type Launch struct {
	Run     int64  // the automation_runs row
	Binding string // the herdr session of the binding
	Store   string // the store folder of the binding
}

const launchUsage = `usage: brain automation launch --run <id> --binding <herdr session> --store <folder>`

// Args returns the arguments of `brain automation launch` for l.
func (l Launch) Args() []string {
	return []string{"automation", "launch", "--run", strconv.FormatInt(l.Run, 10), "--binding", l.Binding, "--store", l.Store}
}

// ParseLaunch reads the arguments of `brain automation launch` after "launch", as Args writes
// them.
func ParseLaunch(args []string) (Launch, error) {
	var l Launch
	fs := flag.NewFlagSet("launch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Int64Var(&l.Run, "run", 0, "")
	fs.StringVar(&l.Binding, "binding", "", "")
	fs.StringVar(&l.Store, "store", "", "")
	if err := fs.Parse(args); err != nil {
		return Launch{}, fmt.Errorf("%w\n%s", err, launchUsage)
	}
	if fs.NArg() != 0 || l.Run <= 0 || l.Binding == "" || !filepath.IsAbs(l.Store) {
		return Launch{}, fmt.Errorf("bad arguments %q\n%s", args, launchUsage)
	}
	return l, nil
}

// StartLaunch starts `brain automation launch` for l with environ, detached, as Start does: the
// run is Brain's own process, and it outlives brained.
func StartLaunch(ctx context.Context, environ []string, l Launch) error {
	if err := detach(ctx, environ, l.Args()); err != nil {
		return fmt.Errorf("start brain automation launch: %w", err)
	}
	return nil
}

// eventKey is the event key of a stop that joined agent state a (method P of the E17-S1
// test). state_change_seq starts again at a herdr server restart, and herdr resumes the
// same agent session in the same pane, so the key holds terminal_id, which changes then. [NEW]
// the text form.
func eventKey(a herdr.Agent) string {
	return fmt.Sprintf("%s|%s|%s|%d", a.PaneID, a.TerminalID, a.Session, a.Seq)
}

// baselineAgent sets the joined status of pane id from agent.get. After a start, a reconnect
// or a new stream of brained, the current state is a baseline, not a stop. A
// failed read leaves no status, so the next stop does not count.
func (l *loop) baselineAgent(ctx context.Context, id string, p *pane) {
	p.joined = ""
	if !p.auto {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	a, err := l.d.sock.Agent(ctx, id)
	if err != nil {
		l.report(fmt.Errorf("automation baseline of pane %s: %w", id, err))
		return
	}
	p.joined = a.Status
}

// automate handles status event ev of pane id for the automations (method P of the E17-S1
// test). It calls agent.get at once. The event joins only when agent.get gives the event
// status; an event that does not join changes nothing. A stop is a joined idle or done after a
// joined working, with a herdr agent session id: the source agent is the binding, the harness
// and that id. A user stop (Esc) gives the same change as a normal end, so it runs too.
func (l *loop) automate(ctx context.Context, id string, p *pane, ev herdr.Event) {
	if !p.auto || l.gate == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	a, err := l.d.sock.Agent(cctx, id)
	cancel()
	if err != nil {
		l.report(fmt.Errorf("automation: event of pane %s: %w", id, err))
		return
	}
	if a.Status != ev.Status {
		return
	}
	prev := p.joined
	p.joined = a.Status
	switch {
	case prev != herdr.StatusWorking || a.Status != herdr.StatusIdle && a.Status != herdr.StatusDone:
		return
	case a.Session == "":
		l.report(fmt.Errorf("automation: skipped the stop of pane %s: herdr gives no agent session id", id))
		return
	case a.PaneID != id || a.WorkspaceID != p.workspaceID:
		l.report(fmt.Errorf("automation: skipped the stop of pane %s: the pane moved", id))
		return
	}
	l.report(l.claim(ctx, *l.gate, a))
}

// claim claims a run of each automation of the workspace of stop a whose CLI and folder are
// the ones of the agent, and starts the launch process of each new claim. The
// claim checks the binding in its transaction: a paused, removed or undone binding, or one
// whose tool may not run, claims nothing. A second claim of the event key inserts nothing.
func (l *loop) claim(ctx context.Context, g Gate, a herdr.Agent) (err error) {
	key, err := g.workspaceKey(a.WorkspaceID)
	if err != nil || key == "" {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	st, err := store.Open(cctx, g.Binding.Store, busyTimeout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	home := lookup(l.d.environ, "HOME")
	var runs []int64
	err = st.Write(cctx, func(tx *sql.Tx) error {
		runs = nil
		as, err := store.ActiveAutomations(cctx, tx)
		if err != nil {
			return err
		}
		for _, at := range as {
			if at.WorkspaceKey != key || at.CLI != a.Agent || !at.In(a.Cwd, home) {
				continue
			}
			_, err := output.AutomationMayRun(cctx, tx, filepath.Join(g.Binding.Store, outDir), at.ID)
			if errors.Is(err, output.ErrMayNotRun) {
				continue
			}
			if err != nil {
				return err
			}
			id, err := store.ClaimRun(cctx, tx, store.AutomationRun{AutomationID: at.ID, EventKey: eventKey(a),
				PaneID: a.PaneID, WorkspaceID: a.WorkspaceID, Claimed: l.d.now().UTC()})
			if err != nil {
				return err
			}
			if id != 0 {
				runs = append(runs, id)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("automation claim of pane %s: %w", a.PaneID, err)
	}
	var errs []error
	for _, id := range runs {
		env := paneEnv(l.d.environ, g.Binding, a.PaneID, a.WorkspaceID)
		errs = append(errs, l.d.launch(ctx, env, Launch{Run: id, Binding: g.Binding.Session, Store: g.Binding.Store}))
	}
	return errors.Join(errs...)
}

// automated returns the Brain keys of the workspaces that have an enabled automation, read in
// tx: brained follows the agent panes of those workspaces also while learning is off.
func automated(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	as, err := store.ActiveAutomations(ctx, tx)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, a := range as {
		if a.Enabled {
			keys[a.WorkspaceKey] = true
		}
	}
	return keys, nil
}
