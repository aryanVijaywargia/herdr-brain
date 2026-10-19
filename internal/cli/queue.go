package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// errNotFound hides a proposal or an item of another workspace: the call learns nothing about
// it, not even its text (E4 scope check).
var errNotFound = errors.New("not found in this workspace")

// errDecided: a surface decided the proposal already, for example in another process.
var errDecided = errors.New("conflict")

// permissionWords finds text that may change a permission, an allow rule or a safety rule.
// [NEW] the list is a guess (E4-S3); proposals.permission_warning records how often
// it fires in M1.
var permissionWords = regexp.MustCompile(`(?i)allow|permission|sandbox|--dangerously|\b(never|always)\s+run`)

// caller is an open command call and what it may see: the global items and the items of the
// calling pane's workspace. All real herdr sessions share one main store, so every queue read
// and every id-based action goes through it (E4 scope check).
type caller struct {
	Env
	*Call
	key     string // the Brain key of the calling workspace; "" when it has none
	label   string
	keyErr  error         // why key is ""
	surface store.Surface // where a decision of this call is made
}

func newCaller(e Env, c *Call) caller {
	cl := caller{Env: e, Call: c, surface: store.SurfaceCLI}
	// A workspace with no key has no items of its own: it sees the global items only, as the
	// hook gives it the global part only (S48). keyErr says why, for --scope workspace.
	cl.key, cl.label, cl.keyErr = c.WorkspaceKey()
	return cl
}

// sees reports whether an item with workspace key key is in the caller's view.
func (cl caller) sees(key *string) bool { return key == nil || (cl.key != "" && *key == cl.key) }

// item reads item id and its current revision, when the caller sees it.
func (cl caller) item(ctx context.Context, tx *sql.Tx, id int64) (store.Item, store.ItemRevision, error) {
	it, err := store.ItemByID(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !cl.sees(it.WorkspaceKey) {
		return store.Item{}, store.ItemRevision{}, fmt.Errorf("item %d: %w", id, errNotFound)
	}
	if err != nil {
		return store.Item{}, store.ItemRevision{}, err
	}
	rev, err := store.ItemRevisionByKey(ctx, tx, id, it.CurrentRevision)
	return it, rev, err
}

// proposal reads proposal id, its item and the item's current revision, when the caller sees
// the item. A proposal has an item and a base revision, but a script proposal (E17-S8): it has
// none, and the caller sees it when it sees its candidate. Any other proposal without them is
// not in the queue.
func (cl caller) proposal(ctx context.Context, tx *sql.Tx, id int64) (store.Proposal, store.Item, store.ItemRevision, error) {
	notFound := fmt.Errorf("proposal %d: %w", id, errNotFound)
	p, err := store.ProposalByID(ctx, tx, id)
	switch {
	case err == nil && p.Script():
		return cl.script(ctx, tx, p, notFound)
	case errors.Is(err, sql.ErrNoRows) || err == nil && (p.ItemID == nil || p.BaseRevision == nil):
		return store.Proposal{}, store.Item{}, store.ItemRevision{}, notFound
	case err != nil:
		return store.Proposal{}, store.Item{}, store.ItemRevision{}, err
	}
	it, rev, err := cl.item(ctx, tx, *p.ItemID)
	if errors.Is(err, errNotFound) {
		return store.Proposal{}, store.Item{}, store.ItemRevision{}, notFound
	}
	return p, it, rev, err
}

// script returns script proposal p, with no item, when the caller sees its candidate, and
// notFound when it does not, or when a forget took the candidate.
func (cl caller) script(ctx context.Context, tx *sql.Tx, p store.Proposal, notFound error) (store.Proposal, store.Item, store.ItemRevision, error) {
	c, err := cl.candidate(ctx, tx, p)
	switch {
	case errors.Is(err, errNotFound) || err == nil && c == nil:
		return store.Proposal{}, store.Item{}, store.ItemRevision{}, notFound
	case err != nil:
		return store.Proposal{}, store.Item{}, store.ItemRevision{}, err
	}
	return p, store.Item{}, store.ItemRevision{}, nil
}

// pending reads proposal id as proposal does, and fails with a conflict when you decided it
// already, for example in another process.
func (cl caller) pending(ctx context.Context, tx *sql.Tx, id int64) (store.Proposal, store.Item, store.ItemRevision, error) {
	p, it, rev, err := cl.proposal(ctx, tx, id)
	if err == nil && p.Status != store.ProposalPending {
		err = fmt.Errorf("proposal %d: %w: it is %s already", id, errDecided, p.Status)
	}
	return p, it, rev, err
}

// target names the part of the brief that an item with scope goes into. A workspace item is
// always in the caller's workspace, since the caller sees it.
func (cl caller) target(scope store.Scope) string {
	if scope == store.ScopeGlobal {
		return "global"
	}
	return fmt.Sprintf("workspace %s (%s)", cl.WorkspaceID, cl.label)
}

// nextRevision is the revision that you make from cur in review: it keeps the fields of cur,
// and you are its author.
func nextRevision(cur store.ItemRevision, now time.Time) store.ItemRevision {
	cur.Author, cur.Origin, cur.ProposalID, cur.UndoesRevision, cur.Created = store.AuthorYou, store.OriginReview, nil, nil, now
	return cur
}

// deliver runs the post-commit build (E4): the hook never builds, so a commit that
// changes the delivered text is built at once, and the next hook prints it. A failed build
// keeps the commit; the repair in the next brain call builds it.
func (cl caller) deliver(ctx context.Context, proposalID *int64) error {
	_, err := output.Build(ctx, cl.Store, filepath.Join(cl.Binding.Store, outDir), proposalID, cl.Now)
	// A newer commit made this build stale; the build of that commit holds this change too.
	if err == nil || errors.Is(err, output.ErrStale) {
		return nil
	}
	_, werr := fmt.Fprintf(cl.Stdout, "committed, delivery pending: %v (the next brain call repairs it)\n", err)
	return werr
}

// oneLine is text on one line, for a list.
func oneLine(text string) string { return strings.Join(strings.Fields(text), " ") }
