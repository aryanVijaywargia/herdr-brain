package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/BurntSushi/toml"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// notificationTitle is the title of the notification of a suggestion.
const notificationTitle = "Brain has 1 suggestion"

// errSkip: a check before the suggestion failed. The suggestion shows nothing (E14-S3).
var errSkip = errors.New("suggestion skipped")

// Suggest runs `brain suggest` with args after "suggest" (E14-S3): the detached process
// of one reserved suggestion. The hook or brained starts it with ids only. It resolves its pane
// again, checks each id and the state of the proposal, shows a herdr notification and opens
// the review pane beside the agent without focus. Any doubt skips, with no UI: the suggestion
// is then expired. It never types into the agent.
func Suggest(ctx context.Context, e Env, args []string) error {
	d, err := brained.ParseDispatch(args)
	if err != nil {
		return err
	}
	return suggester{Env: e}.run(ctx, d)
}

// suggester is one run of `brain suggest`.
type suggester struct {
	Env
	paused func() // tests only: called after the resolve, before the first check
}

// run shows suggestion d, or skips it.
func (sg suggester) run(ctx context.Context, d brained.Dispatch) (err error) {
	c, err := sg.open(ctx, openNoRepair)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	cl := newCaller(sg.Env, c)
	if sg.paused != nil {
		sg.paused()
	}
	// The lock spans the checks, the open and its record: two suggestions for one agent never
	// both see no pane, and a later in a review pane waits for its suggestion to be recorded.
	unlock, err := lockSuggestions(c.Binding.Store)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	// The checks run before the notification, and again before the open: either can wait.
	h, err := sg.check(ctx, cl, d)
	if err == nil {
		err = sg.notify(ctx, h, d, c.PaneID)
	}
	if err == nil {
		_, err = sg.check(ctx, cl, d)
	}
	if err != nil {
		return sg.expire(ctx, cl, d, err)
	}
	_, err = openReviewPane(ctx, sg.Env, c, d.Proposal, func(tx *sql.Tx, pane string) error {
		return store.ShowSuggestion(ctx, tx, d.Suggestion, pane, sg.Now())
	})
	if err != nil {
		return sg.expire(ctx, cl, d, err)
	}
	return nil
}

// lockSuggestions takes the suggestion lock of the store in folder dir and returns its unlock.
// The lock file is <store>/suggest.lock; the herdr deadlines bound how long a holder keeps it.
func lockSuggestions(dir string) (func() error, error) {
	f, err := os.OpenFile(filepath.Join(dir, "suggest.lock"), os.O_CREATE|os.O_RDWR, 0o600) // 0o600, as the store
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, errors.Join(fmt.Errorf("lock suggestions: %w", err), f.Close())
	}
	return f.Close, nil // the close drops the lock
}

// notify shows the herdr notification of suggestion d beside pane paneID. It names ids only.
func (sg suggester) notify(ctx context.Context, h PaneHerdr, d brained.Dispatch, paneID string) error {
	ctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	defer cancel()
	return h.ShowNotification(ctx, notificationTitle, fmt.Sprintf("proposal %d opens beside pane %s", d.Proposal, paneID))
}

// expire marks suggestion d expired after err, when its row is still the one of d: a skip is
// no error of the process, and nothing shows it.
func (sg suggester) expire(ctx context.Context, cl caller, d brained.Dispatch, err error) error {
	if !errors.Is(err, errSkip) {
		return err
	}
	return cl.Store.Write(ctx, func(tx *sql.Tx) error {
		s, err := store.SuggestionByID(ctx, tx, d.Suggestion)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil // a forget deleted it
		case err != nil:
			return err
		case !sameDispatch(s, d, cl.PaneID):
			return nil
		}
		return store.ExpireSuggestion(ctx, tx, d.Suggestion)
	})
}

// sameDispatch reports whether row s is the suggestion of d for the agent in pane paneID.
func sameDispatch(s store.Suggestion, d brained.Dispatch, paneID string) bool {
	return s.ObservationID == d.Occurrence && s.ProposalID == d.Proposal && s.SessionID == d.Session &&
		s.Generation == d.Generation && s.HerdrSession == d.Binding && s.WorkspaceKey == d.Workspace &&
		s.AgentPaneID == paneID
}

// skip is errSkip with why.
func skip(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errSkip, fmt.Sprintf(format, args...))
}

