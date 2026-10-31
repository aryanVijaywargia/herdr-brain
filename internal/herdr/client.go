// Package herdr runs the herdr CLI for Brain and parses its JSON replies. It fails closed:
// a reply with an unknown shape is an error, never a zero value.
package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ErrPaneNotFound means that herdr has no pane with the asked id: the pane closed.
var ErrPaneNotFound = errors.New("pane not found")

// waitDelay is how long run waits for herdr's output pipes after herdr exits or the
// deadline kills it. A child of herdr that keeps a pipe open must not hold a Brain call
// past its deadline. 50 ms: well inside the 300 ms hook cap.
const waitDelay = 50 * time.Millisecond

// Target is the one herdr session that every call of a Client goes to. The resolver gives
// it. The client never uses the UI-focused session and never lets a child call pick its own.
// The socket decides: the client runs herdr with HERDR_SOCKET_PATH and no --session flag,
// because with --session herdr derives the socket from the name and the config folder under
// HOME, and Socket would have no effect (TestLiveTarget). So HOME does not change the target.
type Target struct {
	Session string // session name; "default" for the default session; goes to HERDR_SESSION
	Socket  string // API socket path of that session, from the binding
}

// Client runs herdr commands against one Target.
type Client struct {
	bin    string
	target Target
	env    []string
}

// New returns a client for target t. environ is the parent environment (os.Environ()). The
// binary is $HERDR_BIN_PATH, else herdr on the PATH of environ.
func New(environ []string, t Target) (*Client, error) {
	if t.Session == "" {
		return nil, errors.New("herdr target: no session name")
	}
	if !filepath.IsAbs(t.Socket) {
		return nil, fmt.Errorf("herdr target: socket path %q is not absolute", t.Socket)
	}
	bin, err := binary(environ)
	if err != nil {
		return nil, err
	}
	return &Client{bin: bin, target: t, env: childEnv(environ, t)}, nil
}

// binary returns $HERDR_BIN_PATH, else the first herdr on the PATH of environ. It skips a
// relative PATH entry, as exec.LookPath does, so the working folder cannot supply herdr.
func binary(environ []string) (string, error) {
	if bin := lookup(environ, "HERDR_BIN_PATH"); bin != "" {
		return bin, nil
	}
	for _, dir := range filepath.SplitList(lookup(environ, "PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, "herdr")
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", errors.New("herdr not found: HERDR_BIN_PATH is not set and herdr is not on PATH")
}

func lookup(environ []string, key string) string {
	var v string
	for _, kv := range environ {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val // the last entry wins, as in os/exec
		}
	}
	return v
}

// childEnv sets HERDR_SESSION and HERDR_SOCKET_PATH to the target. herdr 0.9.1 picks the
// session from --session first, then HERDR_SOCKET_PATH, then HERDR_SESSION, so inherited
// values of another session must never reach the child. HERDR_SESSION is set too, so that
// the child's two variables never disagree.
func childEnv(environ []string, t Target) []string {
	env := slices.DeleteFunc(slices.Clone(environ), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return k == "HERDR_SESSION" || k == "HERDR_SOCKET_PATH"
	})
	return append(env, "HERDR_SESSION="+t.Session, "HERDR_SOCKET_PATH="+t.Socket)
}

func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	return run(ctx, c.bin, c.env, args...)
}

// run runs `herdr args...` with env and returns its stdout. The caller gives the deadline in
// ctx: herdr is on the path of every Brain call, so no call may wait forever. herdr 0.9.1
// takes the last --session in the whole command line, and that flag wins over
// HERDR_SOCKET_PATH, so run refuses any argument that would change the target.
func run(ctx context.Context, bin string, env []string, args ...string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("context has no deadline")
	}
	for _, a := range args {
		if a == "--session" || strings.HasPrefix(a, "--session=") {
			return nil, fmt.Errorf("argument %q would change the herdr target", a)
		}
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	cmd.WaitDelay = waitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failure(err, stderr.Bytes())
	}
	return stdout.Bytes(), nil
}

