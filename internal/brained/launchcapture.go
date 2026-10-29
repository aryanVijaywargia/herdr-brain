package brained

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/transcript"
)

const (
	// captureWindow is how long after its end brained still tries the capture of a launch; an
	// older launch stays as it is. A day.
	captureWindow = 24 * time.Hour
	// captureTimeout bounds one capture pass of ended launches.
	captureTimeout = time.Minute
)

// captureLaunches finishes the capture sources of the ended launches of the herdr server of
// gate g that ended at or after since and that brained has not finished yet (E24-S5). Each
// session that herdr named in the pane of a launch joins the launch record with source screen,
// unless it has a higher one. A Codex session that no Brain hook captured gets the records of
// the exact rollout of its thread under the Codex home of the launch, never the newest file
// in a folder, and its source is transcript then. A session with no rollout keeps the
// pane-screen capture. A launch that the pass finished is noted, so an error leaves it for the
// next pass.
func captureLaunches(ctx context.Context, g Gate, since, now time.Time) (err error) {
	st, err := store.Open(ctx, g.Binding.Store, busyTimeout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	var ls []store.Launch
	err = st.Write(ctx, func(tx *sql.Tx) (err error) {
		ls, err = store.LaunchesToCapture(ctx, tx, g.Binding.Socket, since)
		return err
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, l := range ls {
		if err := captureLaunch(ctx, st, g, l, now); err != nil {
			errs = append(errs, fmt.Errorf("launch %d: %w", l.ID, err))
		}
	}
	return errors.Join(errs...)
}

// captureLaunch is captureLaunches of launch l. It leaves the current session of l as it is:
// only a hook of the launch knows which session is current.
func captureLaunch(ctx context.Context, st *store.Store, g Gate, l store.Launch, now time.Time) error {
	var caps []store.LaunchCapture
	var ss []store.LaunchSession
	err := st.Write(ctx, func(tx *sql.Tx) (err error) {
		if caps, err = store.LaunchCaptures(ctx, tx, g.Binding.Session, l); err != nil {
			return err
		}
		for _, c := range caps {
			if err := store.AddLaunchSession(ctx, tx, l.ID, c.AgentSessionID, store.CaptureScreen, c.First); err != nil {
				return err
			}
		}
		ss, err = store.LaunchSessions(ctx, tx, l.ID)
		return err
	})
	if err != nil {
		return err
	}
	for _, s := range ss {
		i := slices.IndexFunc(caps, func(c store.LaunchCapture) bool { return c.AgentSessionID == s.ID })
		// A rollout joins only a capture session of the pane. A forget of that session deleted
		// text that the rollout still holds, so it stays out.
		if l.CLI != transcript.Codex || l.CodexHome == "" || s.Source != store.CaptureScreen || i < 0 || caps[i].Generation != 0 {
			continue
		}
		if err := readRollout(ctx, st, g, l, caps[i], now); err != nil {
			return err
		}
	}
	return st.Write(ctx, func(tx *sql.Tx) error { return store.SetLaunchCaptured(ctx, tx, l.ID, now) })
}

// readRollout imports the turns of the rollout of the thread of capture session c of launch l
// that capture was allowed to see, and then sets the source of the thread to transcript.
func readRollout(ctx context.Context, st *store.Store, g Gate, l store.Launch, c store.LaunchCapture, now time.Time) error {
	path, ok, err := transcript.CodexRollout(ctx, l.CodexHome, c.AgentSessionID)
	if err != nil || !ok {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	ses, err := transcript.ReadCodex(ctx, path, info.Size())
	if errors.Is(err, transcript.ErrUnknownFormat) {
		return nil // the reader fails closed: the session keeps the pane-screen capture
	}
	if err != nil {
		return err
	}
	if ses.ID != c.AgentSessionID {
		return nil // the name is no proof: the rollout of another thread stays out
	}
	err = st.Write(ctx, func(tx *sql.Tx) error {
		// A forget of the session during the read: the read may hold forgotten text.
		if err := store.CheckGeneration(ctx, tx, c.SessionID, c.Generation); err != nil {
			return err
		}
		learning, err := g.Learning(ctx, tx, l.PaneID, l.WorkspaceID)
		if err != nil || !learning.On {
			return err
		}
		segs, err := store.PaneSegments(ctx, tx, c.SessionID, l.PaneID, l.Started)
		if err != nil {
			return err
		}
		imp := importer{tx: tx, harness: transcript.Codex, cwd: ses.CWD, at: cmp.Or(ses.Last, now), match: c.SessionID}
		stored := false
		for _, t := range ses.Turns {
			if imp.segment = turnSegment(segs, learning.Changed, now, t); imp.segment == 0 {
				continue
			}
			id, err := imp.turn(ctx, t)
			if err != nil {
				return err
			}
			stored = stored || id != 0
		}
		if !stored {
			return nil
		}
		return store.AddLaunchSession(ctx, tx, l.ID, c.AgentSessionID, store.CaptureTranscript, now)
	})
	if errors.Is(err, store.ErrForgotten) {
		return nil
	}
	return err
}

// turnSegment returns the segment of segs that holds each record of turn t, or 0 when none
// does. Capture keeps a segment only while learning is on, so a segment is a time in which
// capture was allowed: from its start to its end, or to now while it is open. A learning
// change at changed after the start of a segment cuts it there, because Brain cannot prove the
// learning state after it. A record with no time is in no segment.
func turnSegment(segs []store.Segment, changed, now time.Time, t transcript.Turn) int64 {
	times := []time.Time{t.Prompt.At}
	for _, c := range t.Commands {
		times = append(times, c.At)
		if c.Failure != nil {
			times = append(times, c.Failure.At)
		}
	}
	if t.Reply.ID != "" {
		times = append(times, t.Reply.At)
	}
	for _, s := range segs {
		end := cmp.Or(s.Ended, now)
		if changed.After(s.Started) && changed.Before(end) {
			end = changed
		}
		if !slices.ContainsFunc(times, func(at time.Time) bool { return at.Before(s.Started) || at.After(end) }) {
			return s.ID
		}
	}
	return 0
}
