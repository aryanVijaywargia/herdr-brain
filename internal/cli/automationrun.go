package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"syscall"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// RunHerdr holds the herdr calls of an automation run.
type RunHerdr interface {
	Agents(ctx context.Context) ([]herdr.Agent, error)
	SetPaneToken(ctx context.Context, id, source, name, value string) error
}

// The pane label of an automation run: a herdr metadata token of the triggering pane, whose
// value is the tool and the result of its newest run. [NEW] the source, the token name and the
// value form.
const (
	labelSource = "brain"
	labelToken  = "brain-automation"
)

// launcher is one `brain automation launch` process: the run of one claimed stop event
// (§10.4 steps 3 to 7). brained starts it with ids only, and with the herdr variables of the
// triggering pane.
type launcher struct {
	Env
	pid    int    // this process, for brain automation cancel
	paused func() // tests only: called after the checks, before the launch mark
}

// run runs claimed run l.Run once. It resolves the pane of the event again, skips the run when
// a check fails or an agent works in the pinned folder, writes the launch mark, runs the tool
// with the brain run checks as a Brain-origin call, and writes the result. The result is
// unknown from the claim on, so a crash at any point leaves unknown. A run that has a launch
// mark or an end already never runs again: Brain does not retry.
func (ln launcher) run(ctx context.Context, l brained.Launch) (err error) {
	st, err := store.Open(ctx, l.Store, busyTimeout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	var r store.AutomationRun
	var a store.Automation
	err = st.Write(ctx, func(tx *sql.Tx) (err error) {
		if r, err = store.AutomationRunByID(ctx, tx, l.Run); err != nil {
			return err
		}
		a, err = store.AutomationByID(ctx, tx, r.AutomationID)
		return err
	})
	if err != nil {
		return err
	}
	if !r.Started.IsZero() || !r.Finished.IsZero() {
		return fmt.Errorf("run %d is %s already; Brain does not run it again", r.ID, r.Result)
	}
	b, why := ln.check(ctx, l, r, a)
	if ln.paused != nil {
		ln.paused()
	}
	var name string
	if why == nil {
		a, name, why = ln.mark(ctx, st, l, r)
	}
	if why != nil {
		_, err := ln.finish(ctx, st, r.ID, store.RunSkipped)
		return errors.Join(fmt.Errorf("run %d of automation %d skipped: %w", r.ID, r.AutomationID, why), err)
	}
	labelErr := ln.label(ctx, b, r.PaneID, name, store.RunUnknown)
	_, runErr := RunTool(ctx, ln.Env, ToolRun{PaneID: r.PaneID, WorkspaceID: r.WorkspaceID, Origin: store.OriginAutomation,
		Tool: name, ToolID: a.ToolID, ContractHash: a.ContractHash, Stdout: io.Discard, Stderr: io.Discard})
	result := store.RunOK
	if runErr != nil {
		result = store.RunFailed
	}
	// A cancel ends ctx, and the run still writes its end.
	ctx = context.WithoutCancel(ctx)
	final, err := ln.finish(ctx, st, r.ID, result)
	if err == nil {
		labelErr = errors.Join(labelErr, ln.label(ctx, b, r.PaneID, name, final))
	}
	return errors.Join(runErr, err, labelErr)
}

// check builds the caller context of run r from its pane and resolves it again (§10.4 step 3):
// the pane must resolve to the binding and the store of l, with the pane and the workspace of
// the event, and its workspace key must be the key of automation a. It never uses the pane
// that brained started in: brained gives the pane of the event. Then no agent of the
// workspace may work in the pinned folder (step 4). It returns the binding, or why the run is
// skipped.
func (ln launcher) check(ctx context.Context, l brained.Launch, r store.AutomationRun, a store.Automation) (_ bindings.Binding, err error) {
	c, err := ln.open(ctx, false)
	if err != nil {
		return bindings.Binding{}, err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	switch {
	case c.Binding.Session != l.Binding || filepath.Clean(c.Binding.Store) != filepath.Clean(l.Store):
		return bindings.Binding{}, fmt.Errorf("pane %s resolves to the store %s of herdr session %s, not to the binding of the run",
			c.PaneID, c.Binding.Store, c.Binding.Session)
	case c.PaneID != r.PaneID || c.WorkspaceID != r.WorkspaceID:
		return bindings.Binding{}, fmt.Errorf("pane %s in workspace %s is not the pane of the event", c.PaneID, c.WorkspaceID)
	}
	key, _, err := c.WorkspaceKey()
	if err != nil {
		return bindings.Binding{}, err
	}
	if key != a.WorkspaceKey {
		return bindings.Binding{}, fmt.Errorf("pane %s is in workspace %s, not in the workspace of automation %d", c.PaneID, key, a.ID)
	}
	return c.Binding, ln.busy(ctx, c.Binding, r, a)
}

// busy fails when an agent in the workspace of run r works in the pinned folder of a (§10.4
// step 4).
func (ln launcher) busy(ctx context.Context, b bindings.Binding, r store.AutomationRun, a store.Automation) error {
	h, err := ln.RunHerdr(target(b))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	defer cancel()
	as, err := h.Agents(ctx)
	if err != nil {
		return err
	}
	home := lookupEnv(ln.Environ, "HOME")
	for _, ag := range as {
		if ag.WorkspaceID == r.WorkspaceID && ag.Status == herdr.StatusWorking && a.In(ag.Cwd, home) {
			return fmt.Errorf("the agent in pane %s works in %s", ag.PaneID, a.Folder)
		}
	}
	return nil
}

// mark writes the launch mark of run r in one short transaction, after it checks the binding,
// its enabled state and the approval again (§10.4 step 5). A pause, a removal or an undo that
// completed before it stops the run. It returns the automation and the tool name.
func (ln launcher) mark(ctx context.Context, st *store.Store, l brained.Launch, r store.AutomationRun) (a store.Automation, name string, err error) {
	err = st.Write(ctx, func(tx *sql.Tx) error {
		if a, err = output.AutomationMayRun(ctx, tx, filepath.Join(l.Store, outDir), r.AutomationID); err != nil {
			return err
		}
		t, err := store.ToolByID(ctx, tx, a.ToolID)
		if err != nil {
			return err
		}
		name = t.Name
		return store.StartRun(ctx, tx, r.ID, ln.pid, ln.Now().UTC())
	})
	return a, name, err
}

// finish writes result to run id when it has not ended, and returns the result that the row
// holds then: a cancel that came first keeps cancelled.
func (ln launcher) finish(ctx context.Context, st *store.Store, id int64, result store.RunResult) (final store.RunResult, err error) {
	err = st.Write(ctx, func(tx *sql.Tx) error {
		if _, err := store.FinishRun(ctx, tx, id, result, ln.Now().UTC()); err != nil {
			return err
		}
		r, err := store.AutomationRunByID(ctx, tx, id)
		final = r.Result
		return err
	})
	return final, err
}

// label shows result of a run of tool name on the pane label of pane id; ok clears it. The
// label holds the row's result, so a run shows unknown until it ends, and a crash leaves
// unknown (§10.4 "Failure feedback").
func (ln launcher) label(ctx context.Context, b bindings.Binding, id, name string, result store.RunResult) error {
	h, err := ln.RunHerdr(target(b))
	if err != nil {
		return err
	}
	value := name + " " + string(result)
	if result == store.RunOK {
		value = ""
	}
	ctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	defer cancel()
	return h.SetPaneToken(ctx, id, labelSource, labelToken, value)
}

// cancelRun records cancelled for active run id of an automation of the caller's workspace,
// then stops its process (§10.4 "Cancel"). The record comes first, so the end that the run
// writes after the stop keeps cancelled. Effects that happened stay.
func (cl caller) cancelRun(ctx context.Context, id int64) error {
	var r store.AutomationRun
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		run, err := store.AutomationRunByID(ctx, tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %d: %w", id, errNotFound)
		}
		if err != nil {
			return err
		}
		a, err := store.AutomationByID(ctx, tx, run.AutomationID)
		if err != nil {
			return err
		}
		if a.WorkspaceKey != cl.key {
			return fmt.Errorf("run %d: %w", id, errNotFound)
		}
		r, err = store.CancelRun(ctx, tx, id, cl.Now().UTC())
		return err
	})
	if err != nil {
		return err
	}
	// A process that is gone ended with a crash: the run is cancelled, and nothing runs.
	if err := cl.Kill(r.PID); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stop process %d of run %d: %w", r.PID, id, err)
	}
	_, err = fmt.Fprintf(cl.Stdout, "run %d of automation %d cancelled; effects that already happened stay\n", id, r.AutomationID)
	return err
}