// failure turns a failed herdr run into an error. herdr prints
// {"error":{"code":...,"message":...}} on stderr when the server refuses a request.
func failure(err error, stderr []byte) error {
	var reply struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(stderr, &reply) == nil && reply.Error.Code != "" {
		return apiError(reply.Error.Code, reply.Error.Message)
	}
	if msg := bytes.TrimSpace(stderr); len(msg) > 0 {
		return fmt.Errorf("%s: %w", msg, err)
	}
	return err
}

// apiError is the error of a request that the herdr server refused with code and message.
func apiError(code, message string) error {
	if code == "pane_not_found" {
		return fmt.Errorf("%s: %w", message, ErrPaneNotFound)
	}
	return fmt.Errorf("%s (%s)", message, code)
}

// result runs herdr, checks that the reply is {"result":{"type":want,...}} and decodes the
// result into v.
func (c *Client) result(ctx context.Context, want string, v any, args ...string) error {
	out, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	var reply struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(out, &reply); err != nil {
		return &ShapeError{Reason: err.Error()}
	}
	return decodeResult(reply.Result, want, v)
}

// decodeResult checks that result is {"type":want,...} and decodes it into v.
func decodeResult(result json.RawMessage, want string, v any) error {
	var head struct {
		Type *string `json:"type"`
	}
	if err := json.Unmarshal(result, &head); err != nil || head.Type == nil {
		return &ShapeError{Field: "result.type", Reason: "missing, or the result is not an object"}
	}
	if *head.Type != want {
		return &ShapeError{Field: "result.type", Reason: fmt.Sprintf("%q, want %q", *head.Type, want)}
	}
	return decode(result, v)
}

func decode(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		return &ShapeError{Reason: err.Error()}
	}
	return nil
}

// ServerVersion returns the version of the running herdr server, from `herdr api snapshot`.
func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	var r struct {
		Snapshot *struct {
			Version  *string `json:"version"`
			Protocol *uint32 `json:"protocol"`
		} `json:"snapshot"`
	}
	if err := c.result(ctx, "session_snapshot", &r, "api", "snapshot"); err != nil {
		return "", fmt.Errorf("herdr api snapshot: %w", err)
	}
	var ck check
	s := req(&ck, r.Snapshot, "snapshot")
	v := req(&ck, s.Version, "snapshot.version")
	req(&ck, s.Protocol, "snapshot.protocol")
	if ck.err == nil {
		if _, _, err := parseVersion(v); err != nil {
			ck.fail("snapshot.version", err.Error())
		}
	}
	if ck.err != nil {
		return "", fmt.Errorf("herdr api snapshot: %w", ck.err)
	}
	return v, nil
}

// ClientVersion returns the version of the herdr binary, from `herdr --version`. It is for
// reports only: the server version decides compatibility.
func (c *Client) ClientVersion(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "--version")
	if err != nil {
		return "", fmt.Errorf("herdr --version: %w", err)
	}
	v, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "herdr ")
	if !ok || v == "" {
		return "", fmt.Errorf("herdr --version: %w", &ShapeError{Reason: fmt.Sprintf("%q is not \"herdr <version>\"", out)})
	}
	return v, nil
}

// Pane returns the pane with id, from `herdr pane get`. It returns ErrPaneNotFound when the
// pane closed. The returned ID can differ from id: herdr 0.9.1 answers the old id of a pane
// that moved to another workspace with that pane and its new id (testdata/pane-moved.json).
// The caller compares the ids; a pane that stays in its workspace keeps its id.
func (c *Client) Pane(ctx context.Context, id string) (Pane, error) {
	if err := checkPaneID(id); err != nil {
		return Pane{}, fmt.Errorf("herdr pane get: %w", err)
	}
	var r struct {
		Pane *rawPane `json:"pane"`
	}
	if err := c.result(ctx, "pane_info", &r, "pane", "get", id); err != nil {
		return Pane{}, fmt.Errorf("herdr pane get: %w", err)
	}
	p, err := r.Pane.parse("pane")
	if err != nil {
		return Pane{}, fmt.Errorf("herdr pane get: %w", err)
	}
	return p, nil
}

