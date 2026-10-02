package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
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
	ShowNotification(ctx context.Context, title, body string) error
}

const (
	pluginID         = "brain"  // the id in herdr-plugin.toml
	reviewEntrypoint = "review" // the [[panes]] id of the review pane
	// proposalEnv carries the proposal id to the review pane. [NEW] the name (E5-S1).
	proposalEnv = "BRAIN_PROPOSAL_ID"
)

// target is the herdr session of binding b.
func target(b bindings.Binding) herdr.Target {
	return herdr.Target{Session: b.Session, Socket: b.Socket}
}

// openReviewPane opens the review pane of proposal id beside the calling pane, without focus,
// and records it in review_panes (§5.1, S52), with also in the same transaction. It returns
// the new pane id.
func openReviewPane(ctx context.Context, e Env, c *Call, id int64, also func(tx *sql.Tx, pane string) error) (string, error) {
	// The split is in the workspace of the calling pane.
	identity, err := continuity(c.Workspace, c.WorkspaceID)
	if err != nil {
		return "", err
	}
	h, err := e.Herdr(target(c.Binding))
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
			SessionIdentity: identity, PaneID: pp.Pane.ID, BesidePaneID: c.PaneID, ProposalID: id, Opened: e.Now()})
		if err != nil {
			return err
		}
		return also(tx, pp.Pane.ID)
	})
	if err != nil {
		// The pane runs, and closes itself after a decision; Brain only cannot close it.
		return "", fmt.Errorf("review pane %s opened, but Brain has no record of it: %w", pp.Pane.ID, err)
	}
	return pp.Pane.ID, nil
}

// errNoContinuity: the workspace no longer holds the project of its entry.
var errNoContinuity = errors.New("the workspace has no continuity identity")

// continuity is the continuity identity of workspace ws of s: its Brain key. The key changes
// when the (herdr session, workspace id) pair no longer holds its project (§11 "Workspace
// identity"), so a row of an old workspace stops matching. An unmapped workspace has no
// identity yet (nil): herdr saves a new workspace after about 5 s. A withheld workspace gives
// errNoContinuity.
func continuity(s workspace.Session, ws string) (*string, error) {
	m, err := s.Lookup(ws)
	switch {
	case err != nil:
		return nil, err
	case m.State == workspace.Mapped:
		return &m.Entry.Key, nil
	case m.State == workspace.Unmapped:
		return nil, nil
	default:
		return nil, fmt.Errorf("%w: %w", errNoContinuity, m.Err)
	}
}

// closeReviewPanes closes the review panes of proposal id after a decision (§5.1). A pane can
// be in any herdr session that is bound to this store: the call targets the binding of the
// row. It closes only a pane whose binding and pane id are in review_panes, whose workspace
// keeps its continuity identity, and that herdr shows still runs the review pane of id (§11
// review_panes, S82). A row that fails a check is invalid and is marked closed with no herdr
// call. The review pane that made the decision exits by itself: its row is only marked. The
// decision is done, so a failed herdr call is a warning.
func (cl caller) closeReviewPanes(ctx context.Context, id int64) error {
	var rows []store.ReviewPane
	err := cl.Store.Write(ctx, func(tx *sql.Tx) (err error) {
		rows, err = store.OpenReviewPanes(ctx, tx, id)
		return err
	})
	if err != nil || len(rows) == 0 {
		return err
	}
	bs, _, err := bindings.Read(cl.Bindings)
	if err != nil {
		return cl.warn(err)
	}
	var closed []int64
	var warns []error
	for _, p := range rows {
		done, err := cl.closeReviewPane(ctx, bs, p)
		if done {
			closed = append(closed, p.ID)
		}
		if err != nil {
			warns = append(warns, fmt.Errorf("review pane %s of herdr session %s: %w", p.PaneID, p.HerdrSession, err))
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

// closeReviewPane closes the pane of row p when the checks pass. bs is the bindings file. done
// is true when the row is no longer open: the pane closed, is gone, or the row is invalid.
func (cl caller) closeReviewPane(ctx context.Context, bs []bindings.Binding, p store.ReviewPane) (done bool, err error) {
	if p.HerdrSession == cl.Binding.Session && p.PaneID == cl.PaneID && cl.surface == store.SurfacePane {
		return true, nil // the calling review pane exits by itself
	}
	h, live, err := cl.reviewPaneLive(ctx, bs, p)
	if err != nil || !live {
		return err == nil, err
	}
	hctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	defer cancel()
	if err := h.ClosePluginPane(hctx, p.PaneID); err != nil {
		return false, err
	}
	return true, nil
}

// reviewPaneLive reports whether the pane of row p is still Brain's review pane: its binding is
// in bs, its workspace keeps its continuity identity, and herdr shows that it still runs the
// review pane of its proposal (§11 review_panes, S82). live is false when the row is invalid
// or the pane is gone. h is the herdr of the pane's binding when live is true.
func (cl caller) reviewPaneLive(ctx context.Context, bs []bindings.Binding, p store.ReviewPane) (h PaneHerdr, live bool, err error) {
	i := slices.IndexFunc(bs, func(b bindings.Binding) bool {
		return b.Session == p.HerdrSession && b.Store == cl.Binding.Store
	})
	if i < 0 {
		return nil, false, nil // the binding is gone
	}
	ws, _, _ := strings.Cut(p.PaneID, ":")
	identity, err := continuity(workspace.For(bs[i]), ws)
	switch {
	case errors.Is(err, errNoContinuity):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	case !samePtr(p.SessionIdentity, identity):
		return nil, false, nil // continuity lost: the pane id may name another pane now
	}
	if h, err = cl.Herdr(target(bs[i])); err != nil {
		return nil, false, err
	}
	hctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	defer cancel()
	ps, err := h.PaneProcesses(hctx, p.PaneID)
	switch {
	case errors.Is(err, herdr.ErrPaneNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	// Another process means that the pane id names another pane now.
	return h, runsReview(ps, p.ProposalID), nil
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
