package hook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/launch"
	"github.com/aryanVijaywargia/herdr-brain/internal/resolver"
	"github.com/aryanVijaywargia/herdr-brain/internal/secret"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// argvLimit is the longest stored command in bytes (≤ 4 KB).
const argvLimit = 4096

// The senders of the hook records. A submitted prompt keeps the sender unconfirmed: a
// script or another agent can submit text too.
var senders = map[string]store.Sender{
	adapter.EventPrompt: store.SenderUnconfirmed,
	adapter.EventTool:   store.SenderTool,
	adapter.EventStop:   store.SenderAssistant,
}

// record stores one hook record of h.Event from the hook input stdin (E11-S5): one
// observation and, for a tool, one command record. It uses the same resolver and live check
// as the start hook, and it prints nothing. It stores nothing when the profile does not turn
// the event on, when learning is off, or when capture has no open segment of the pane: a hook
// record joins the session that capture keeps, as the global hooks do: a record before capture
// opens the session of the pane, or of a new session after /clear, is not stored. A launched
// session's hook uses the pane of its launch record (resolve), stores nothing once the launch
// has ended, and adds its native session to the launch record with the capture source hooks
// when it stores a record.
func (h Hook) record(ctx context.Context, stdin io.Reader) (err error) {
	var r *resolver.Resolved
	var rs records
	var session, gen int64
	if h.Profile.On(h.Event) {
		r, rs, session, gen, err = h.note(ctx)
		if r != nil {
			defer func() { err = errors.Join(err, r.Store.Close()) }()
		}
		if err != nil {
			return err
		}
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("read hook input: %w", err)
	}
	if !h.Profile.On(h.Event) {
		return nil
	}
	in, err := h.Profile.ParseRecord(h.Event, raw)
	if errors.Is(err, adapter.ErrRetest) {
		return errors.Join(err, harness.MarkRetest(h.Retest, h.Profile.Kind, h.Profile.SHA256, h.Now()))
	}
	if err != nil || session == 0 {
		return err
	}
	var d brained.Dispatch
	err = rs.s.Write(ctx, func(tx *sql.Tx) (err error) {
		if rs.launch != 0 {
			if err := launchRunning(ctx, tx, rs.launch); err != nil {
				return err
			}
		}
		d, err = h.write(ctx, tx, r, rs.launch, in, session, gen, h.Now().UTC())
		return err
	})
	if err != nil || d.Suggestion == 0 {
		return ignoreForgotten(err)
	}
	// A prompt that matches a ready proposal: a detached process shows the suggestion, and
	// the hook does not wait for it. It inherits the pane's environment; a launched session's
	// gets the herdr variables of the pane of its launch record, as its own may be missing.
	d.Store = r.Binding.Store
	environ := h.Environ
	if rs.launch != 0 {
		environ = brained.PaneEnv(environ, r.Binding, r.PaneID, r.WorkspaceID)
	}
	return h.Dispatch(ctx, environ, d)
}

// ignoreForgotten returns err, or nil when a forget of the session made the record stale.
func ignoreForgotten(err error) error {
	if errors.Is(err, store.ErrForgotten) {
		return nil
	}
	return err
}

// note resolves the pane of the hook and notes the capture session of the pane and its
// generation, before the record reads its text: the write refuses a newer generation, as a
// brain forget --session between the two deleted the evidence. r is nil when no store is open
// or the pane failed the live check, and session is 0 when capture has no open segment of the
// pane; a hook record writes nothing then. rs is the store of r, and the caller closes it.
func (h Hook) note(ctx context.Context) (r *resolver.Resolved, rs records, session, gen int64, err error) {
	if r, err = h.resolve(ctx); err != nil || r == nil {
		return nil, rs, 0, 0, err
	}
	rs, ok := r.Store.(records)
	if !ok {
		panic(fmt.Sprintf("hook: resolver store is %T, want the hook's own", r.Store))
	}
	err = rs.s.Write(ctx, func(tx *sql.Tx) (err error) {
		if session, _, err = store.HookSegment(ctx, tx, r.Binding.Session, h.Profile.Kind, r.PaneID, ""); err != nil || session == 0 {
			return err
		}
		gen, err = store.Generation(ctx, tx, session)
		return err
	})
	if err == nil && h.noted != nil {
		h.noted()
	}
	return r, rs, session, gen, err
}

// resolve finds the pane of a hook record and opens its store. For a launched session it is
// the pane of the launch record, after launch.Check; else the pane of the environment, after
// the resolver's live check. r is nil when no store is open or the pane failed the live check.
func (h Hook) resolve(ctx context.Context) (*resolver.Resolved, error) {
	if h.Launch == "" {
		open := func(ctx context.Context, dir string) (resolver.Store, error) {
			s, err := store.Open(ctx, dir, busyTimeout)
			if err != nil {
				return nil, err
			}
			return records{s: s}, nil
		}
		// No agent session goes to the resolver: a hook record writes no delivery record.
		res, err := resolver.New(h.Bindings, open, h.pane).Resolve(ctx, h.Environ, "")
		r, _ := res.(*resolver.Resolved)
		return r, err
	}
	l, err := h.checkLaunch(ctx)
	if err != nil {
		return nil, err
	}
	s, err := store.Open(ctx, l.Binding.Store, busyTimeout)
	if err != nil {
		return nil, err
	}
	return &resolver.Resolved{Binding: l.Binding, PaneID: l.PaneID, WorkspaceID: l.WorkspaceID,
		Store: records{s: s, launch: l.ID}}, nil
}