// Panes returns every pane of the target session, from `herdr pane list`.
func (c *Client) Panes(ctx context.Context) ([]Pane, error) {
	var r struct {
		Panes *[]*rawPane `json:"panes"`
	}
	if err := c.result(ctx, "pane_list", &r, "pane", "list"); err != nil {
		return nil, fmt.Errorf("herdr pane list: %w", err)
	}
	ps, err := parsePanes(r.Panes)
	if err != nil {
		return nil, fmt.Errorf("herdr pane list: %w", err)
	}
	return ps, nil
}

// parsePanes parses the panes field of a pane_list result.
func parsePanes(panes *[]*rawPane) ([]Pane, error) {
	var ck check
	raw := req(&ck, panes, "panes")
	if ck.err != nil {
		return nil, ck.err
	}
	ps := make([]Pane, len(raw))
	for i, rp := range raw {
		p, err := rp.parse(fmt.Sprintf("panes[%d]", i))
		if err != nil {
			return nil, err
		}
		ps[i] = p
	}
	return ps, nil
}

// checkPaneID refuses a pane id that herdr would not print, before it reaches the command
// line.
func checkPaneID(id string) error {
	if !paneIDRe.MatchString(id) {
		return fmt.Errorf("malformed pane id %q", id)
	}
	return nil
}

// Workspaces returns the workspaces of the target session, from `herdr workspace list`.
func (c *Client) Workspaces(ctx context.Context) ([]Workspace, error) {
	var r struct {
		Workspaces *[]rawWorkspace `json:"workspaces"`
	}
	if err := c.result(ctx, "workspace_list", &r, "workspace", "list"); err != nil {
		return nil, fmt.Errorf("herdr workspace list: %w", err)
	}
	var ck check
	raw := req(&ck, r.Workspaces, "workspaces")
	ws := make([]Workspace, len(raw))
	for i, w := range raw {
		ws[i] = w.parse(&ck, fmt.Sprintf("workspaces[%d]", i))
	}
	if ck.err != nil {
		return nil, fmt.Errorf("herdr workspace list: %w", ck.err)
	}
	return ws, nil
}

// Sessions returns every herdr session, from `herdr session list --json`. It needs no
// Target: the list is the same from every session. environ is as for New.
func Sessions(ctx context.Context, environ []string) ([]Session, error) {
	bin, err := binary(environ)
	if err != nil {
		return nil, err
	}
	out, err := run(ctx, bin, environ, "session", "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("herdr session list: %w", err)
	}
	var r struct {
		Sessions *[]rawSession `json:"sessions"`
	}
	if err := decode(out, &r); err != nil {
		return nil, fmt.Errorf("herdr session list: %w", err)
	}
	var ck check
	raw := req(&ck, r.Sessions, "sessions")
	ss := make([]Session, len(raw))
	for i, s := range raw {
		ss[i] = s.parse(&ck, fmt.Sprintf("sessions[%d]", i))
	}
	if ck.err != nil {
		return nil, fmt.Errorf("herdr session list: %w", ck.err)
	}
	return ss, nil
}

// PluginPaneSpec says which plugin pane to open, and where.
type PluginPaneSpec struct {
	Plugin     string   // plugin id
	Entrypoint string   // [[panes]] id in the plugin manifest
	Placement  string   // overlay, split, tab or zoomed
	TargetPane string   // pane to open beside
	Env        []string // KEY=VALUE for the pane process
}

