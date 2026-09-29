package hook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/brained"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/resolver"
	"github.com/aryanVijaywargia/herdr-brain/internal/secret"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// argvLimit is the longest stored command in bytes (§6.7, §11: ≤ 4 KB).
const argvLimit = 4096

// The senders of the hook records (§6.1, §6.5). A submitted prompt keeps the sender
// unconfirmed: a script or another agent can submit text too.
var senders = map[string]store.Sender{
	adapter.EventPrompt: store.SenderUnconfirmed,
	adapter.EventTool:   store.SenderTool,
	adapter.EventStop:   store.SenderAssistant,
}

// record stores one hook record of h.Event from the hook input raw (E11-S5, §6.1): one
// observation and, for a tool, one command record. It uses the same resolver and live check
// as the start hook, and it prints nothing. It stores nothing when the profile does not turn
// the event on, when learning is off, or when capture has no open segment of the pane: a hook
// record joins the session that capture keeps.
func (h Hook) record(ctx context.Context, raw []byte) (err error) {
	if !h.Profile.On(h.Event) {
		return nil
	}
	in, err := h.Profile.ParseRecord(h.Event, raw)
	if errors.Is(err, adapter.ErrRetest) {
		return errors.Join(err, harness.MarkRetest(h.Retest, h.Profile.Kind, h.Now()))
	}
	if err != nil {
		return err
	}
	open := func(ctx context.Context, dir string) (resolver.Store, error) {
		s, err := store.Open(ctx, dir, busyTimeout)
		if err != nil {
			return nil, err
		}
		return records{s}, nil
	}
	// No agent session goes to the resolver: a hook record writes no delivery record.
	res, err := resolver.New(h.Bindings, open, h.pane).Resolve(ctx, h.Environ, "")
	if err != nil {
		return err
	}
	r, ok := res.(*resolver.Resolved)
	if !ok {
		// No store, or a pane that failed the live check: a hook record writes nothing.
		return nil
	}
	defer func() { err = errors.Join(err, r.Store.Close()) }()
	rs, ok := r.Store.(records)
	if !ok {
		panic(fmt.Sprintf("hook: resolver store is %T, want the hook's own", r.Store))
	}
	return rs.s.Write(ctx, func(tx *sql.Tx) error {
		return h.write(ctx, tx, r, in, h.Now().UTC())
	})
}

// write stores the hook record in in tx, after the learning check of the pane.
func (h Hook) write(ctx context.Context, tx *sql.Tx, r *resolver.Resolved, in adapter.Record, now time.Time) error {
	learning, err := brained.NewGate(r.Binding).Learning(ctx, tx, r.PaneID, r.WorkspaceID)
	if err != nil || !learning.On {
		return err
	}
	kind := h.Profile.Kind
	session, segment, err := store.HookSegment(ctx, tx, r.Binding.Session, kind, r.PaneID, in.SessionID)
	if err != nil || segment == 0 {
		return err
	}
	o := store.Observation{
		// A hook has no event id: the identity is the binding, the segment and the time (§6.4).
		CaptureIdentity: fmt.Sprintf("%s/%d/hook/%s/%s", r.Binding.Session, segment, h.Event, now.Format(time.RFC3339Nano)),
		Source:          store.SourceHook, SegmentID: segment, Event: h.Event,
		Status: store.ObservationComplete, Sender: senders[h.Event], Counts: true, At: now,
	}
	var cmd store.CommandRecord
	if h.Event == adapter.EventTool {
		cmd = command(in)
		if !cmd.SecretFound {
			o.Excerpt, o.Truncated, o.BrainOrigin = brained.HookText(kind, in.Command)
		}
	} else {
		o.Excerpt, o.Truncated, o.BrainOrigin = brained.HookText(kind, in.Text)
	}
	if o.ID, err = store.AddObservation(ctx, tx, o); err != nil || o.ID == 0 {
		return err
	}
	if h.Event == adapter.EventTool {
		cmd.ObservationID = o.ID
		if err := store.AddCommandRecord(ctx, tx, cmd); err != nil {
			return err
		}
	}
	if err := brained.Match(ctx, tx, kind, session, o); err != nil {
		return err
	}
	return store.MoveCursor(ctx, tx, session, o.Source, o.CaptureIdentity)
}

// command is the command record of tool input in (§6.7). A command in which the secret filter
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
type records struct{ s *store.Store }

func (records) WriteWithheld(context.Context, resolver.Withheld) error { return nil }

func (records) RecordAgentSession(context.Context, string, string, string) error { return nil }

func (records) UncheckedAgentSessions(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (records) SetAgentCheck(context.Context, string, string, string, resolver.AgentCheck) error {
	return nil
}

func (rs records) Close() error { return rs.s.Close() }
