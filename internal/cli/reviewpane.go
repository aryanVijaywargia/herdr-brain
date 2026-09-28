package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

// PaneHerdr is the herdr calls that open, check and close review panes in one herdr session
// (E5-S2). *herdr.Client has them.
type PaneHerdr interface {
	OpenPluginPane(ctx context.Context, s herdr.PluginPaneSpec) (herdr.PluginPane, error)
	PaneProcesses(ctx context.Context, id string) ([]herdr.Process, error)
	ClosePluginPane(ctx context.Context, id string) error
}

const (
	pluginID         = "brain"  // the id in herdr-plugin.toml
	reviewEntrypoint = "review" // the [[panes]] id of the review pane
	// proposalEnv carries the proposal id to the review pane. [NEW] the name (E5-S1).
	proposalEnv = "BRAIN_PROPOSAL_ID"
)

// target is the herdr session of call c.
func target(c *Call) herdr.Target {
	return herdr.Target{Session: c.Binding.Session, Socket: c.Binding.Socket}
}

// openReviewPane opens the review pane of proposal id beside the calling pane, without focus,
// and records it in review_panes (§5.1, S52). It returns the new pane id.
func openReviewPane(ctx context.Context, e Env, c *Call, id int64) (string, error) {
	h, err := e.Herdr(target(c))
	if err != nil {
		return "", err
	}
	hctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	pp, err := h.OpenPluginPane(hctx, herdr.PluginPaneSpec{Plugin: pluginID, Entrypoint: reviewEntrypoint,
		Placement: "split", TargetPane: c.PaneID, Env: []string{proposalEnv + "=" + strconv.FormatInt(id, 10)}})
	cancel()
	if err != nil {
		return "", err
	}
	err = c.Store.Write(ctx, func(tx *sql.Tx) error {
		_, err := store.InsertReviewPane(ctx, tx, store.ReviewPane{HerdrSession: c.Binding.Session,
			SessionIdentity: continuity(c.Workspace, pp.Pane.ID), PaneID: pp.Pane.ID, BesidePaneID: c.PaneID,
			ProposalID: id, Opened: e.Now()})
		return err
	})
	if err != nil {
		// The pane runs, and closes itself after a decision; Brain only cannot close it.
		return "", fmt.Errorf("review pane %s opened, but Brain has no record of it: %w", pp.Pane.ID, err)
	}
	return pp.Pane.ID, nil
}

// continuity is the continuity identity of the workspace of paneID: its Brain key, or nil
// when it has none. The key changes when the (herdr session, workspace id) pair no longer
// holds its project (§11 "Workspace identity"), so a row of an old workspace stops matching.
func continuity(s workspace.Session, paneID string) *string {
	ws, _, _ := strings.Cut(paneID, ":")
	m, err := s.Lookup(ws)
	if err != nil || m.State != workspace.Mapped {
		return nil
	}
	return &m.Entry.Key
}

// closeReviewPanes closes the review panes of proposal id after a decision (§5.1). It closes
// only a pane whose herdr session and pane id are in review_panes, whose workspace keeps its
// continuity identity, and that herdr shows still runs the review pane of id (§11
// review_panes, S82). A row that fails a check is invalid and is marked closed with no herdr
// call. The review pane that made the decision exits by itself: its row is only marked. The
// decision is done, so a failed herdr call is a warning.
func (cl caller) closeReviewPanes(ctx context.Context, id int64) error {
	var rows []store.ReviewPane
	err := cl.Store.Write(ctx, func(tx *sql.Tx) (err error) {
		rows, err = store.OpenReviewPanes(ctx, tx, cl.Binding.Session, id)
		return err
	})
	if err != nil || len(rows) == 0 {
		return err
	}
	h, err := cl.Herdr(target(cl.Call))
	if err != nil {
		return cl.warn(err)
	}
	var closed []int64
	var warns []error
	for _, p := range rows {
		done, err := cl.closeReviewPane(ctx, h, p)
		if done {
			closed = append(closed, p.ID)
		}
		if err != nil {
			warns = append(warns, fmt.Errorf("review pane %s: %w", p.PaneID, err))
		}
	}
	if len(closed) > 0 {
		err = cl.Store.Write(ctx, func(tx *sql.Tx) error {
			for _, rid := range closed {
				if err := store.CloseReviewPane(ctx, tx, rid, cl.Now()); err != nil {
					return err
				}
			}
			return nil
		})
		warns = append(warns, err)
	}
	if err := errors.Join(warns...); err != nil {
		return cl.warn(err)
	}
	return nil
}

// closeReviewPane closes the pane of row p when the checks pass. done is true when the row is
// no longer open: the pane closed, is gone, or the row is invalid.
func (cl caller) closeReviewPane(ctx context.Context, h PaneHerdr, p store.ReviewPane) (done bool, err error) {
	switch {
	case p.PaneID == cl.PaneID && cl.surface == store.SurfacePane:
		return true, nil // the calling review pane exits by itself
	case !samePtr(p.SessionIdentity, continuity(cl.Workspace, p.PaneID)):
		return true, nil // continuity lost: the pane id may name another pane now
	}
	hctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	defer cancel()
	ps, err := h.PaneProcesses(hctx, p.PaneID)
	switch {
	case errors.Is(err, herdr.ErrPaneNotFound):
		return true, nil
	case err != nil:
		return false, err
	case !runsReview(ps, p.ProposalID):
		return true, nil // the pane id names another pane now
	}
	if err := h.ClosePluginPane(hctx, p.PaneID); err != nil {
		return false, err
	}
	return true, nil
}

// runsReview reports whether herdr shows the review pane of proposal id among ps: the
// command of the review [[panes]] entry after its exec.
func runsReview(ps []herdr.Process, id int64) bool {
	want := []string{"pane", "review", "--proposal", strconv.FormatInt(id, 10)}
	return slices.ContainsFunc(ps, func(p herdr.Process) bool {
		return p.Name == "brain" && len(p.Argv) == len(want)+1 && slices.Equal(p.Argv[1:], want)
	})
}

func samePtr(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// warn prints err as a warning. It returns only the error of the print.
func (cl caller) warn(err error) error {
	_, werr := fmt.Fprintf(cl.Stderr, "brain: warning: close the review pane: %s\n", strings.ReplaceAll(err.Error(), "\n", "; "))
	return werr
}