// OpenPluginPane opens a plugin pane with `herdr plugin pane open`. It never takes focus:
// Brain opens its review pane beside the caller.
func (c *Client) OpenPluginPane(ctx context.Context, s PluginPaneSpec) (PluginPane, error) {
	args := []string{"plugin", "pane", "open", "--plugin", s.Plugin, "--entrypoint", s.Entrypoint,
		"--placement", s.Placement, "--target-pane", s.TargetPane, "--no-focus"}
	for _, kv := range s.Env {
		args = append(args, "--env", kv)
	}
	var r struct {
		PluginPane *struct {
			PluginID   *string  `json:"plugin_id"`
			Entrypoint *string  `json:"entrypoint"`
			Pane       *rawPane `json:"pane"`
		} `json:"plugin_pane"`
	}
	if err := c.result(ctx, "plugin_pane_opened", &r, args...); err != nil {
		return PluginPane{}, fmt.Errorf("herdr plugin pane open: %w", err)
	}
	var ck check
	raw := req(&ck, r.PluginPane, "plugin_pane")
	pp := PluginPane{
		PluginID:   req(&ck, raw.PluginID, "plugin_pane.plugin_id"),
		Entrypoint: req(&ck, raw.Entrypoint, "plugin_pane.entrypoint"),
	}
	if ck.err != nil {
		return PluginPane{}, fmt.Errorf("herdr plugin pane open: %w", ck.err)
	}
	p, err := raw.Pane.parse("plugin_pane.pane")
	if err != nil {
		return PluginPane{}, fmt.Errorf("herdr plugin pane open: %w", err)
	}
	pp.Pane = p
	return pp, nil
}

// ClosePluginPane closes a plugin pane with `herdr plugin pane close`. herdr refuses a pane
// that no plugin opened.
func (c *Client) ClosePluginPane(ctx context.Context, paneID string) error {
	if err := checkPaneID(paneID); err != nil {
		return fmt.Errorf("herdr plugin pane close: %w", err)
	}
	var r struct {
		PaneID *string `json:"pane_id"`
	}
	if err := c.result(ctx, "plugin_pane_closed", &r, "plugin", "pane", "close", paneID); err != nil {
		return fmt.Errorf("herdr plugin pane close: %w", err)
	}
	var ck check
	if got := req(&ck, r.PaneID, "pane_id"); ck.err == nil && got != paneID {
		ck.fail("pane_id", fmt.Sprintf("%q, want %q", got, paneID))
	}
	if ck.err != nil {
		return fmt.Errorf("herdr plugin pane close: %w", ck.err)
	}
	return nil
}

// ShowNotification shows a herdr notification with title and body, from `herdr notification
// show`. It has no reply and no action. herdr answers with shown false, and a reason,
// when it does not show it, as when no client is attached (E14-S3 live test); that is no
// error.
func (c *Client) ShowNotification(ctx context.Context, title, body string) error {
	var r struct {
		Shown *bool `json:"shown"`
	}
	if err := c.result(ctx, "notification_show", &r, "notification", "show", title, "--body", body); err != nil {
		return fmt.Errorf("herdr notification show: %w", err)
	}
	var ck check
	req(&ck, r.Shown, "shown")
	if ck.err != nil {
		return fmt.Errorf("herdr notification show: %w", ck.err)
	}
	return nil
}

// InvokeAction invokes the plugin action id, such as "brain.start", with `herdr plugin action
// invoke`. herdr starts the action and does not wait for its end.
func (c *Client) InvokeAction(ctx context.Context, id string) error {
	if err := c.result(ctx, "plugin_action_invoked", &struct{}{}, "plugin", "action", "invoke", id); err != nil {
		return fmt.Errorf("herdr plugin action invoke: %w", err)
	}
	return nil
}

// Process is a foreground process of a pane.
type Process struct {
	Name string
	Argv []string // nil when herdr cannot read it
	PID  int
}

// PaneProcesses returns the foreground processes of the pane with id, from `herdr pane
// process-info`. It returns ErrPaneNotFound when the pane closed.
func (c *Client) PaneProcesses(ctx context.Context, id string) ([]Process, error) {
	pi, err := c.PaneProcessInfo(ctx, id)
	if err != nil {
		return nil, err
	}
	return pi.Processes, nil
}
