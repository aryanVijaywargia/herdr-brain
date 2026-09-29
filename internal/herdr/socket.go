package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
)

// Event kinds that brained subscribes to. herdr 0.9.1 names most events on the stream with
// "_" (pane_created), but pane.agent_status_changed with "."; Next gives the "." form.
const (
	PaneCreated       = "pane.created"
	PaneAgentDetected = "pane.agent_detected"
	PaneStatusChanged = "pane.agent_status_changed"
	PaneExited        = "pane.exited"
	PaneClosed        = "pane.closed"
)

// Socket talks to the herdr API socket at Path directly: one JSON request line, then JSON
// reply lines (herdr 0.9.1 src/api/server.rs). brained needs it, because an events.subscribe
// stream stays open, and the CLI cannot give one.
type Socket struct {
	Path string
}

// Subscription is one entry of events.subscribe. PaneID is set only for
// pane.agent_status_changed: herdr needs the pane for status events.
type Subscription struct {
	Type   string `json:"type"`
	PaneID string `json:"pane_id,omitempty"`
}

// Event is one event of a subscription stream.
type Event struct {
	Kind        string // one of the Pane* kinds
	PaneID      string
	WorkspaceID string
	Agent       string // the agent of an agent event; "" when herdr gives none
	Status      string // the agent_status of pane.agent_status_changed
	Released    bool   // pane.agent_detected: the agent left the pane
}

// Stream is an open events.subscribe connection. herdr fixes its subscriptions when it
// starts, so a new pane needs a new stream.
type Stream struct {
	conn net.Conn
	r    *bufio.Reader
}

// Subscribe opens a stream with subs. It returns after herdr accepts them. A pane status
// subscription for a pane that is gone gives ErrPaneNotFound.
func (s Socket) Subscribe(ctx context.Context, subs []Subscription) (*Stream, error) {
	params := map[string]any{"subscriptions": subs}
	conn, r, err := s.request(ctx, "events.subscribe", params, "subscription_started", nil)
	if err != nil {
		return nil, fmt.Errorf("herdr events.subscribe: %w", err)
	}
	return &Stream{conn: conn, r: r}, nil
}

// Next blocks until the next event. It returns io.EOF when herdr closes the stream, and an
// error after Close.
func (st *Stream) Next() (Event, error) {
	line, err := st.r.ReadBytes('\n')
	if err != nil {
		return Event{}, err
	}
	return parseEvent(line)
}

// Close closes the stream. A Next that waits returns an error.
func (st *Stream) Close() error {
	return st.conn.Close()
}

// Panes returns every pane of the session, from pane.list.
func (s Socket) Panes(ctx context.Context) ([]Pane, error) {
	var r struct {
		Panes *[]*rawPane `json:"panes"`
	}
	if err := s.call(ctx, "pane.list", map[string]any{}, "pane_list", &r); err != nil {
		return nil, fmt.Errorf("herdr pane.list: %w", err)
	}
	ps, err := parsePanes(r.Panes)
	if err != nil {
		return nil, fmt.Errorf("herdr pane.list: %w", err)
	}
	return ps, nil
}

// Plugin is a plugin that herdr has linked.
type Plugin struct {
	ID      string
	Enabled bool
}

// Plugins returns the linked plugins, from plugin.list. An unlinked plugin is not in it.
func (s Socket) Plugins(ctx context.Context) ([]Plugin, error) {
	var r struct {
		Plugins *[]struct {
			PluginID *string `json:"plugin_id"`
			Enabled  *bool   `json:"enabled"`
		} `json:"plugins"`
	}
	if err := s.call(ctx, "plugin.list", map[string]any{}, "plugin_list", &r); err != nil {
		return nil, fmt.Errorf("herdr plugin.list: %w", err)
	}
	var ck check
	raw := req(&ck, r.Plugins, "plugins")
	ps := make([]Plugin, len(raw))
	for i, p := range raw {
		f := fmt.Sprintf("plugins[%d]", i)
		ps[i] = Plugin{ID: req(&ck, p.PluginID, f+".plugin_id"), Enabled: req(&ck, p.Enabled, f+".enabled")}
	}
	if ck.err != nil {
		return nil, fmt.Errorf("herdr plugin.list: %w", ck.err)
	}
	return ps, nil
}

// call sends one request and decodes its reply into v.
func (s Socket) call(ctx context.Context, method string, params any, want string, v any) error {
	conn, _, err := s.request(ctx, method, params, want, v)
	if err != nil {
		return err
	}
	return conn.Close()
}

// request connects, sends one request and reads its first reply, which must be
// {"result":{"type":want,...}}. It decodes the result into v when v is not nil. The caller
// owns the open connection and the reader that holds the rest of the replies. The request
// ends at the deadline or cancel of ctx.
func (s Socket) request(ctx context.Context, method string, params any, want string, v any) (net.Conn, *bufio.Reader, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", s.Path)
	if err != nil {
		return nil, nil, err
	}
	// Close ends a blocked write or read when ctx ends; the close error is not the one to report.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	r := bufio.NewReader(conn)
	err = exchange(conn, r, method, params, want, v)
	if !stop() {
		return nil, nil, errors.Join(ctx.Err(), err)
	}
	if err != nil {
		_ = conn.Close() // the request error is the one to report
		return nil, nil, err
	}
	return conn, r, nil
}

