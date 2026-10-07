package brained

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// shortCommit is the length of a commit hash in a revert excerpt [NEW], as git's short hashes.
const shortCommit = 7

// Reverts is the revert rule of one brained (#283, §7.1): before each extraction pass it
// stores each revert that git shows in a rooted workspace as a git observation, so the
// extractor sees it as it sees the other signals. It keeps the work-tree changes of the last
// pass in memory only, as capture keeps the last read of a pane (§6.2 step 4). So the first
// pass after a start is a baseline, and a revert commit counts only when it is newer than the
// start (§6.2 step 8): Brain does not invent what it missed.
type Reverts struct {
	since   time.Time
	changes map[string]signal.Changes // by workspace key
}

// NewReverts returns the revert rule of a brained that starts at since.
func NewReverts(since time.Time) *Reverts {
	return &Reverts{since: since, changes: map[string]signal.Changes{}}
}

// gitWorkspace is one rooted workspace with its segments, newest first.
type gitWorkspace struct {
	ws   store.Workspace
	segs []store.Segment
}

// Pass stores the reverts of each live workspace of binding herdrSession that has a root
// folder and a capture segment. A revert commit goes into the newest segment that started
// before the commit; a file that goes back goes into the newest segment. Its capture identity
// holds the commit, or the time of the pass, so a revert counts once (§6.4). A workspace with
// learning off for the pane of that segment gets nothing (§6.7). defaultOn is the learning
// state with no store setting (§12.1). git runs outside the transactions.
func (r *Reverts) Pass(ctx context.Context, st *store.Store, herdrSession string, defaultOn bool, now time.Time) error {
	var wss []gitWorkspace
	err := st.Write(ctx, func(tx *sql.Tx) error {
		rooted, err := store.RootedWorkspaces(ctx, tx, herdrSession)
		if err != nil {
			return err
		}
		wss = nil
		for _, w := range rooted {
			segs, err := store.WorkspaceSegments(ctx, tx, herdrSession, w.Key)
			if err != nil {
				return err
			}
			if len(segs) > 0 {
				wss = append(wss, gitWorkspace{ws: w, segs: segs})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	var obs []gitObs
	changes := map[string]signal.Changes{}
	for _, w := range wss {
		root := *w.ws.RootFolder
		rs, err := signal.Reverts(ctx, root)
		if err != nil {
			return fmt.Errorf("reverts of workspace %s: %w", w.ws.Key, err)
		}
		for _, c := range rs {
			if seg, ok := segmentAt(w.segs, c.At); ok && !c.At.Before(r.since) {
				obs = append(obs, gitObservation(fmt.Sprintf("git/%s/revert/%s", w.ws.Key, c.Commit), seg, commitExcerpt(c), now))
			}
		}
		cur, back, err := signal.WorkChanges(ctx, root, r.changes[w.ws.Key])
		if err != nil {
			return fmt.Errorf("work tree of workspace %s: %w", w.ws.Key, err)
		}
		changes[w.ws.Key] = cur
		if len(back) > 0 {
			obs = append(obs, gitObservation(fmt.Sprintf("git/%s/back/%d", w.ws.Key, now.UnixNano()), w.segs[0],
				"git: the work-tree change of "+strings.Join(back, ", ")+" went back to the committed content, with no commit", now))
		}
	}
	err = st.Write(ctx, func(tx *sql.Tx) error {
		for _, o := range obs {
			on, err := store.LearningOn(ctx, tx, defaultOn, herdrSession, o.workspace, o.pane)
			if err != nil {
				return err
			}
			if !on {
				continue
			}
			if _, err := store.AddObservation(ctx, tx, o.Observation); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.changes = changes
	return nil
}

// gitObs is a git observation with the workspace and the pane of its segment.
type gitObs struct {
	store.Observation
	workspace, pane string
}

// segmentAt returns the newest segment of segs, newest first, that started at or before at.
func segmentAt(segs []store.Segment, at time.Time) (store.Segment, bool) {
	for _, s := range segs {
		if !s.Started.After(at) {
			return s, true
		}
	}
	return store.Segment{}, false
}

// gitObservation is a git observation of a revert in segment seg.
func gitObservation(identity string, seg store.Segment, excerpt string, now time.Time) gitObs {
	text, truncated := filter(strings.Join(redact("", []string{excerpt}), "\n"))
	return gitObs{workspace: seg.WorkspaceKey, pane: seg.PaneID, Observation: store.Observation{
		CaptureIdentity: identity, Source: store.SourceGit, SegmentID: seg.ID, Event: signal.RevertEvent,
		Excerpt: text, Truncated: truncated, Status: store.ObservationComplete, Sender: store.SenderUnconfirmed,
		Counts: true, At: now}}
}

// commitExcerpt is the excerpt of revert commit c: its subject, the commit that it undoes and
// its files.
func commitExcerpt(c signal.RevertCommit) string {
	s := fmt.Sprintf("git revert: commit %s %q", c.Commit[:min(len(c.Commit), shortCommit)], c.Subject)
	for _, u := range c.Reverted {
		s += " undoes commit " + u[:min(len(u), shortCommit)]
	}
	if len(c.Paths) > 0 {
		s += "; files: " + strings.Join(c.Paths, ", ")
	}
	return s
}
