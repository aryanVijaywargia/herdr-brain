package brained

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// shortCommit is the length of a commit hash in a revert excerpt [NEW], as git's short hashes.
const shortCommit = 7

// Reverts is the revert rule of one brained (#283): before each extraction pass it
// stores each revert that git shows in a rooted workspace as a git observation, so the
// extractor sees it as it sees the other signals. It keeps the work-tree changes of the last
// pass in memory only, as capture keeps the last read of a pane. So the first
// pass after a start is a baseline, and a revert commit counts only when it is newer than the
// start: Brain does not invent what it missed.
type Reverts struct {
	since   time.Time
	changes map[string]signal.Changes           // by workspace key
	owners  map[string]map[string]store.Segment // by workspace key and path: the segment when the change was first seen
	done    map[string]bool                     // the capture identities of the revert commits that a pass stored or skipped
}

// NewReverts returns the revert rule of a brained that starts at since.
func NewReverts(since time.Time) *Reverts {
	return &Reverts{since: since.Truncate(time.Second), changes: map[string]signal.Changes{},
		owners: map[string]map[string]store.Segment{}, done: map[string]bool{}}
}

// gitWorkspace is one rooted workspace with its segments, newest first, and what git shows in
// it at this pass.
type gitWorkspace struct {
	ws      store.Workspace
	segs    []store.Segment
	obs     []gitObs
	commits []string // the capture identities of the new revert commits
	changes signal.Changes
	owners  map[string]store.Segment
}

// Pass stores the reverts of each live workspace of binding herdrSession that has a root
// folder and a capture segment. A revert commit goes into the newest segment that started
// before the commit; a file that goes back goes into the segment that was the newest when
// its change was first seen. Its capture identity holds the commit, or the time of the pass,
// so a revert counts once. A revert that a new revert commit of the pass shows is not a
// second one. With learning off for the pane of that segment, the revert is dropped for good,
// and the work-tree changes of the workspace start again from a baseline. defaultOn is
// the learning state with no store setting. git runs outside the transactions. A git
// failure in one workspace does not stop the others.
func (r *Reverts) Pass(ctx context.Context, st *store.Store, herdrSession string, defaultOn bool, now time.Time) error {
	var wss []*gitWorkspace
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
				wss = append(wss, &gitWorkspace{ws: w, segs: segs})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	var errs []error
	ok := wss[:0]
	for _, w := range wss {
		if err := r.find(ctx, w, now); err != nil {
			errs = append(errs, fmt.Errorf("workspace %s: %w", w.ws.Key, err))
			continue
		}
		ok = append(ok, w)
	}
	err = st.Write(ctx, func(tx *sql.Tx) error {
		for _, w := range ok {
			on, err := store.LearningOn(ctx, tx, defaultOn, herdrSession, w.ws.Key, w.segs[0].PaneID)
			if err != nil {
				return err
			}
			if !on {
				w.changes, w.owners = nil, nil
			}
			for _, o := range w.obs {
				if on, err := store.LearningOn(ctx, tx, defaultOn, herdrSession, o.workspace, o.pane); err != nil || !on {
					if err != nil {
						return err
					}
					continue
				}
				if _, err := store.AddObservation(ctx, tx, o.Observation); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, w := range ok {
		r.changes[w.ws.Key], r.owners[w.ws.Key] = w.changes, w.owners
		for _, id := range w.commits {
			r.done[id] = true
		}
	}
	return errors.Join(errs...)
}

// find reads git in workspace w and fills in its observations and its work-tree changes.
func (r *Reverts) find(ctx context.Context, w *gitWorkspace, now time.Time) error {
	root := *w.ws.RootFolder
	rs, err := signal.Reverts(ctx, root)
	if err != nil {
		return err
	}
	undone := map[string]bool{} // the paths of the new revert commits
	for _, c := range rs {
		id := fmt.Sprintf("git/%s/revert/%s", w.ws.Key, c.Commit)
		seg, found := segmentAt(w.segs, c.At)
		if r.done[id] || !found || c.At.Before(r.since) {
			continue
		}
		w.commits = append(w.commits, id)
		w.obs = append(w.obs, gitObservation(id, seg, commitExcerpt(c), now))
		for _, p := range c.Paths {
			undone[p] = true
		}
	}
	before := r.owners[w.ws.Key]
	cur, back, err := signal.WorkChanges(ctx, root, r.changes[w.ws.Key])
	if err != nil {
		return err
	}
	w.changes, w.owners = cur, map[string]store.Segment{}
	for p := range cur {
		seg, seen := before[p]
		if !seen {
			seg = w.segs[0]
		}
		w.owners[p] = seg
	}
	// One observation for each segment, with its paths in path order.
	var segs []store.Segment
	paths := map[int64][]string{}
	for _, p := range back {
		if undone[p] {
			continue
		}
		seg, seen := before[p]
		if !seen {
			seg = w.segs[0]
		}
		if paths[seg.ID] == nil {
			segs = append(segs, seg)
		}
		paths[seg.ID] = append(paths[seg.ID], p)
	}
	for _, seg := range segs {
		w.obs = append(w.obs, gitObservation(fmt.Sprintf("git/%s/back/%d/%d", w.ws.Key, now.UnixNano(), seg.ID), seg,
			"git: the work-tree change of "+strings.Join(paths[seg.ID], ", ")+
				" went back to the committed content, with no commit", now))
	}
	return nil
}

// gitObs is a git observation with the workspace and the pane of its segment.
type gitObs struct {
	store.Observation
	workspace, pane string
}

// segmentAt returns the newest segment of segs, newest first, that started at or before at, a
// git commit time. git keeps whole seconds, so the start counts in whole seconds too.
func segmentAt(segs []store.Segment, at time.Time) (store.Segment, bool) {
	for _, s := range segs {
		if !s.Started.Truncate(time.Second).After(at) {
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