func exchange(conn net.Conn, r *bufio.Reader, method string, params any, want string, v any) error {
	line, err := json.Marshal(map[string]any{"id": "brain", "method": method, "params": params})
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return err
	}
	reply, err := r.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	var head struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(reply, &head); err != nil {
		return &ShapeError{Reason: err.Error()}
	}
	if head.Error != nil {
		return apiError(head.Error.Code, head.Error.Message)
	}
	if v == nil {
		v = &struct{}{}
	}
	return decodeResult(head.Result, want, v)
}

// parseEvent parses one stream line {"event":kind,"data":{...}}. pane_created holds the
// pane in data.pane; the other kinds hold data.pane_id and data.workspace_id.
func parseEvent(line []byte) (Event, error) {
	var raw struct {
		Event *string `json:"event"`
		Data  *struct {
			PaneID      *string  `json:"pane_id"`
			WorkspaceID *string  `json:"workspace_id"`
			Agent       *string  `json:"agent"`
			AgentStatus *string  `json:"agent_status"`
			Released    bool     `json:"released"`
			Pane        *rawPane `json:"pane"`
		} `json:"data"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return Event{}, &ShapeError{Reason: err.Error()}
	}
	var ck check
	kind := strings.Replace(req(&ck, raw.Event, "event"), "pane_", "pane.", 1)
	d := req(&ck, raw.Data, "data")
	if ck.err != nil {
		return Event{}, ck.err
	}
	ev := Event{Kind: kind, Released: d.Released}
	switch kind {
	case PaneCreated:
		p, err := d.Pane.parse("data.pane")
		if err != nil {
			return Event{}, err
		}
		ev.PaneID, ev.WorkspaceID = p.ID, p.WorkspaceID
		return ev, nil
	case PaneStatusChanged:
		ev.Status = req(&ck, d.AgentStatus, "data.agent_status")
	case PaneAgentDetected, PaneExited, PaneClosed:
	default:
		return Event{}, &ShapeError{Field: "event", Reason: fmt.Sprintf("unknown kind %q", kind)}
	}
	ev.PaneID = req(&ck, d.PaneID, "data.pane_id")
	ev.WorkspaceID = req(&ck, d.WorkspaceID, "data.workspace_id")
	if d.Agent != nil {
		ev.Agent = *d.Agent
	}
	ck.id(workspaceIDRe, ev.WorkspaceID, "", "data.workspace_id")
	ck.id(paneIDRe, ev.PaneID, ev.WorkspaceID, "data.pane_id")
	if ck.err != nil {
		return Event{}, ck.err
	}
	return ev, nil
}

// Pane returns pane id, from pane.get. A pane that is gone gives ErrPaneNotFound.
func (s Socket) Pane(ctx context.Context, id string) (Pane, error) {
	if err := checkPaneID(id); err != nil {
		return Pane{}, err
	}
	var r struct {
		Pane *rawPane `json:"pane"`
	}
	if err := s.call(ctx, "pane.get", map[string]any{"pane_id": id}, "pane_info", &r); err != nil {
		return Pane{}, fmt.Errorf("herdr pane.get: %w", err)
	}
	p, err := r.Pane.parse("pane")
	if err != nil {
		return Pane{}, fmt.Errorf("herdr pane.get: %w", err)
	}
	return p, nil
}

// The read sources of pane.read that Brain uses. Neither moves the pane's scroll.
const (
	ReadVisible         = "visible"          // the screen as the user sees it
	ReadRecentUnwrapped = "recent_unwrapped" // the last lines of the history, with wraps joined
)

// PaneRead is one pane.read reply.
type PaneRead struct {
	PaneID      string
	WorkspaceID string
	Source      string
	Text        string
	Revision    uint64
	Truncated   bool
}

// Read reads the text of pane id from source, as plain text. lines is the number of lines
// for a history source, 0 for the herdr default. It only reads: pane.read sends no input.
func (s Socket) Read(ctx context.Context, id, source string, lines int) (PaneRead, error) {
	if err := checkPaneID(id); err != nil {
		return PaneRead{}, err
	}
	params := map[string]any{"pane_id": id, "source": source, "format": "text", "strip_ansi": true}
	if lines > 0 {
		params["lines"] = lines
	}
	var r struct {
		Read *struct {
			PaneID      *string `json:"pane_id"`
			WorkspaceID *string `json:"workspace_id"`
			Source      *string `json:"source"`
			Text        *string `json:"text"`
			Revision    *uint64 `json:"revision"`
			Truncated   *bool   `json:"truncated"`
		} `json:"read"`
	}
	if err := s.call(ctx, "pane.read", params, "pane_read", &r); err != nil {
		return PaneRead{}, fmt.Errorf("herdr pane.read: %w", err)
	}
	var ck check
	raw := req(&ck, r.Read, "read")
	if ck.err != nil {
		return PaneRead{}, fmt.Errorf("herdr pane.read: %w", ck.err)
	}
	pr := PaneRead{
		PaneID:      req(&ck, raw.PaneID, "read.pane_id"),
		WorkspaceID: req(&ck, raw.WorkspaceID, "read.workspace_id"),
		Source:      req(&ck, raw.Source, "read.source"),
		Text:        req(&ck, raw.Text, "read.text"),
		Revision:    req(&ck, raw.Revision, "read.revision"),
		Truncated:   req(&ck, raw.Truncated, "read.truncated"),
	}
	ck.id(workspaceIDRe, pr.WorkspaceID, "", "read.workspace_id")
	ck.id(paneIDRe, pr.PaneID, pr.WorkspaceID, "read.pane_id")
	if ck.err != nil {
		return PaneRead{}, fmt.Errorf("herdr pane.read: %w", ck.err)
	}
	return pr, nil
}