// launchRunning returns a refusal when launch record id has ended since its check: the launch
// can end while the hook reads its input.
func launchRunning(ctx context.Context, tx *sql.Tx, id int64) error {
	l, ok, err := store.LaunchByID(ctx, tx, id)
	switch {
	case err != nil:
		return err
	case !ok:
		return &launch.RefusedError{Reason: launch.Unknown}
	case !l.Ended.IsZero():
		return &launch.RefusedError{Reason: launch.Ended}
	}
	return nil
}

// write stores the hook record in in tx, after the learning check of the pane, in session
// noted with capture generation gen. A stored record of launch record launchID (0 for none)
// adds its native session to the launch with the source hooks. It returns the suggestion that
// a prompt reserved, if any.
func (h Hook) write(ctx context.Context, tx *sql.Tx, r *resolver.Resolved, launchID int64, in adapter.Record, noted, gen int64,
	now time.Time) (brained.Dispatch, error) {
	learning, err := brained.NewGate(r.Binding).Learning(ctx, tx, r.PaneID, r.WorkspaceID)
	if err != nil || !learning.On {
		return brained.Dispatch{}, err
	}
	kind := h.Profile.Kind
	session, segment, err := store.HookSegment(ctx, tx, r.Binding.Session, kind, r.PaneID, in.SessionID)
	if err != nil || segment == 0 || session != noted {
		// Another agent session, or another session holds the pane now: the note does not cover it.
		return brained.Dispatch{}, err
	}
	if err := store.CheckGeneration(ctx, tx, session, gen); err != nil {
		return brained.Dispatch{}, err
	}
	o := store.Observation{
		// A hook has no event id: the identity is the binding, the segment and the time.
		CaptureIdentity: fmt.Sprintf("%s/%d/hook/%s/%s", r.Binding.Session, segment, h.Event, now.Format(time.RFC3339Nano)),
		Source:          store.SourceHook, SegmentID: segment, Event: h.Event,
		Status: store.ObservationComplete, Sender: senders[h.Event], Counts: true, At: now,
	}
	var cmd store.CommandRecord
	var prompt string
	if h.Event == adapter.EventTool {
		cmd = command(in)
		if !cmd.SecretFound {
			o.Excerpt, o.Truncated, o.BrainOrigin = brained.HookText(kind, in.Command)
		}
	} else {
		o.Excerpt, o.Truncated, o.BrainOrigin = brained.HookText(kind, in.Text)
	}
	if h.Event == adapter.EventPrompt {
		prompt = brained.HookPrompt(kind, in.Text)
	}
	if o.ID, err = store.AddObservation(ctx, tx, o); err != nil || o.ID == 0 {
		return brained.Dispatch{}, err
	}
	if launchID != 0 {
		if err := store.SetLaunchSession(ctx, tx, launchID, in.SessionID, store.CaptureHooks, now); err != nil {
			return brained.Dispatch{}, err
		}
	}
	// The prompt index row joins the transaction of its observation (E14-S1).
	if err := brained.IndexPrompt(ctx, tx, o, prompt); err != nil {
		return brained.Dispatch{}, err
	}
	if h.Event == adapter.EventTool {
		cmd.ObservationID = o.ID
		if err := store.AddCommandRecord(ctx, tx, cmd); err != nil {
			return brained.Dispatch{}, err
		}
	}
	if err := brained.Match(ctx, tx, kind, session, o); err != nil {
		return brained.Dispatch{}, err
	}
	// The match is a local store read, inside the hook cap. After Match, so a pane read of
	// the same prompt that reserved its suggestion first blocks this one.
	d, err := brained.Reserve(ctx, tx, o, prompt, now)
	if err != nil {
		return brained.Dispatch{}, err
	}
	return d, store.MoveCursor(ctx, tx, session, o.Source, o.CaptureIdentity)
}

// command is the command record of tool input in. A command in which the secret filter
// finds a secret is not stored at all: the record says only that it had one.
func command(in adapter.Record) store.CommandRecord {
	c := store.CommandRecord{CWD: in.CWD, ExitStatus: in.ExitStatus}
	if len(secret.Find(in.Command)) > 0 {
		c.SecretFound = true
		return c
	}
	c.Argv = in.Command
	if len(c.Argv) > argvLimit {
		c.Argv, c.Truncated = cut(c.Argv, argvLimit), true
	}
	return c
}

// cut returns the longest prefix of s of at most n bytes that ends at a character boundary.
func cut(s string, n int) string {
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.ToValidUTF8(s[:n], "")
}

// records is the store of one hook record run as the resolver sees it. A hook record writes
// no receipt and no agent record, so every resolver write does nothing.
type records struct {
	s      *store.Store
	launch int64 // the launch record of a launched session; 0 for none
}

func (records) WriteWithheld(context.Context, resolver.Withheld) error { return nil }

func (records) RecordAgentSession(context.Context, string, string, string) error { return nil }

func (records) UncheckedAgentSessions(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (records) SetAgentCheck(context.Context, string, string, string, resolver.AgentCheck) error {
	return nil
}

func (rs records) Close() error { return rs.s.Close() }