// check runs the checks of suggestion d for the resolved pane of cl, and returns the herdr of
// its binding. Each failed check is errSkip:
//   - live_suggestions is on in the config file;
//   - the binding, the store and the workspace key are the ones of d;
//   - herdr shows the pane in its workspace, and an agent in it, the one of the capture session of d, and the session
//     still holds the pane with the capture generation of d (a new agent, a move, a forget);
//   - the row of d is still there, unshown and undecided, with the ids of d;
//   - learning and extraction are on, the proposal is still pending in this scope (a reject, an
//     approve, a change, a forget), and "later" did not mute it in the session;
//   - the agent has no live review pane: one pane for each agent at a time.
func (sg suggester) check(ctx context.Context, cl caller, d brained.Dispatch) (PaneHerdr, error) {
	live, err := liveSuggestions(sg.Config)
	if err != nil || !live {
		return nil, skip("live suggestions are off (%v)", err)
	}
	c := cl.Call
	// The binding and the workspace key are read again: a rebind or a remap can come at any
	// time (E2-S3).
	bs, _, err := bindings.Read(cl.Bindings)
	if err != nil {
		return nil, skip("read bindings: %v", err)
	}
	b, ok, err := bindings.BySocket(bs, c.Binding.Socket)
	if err != nil || !ok || b != c.Binding || b.Session != d.Binding || b.Store != d.Store {
		return nil, skip("the pane resolves to another binding or store")
	}
	if key, _, err := c.WorkspaceKey(); key != d.Workspace {
		return nil, skip("the workspace has key %q, want %q (%v)", key, d.Workspace, err)
	}
	hctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	p, err := sg.Pane(hctx, target(c.Binding), c.PaneID)
	cancel()
	switch {
	case err != nil:
		return nil, skip("pane %s: %v", c.PaneID, err)
	case p.ID != c.PaneID || p.WorkspaceID != c.WorkspaceID:
		return nil, skip("pane %s moved to %s in workspace %s", c.PaneID, p.ID, p.WorkspaceID)
	}
	var beside []store.ReviewPane
	err = c.Store.Write(ctx, func(tx *sql.Tx) (err error) {
		if err := sg.checkStore(ctx, tx, cl, d, p); err != nil {
			return err
		}
		beside, err = store.ReviewPanesBeside(ctx, tx, c.Binding.Session, c.PaneID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := sg.checkPanes(ctx, cl, beside); err != nil {
		return nil, err
	}
	return sg.Herdr(target(c.Binding))
}

// checkStore runs the store checks of check in tx, for pane p as herdr shows it now.
func (sg suggester) checkStore(ctx context.Context, tx *sql.Tx, cl caller, d brained.Dispatch, p herdr.Pane) error {
	s, err := store.SuggestionByID(ctx, tx, d.Suggestion)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return skip("suggestion %d is gone", d.Suggestion)
	case err != nil:
		return err
	case !sameDispatch(s, d, cl.PaneID) || s.Result != "" || !s.ShownAt.IsZero():
		return skip("suggestion %d is not the one of the call, or it is done", d.Suggestion)
	}
	err = store.CheckGeneration(ctx, tx, d.Session, d.Generation)
	switch {
	case errors.Is(err, store.ErrForgotten):
		return skip("%v", err)
	case err != nil:
		return err
	}
	se, err := store.SessionByID(ctx, tx, d.Session)
	if err != nil {
		return err
	}
	in, err := store.SessionInPane(ctx, tx, d.Session, cl.PaneID)
	if err != nil {
		return err
	}
	if !in || !sameAgent(se, p) {
		return skip("pane %s holds another agent or agent session now", cl.PaneID)
	}
	return checkProposal(ctx, tx, cl, d)
}

// checkProposal checks in tx that learning and extraction are on for the pane of cl, and that
// the proposal of d is still pending in its scope and not muted in the session of d.
func checkProposal(ctx context.Context, tx *sql.Tx, cl caller, d brained.Dispatch) error {
	learning, err := brained.NewGate(cl.Binding).Learning(ctx, tx, cl.PaneID, cl.WorkspaceID)
	if err != nil {
		return err
	}
	extraction, err := store.ExtractionOn(ctx, tx, d.Workspace)
	if err != nil {
		return err
	}
	if !learning.On || !extraction {
		return skip("learning or extraction is off")
	}
	_, _, _, err = cl.pending(ctx, tx, d.Proposal)
	switch {
	case errors.Is(err, errNotFound) || errors.Is(err, errDecided):
		return skip("%v", err)
	case err != nil:
		return err
	}
	muted, err := store.SuggestionMuted(ctx, tx, d.Session, d.Proposal)
	if err != nil {
		return err
	}
	if muted {
		return skip("proposal %d is muted in session %d", d.Proposal, d.Session)
	}
	return nil
}

// sameAgent reports whether herdr pane p shows the agent of capture session s: its harness and
// its agent session id. An id that only one side has is a doubt. When herdr gives no id and
// the session has none, the harness and the open segment of the session in the pane are all
// there is to check, as for a hook record (store.HookSegment).
func sameAgent(s store.Session, p herdr.Pane) bool {
	if p.Agent == nil || *p.Agent != s.Key.Harness {
		return false
	}
	native := ""
	if p.AgentSession != nil {
		native = p.AgentSession.Value
	}
	return native == s.AgentSessionID
}

// checkPanes fails with errSkip when a review pane of rows still runs beside the agent. A row
// whose pane is no longer Brain's review pane is marked closed: it was closed without a
// decision, for example with q.
func (sg suggester) checkPanes(ctx context.Context, cl caller, rows []store.ReviewPane) error {
	if len(rows) == 0 {
		return nil
	}
	bs, _, err := bindings.Read(cl.Bindings)
	if err != nil {
		return err
	}
	var gone []int64
	for _, p := range rows {
		_, live, err := cl.reviewPaneLive(ctx, bs, p)
		switch {
		case err != nil:
			return skip("review pane %s: %v", p.PaneID, err)
		case live:
			return skip("review pane %s still runs beside pane %s", p.PaneID, cl.PaneID)
		}
		gone = append(gone, p.ID)
	}
	return cl.Store.Write(ctx, func(tx *sql.Tx) error {
		for _, id := range gone {
			if err := store.CloseReviewPane(ctx, tx, id, sg.Now()); err != nil {
				return err
			}
		}
		return nil
	})
}

// liveSuggestions reads the live_suggestions switch of the config file at path: "on" or "off".
// A missing file, or a file with no switch, is on: the default.
func liveSuggestions(path string) (bool, error) {
	var cfg struct {
		LiveSuggestions *string `toml:"live_suggestions"`
	}
	_, err := toml.DecodeFile(path, &cfg)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("read config: %w", err)
	case cfg.LiveSuggestions == nil:
		return true, nil
	}
	switch v := *cfg.LiveSuggestions; v {
	case "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("config %s: live_suggestions = %q, want \"on\" or \"off\"", path, v)
	}
}
