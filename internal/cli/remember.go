package cli

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/secret"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const rememberUsage = `usage: brain remember "<text>" [--global | --workspace <w>] [--kind fact|preference|decision|handoff]`

// remember is the parsed arguments of `brain remember`.
type remember struct {
	text      string
	global    bool
	workspace string // the --workspace value; it must name the calling pane's workspace
	kind      store.Kind
}

// Remember runs `brain remember` with args after "remember" (E4-S2). It drafts one item
// and one pending proposal, and prints the proposal id. The draft goes live only when you
// approve it in review; an agent's call takes the same path and never approves (S52).
func Remember(ctx context.Context, e Env, args []string) (err error) {
	// The filter runs on the raw arguments before the parser and the store: a hit writes
	// nothing, and no parse error can print a secret in a flag value (E4-S1).
	if err := secret.Check(strings.Join(args, " ")); err != nil {
		return err
	}
	r, err := parseRemember(args)
	if err != nil {
		return err
	}
	c, err := e.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	d, err := r.draft(c)
	if err != nil {
		return err
	}
	item, err := sameItem(ctx, c.Store, d)
	if err != nil {
		return err
	}
	if item != 0 {
		// The text is live already: a draft of it is only a duplicate to reject (#378).
		_, err = fmt.Fprintf(e.Stdout, "item %d already says this in %s; no draft was made\n", item, d.target)
		return err
	}
	id, err := saveDraft(ctx, c.Store, d, e.Now())
	if err != nil {
		return err
	}
	// The review pane opens beside the caller, an agent too (S52). When it cannot open, the
	// draft waits in `brain review`, and one line says why (S53).
	pane, err := openReviewPane(ctx, e, c, id, func(*sql.Tx, string) error { return nil })
	if err != nil {
		_, err = fmt.Fprintf(e.Stdout, "proposal %d: %s draft, waiting for your review (brain review); the review pane did not open: %s\n",
			id, d.target, strings.ReplaceAll(err.Error(), "\n", "; "))
		return err
	}
	_, err = fmt.Fprintf(e.Stdout, "proposal %d: %s draft, waiting for your review in pane %s (or brain review)\n", id, d.target, pane)
	return err
}

func parseRemember(args []string) (remember, error) {
	var r remember
	fs := flag.NewFlagSet("remember", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&r.global, "global", false, "")
	fs.StringVar(&r.workspace, "workspace", "", "")
	kind := fs.String("kind", string(store.KindFact), "") // [NEW] the default kind
	// The text can come before the flags too, as in `brain remember "use pnpm" --global`.
	var texts []string
	for {
		if err := fs.Parse(args); err != nil {
			return remember{}, fmt.Errorf("%w\n%s", err, rememberUsage)
		}
		if fs.NArg() == 0 {
			break
		}
		texts = append(texts, fs.Arg(0))
		args = fs.Args()[1:]
	}
	r.kind = store.Kind(*kind)
	switch {
	case len(texts) != 1:
		return remember{}, fmt.Errorf("want one text, got %d\n%s", len(texts), rememberUsage)
	case strings.TrimSpace(texts[0]) == "":
		return remember{}, errors.New("the text is empty")
	case r.global && r.workspace != "":
		return remember{}, errors.New("--global and --workspace exclude each other")
	// The kinds that remember can draft.
	case !slices.Contains([]store.Kind{store.KindFact, store.KindPreference, store.KindDecision, store.KindHandoff}, r.kind):
		return remember{}, fmt.Errorf("unknown kind %q\n%s", *kind, rememberUsage)
	case r.global && r.kind == store.KindHandoff:
		// A task handoff belongs to one workspace.
		return remember{}, errors.New("a handoff is a workspace item; drop --global")
	}
	r.text = texts[0]
	return r, nil
}

// draft is the item that remember writes.
type draft struct {
	text         string
	kind         store.Kind
	scope        store.Scope
	workspaceKey *string
	target       string // the part of the brief that the item goes into, once approved
}

// draft picks the scope of r for call c: global with --global, else the workspace of the
// calling pane. --workspace only confirms that workspace; it never selects another.
func (r remember) draft(c *Call) (draft, error) {
	d := draft{text: r.text, kind: r.kind, scope: store.ScopeGlobal, target: "global"}
	if r.global {
		return d, nil
	}
	key, label, err := c.WorkspaceKey()
	if err != nil {
		return draft{}, err
	}
	if r.workspace != "" && r.workspace != c.WorkspaceID && r.workspace != label {
		return draft{}, fmt.Errorf("--workspace %s is not the workspace of this pane (%s %q); run brain remember in a pane of %s, or use --global; nothing was written",
			r.workspace, c.WorkspaceID, label, r.workspace)
	}
	d.scope, d.workspaceKey = store.ScopeWorkspace, &key
	d.target = fmt.Sprintf("workspace %s (%s)", c.WorkspaceID, label)
	return d, nil
}

// sameItem returns the id of an active item whose current text is the text of d, in the scope
// of d, or 0 when there is none (#378). Space at the ends does not count.
func sameItem(ctx context.Context, s *store.Store, d draft) (int64, error) {
	var item int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		rs, err := store.ActiveRevisions(ctx, tx)
		if err != nil {
			return err
		}
		if i := slices.IndexFunc(rs, func(r store.ItemRevision) bool {
			return r.State == store.StateActive && r.Scope == d.scope &&
				(d.workspaceKey == nil || r.WorkspaceKey != nil && *r.WorkspaceKey == *d.workspaceKey) &&
				strings.TrimSpace(r.Text) == strings.TrimSpace(d.text)
		}); i >= 0 {
			item = rs[i].ItemID
		}
		return nil
	})
	return item, err
}

// saveDraft writes d as a draft item at revision 1 with a new claim id, and one pending
// proposal to approve it, in one transaction. It returns the proposal id. A draft is in no
// output, so it records no output generation and needs no build.
func saveDraft(ctx context.Context, s *store.Store, d draft, now time.Time) (int64, error) {
	claim, err := newClaimID()
	if err != nil {
		return 0, err
	}
	var pid int64
	err = s.Write(ctx, func(tx *sql.Tx) error {
		item, err := store.InsertItem(ctx, tx, store.Item{ClaimID: claim, CurrentRevision: 1, Kind: d.kind,
			Scope: d.scope, WorkspaceKey: d.workspaceKey, State: store.StateDraft, Created: now})
		if err != nil {
			return err
		}
		base := int64(1)
		// [NEW] the target names the brief part, and the diff of a new item is its text with "+ ".
		pid, err = store.InsertProposal(ctx, tx, store.Proposal{ItemID: &item, BaseRevision: &base,
			Text: d.text, Scope: d.scope, Target: d.target, Diff: "+ " + d.text,
			PermissionWarning: permissionWords.MatchString(d.text), Status: store.ProposalPending, Created: now})
		if err != nil {
			return err
		}
		// The author stays brain until you approve.
		return store.InsertItemRevision(ctx, tx, store.ItemRevision{ItemID: item, Revision: 1, Text: d.text,
			Scope: d.scope, WorkspaceKey: d.workspaceKey, State: store.StateDraft, Author: store.AuthorBrain,
			Origin: store.OriginRemember, ProposalID: &pid, Created: now})
	})
	return pid, err
}

// newClaimID returns a new opaque claim id with no text in it. [NEW] 8 random bytes, in
// hex with a "cl_" prefix, as the workspace keys.
func newClaimID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "cl_" + hex.EncodeToString(b), nil
}
