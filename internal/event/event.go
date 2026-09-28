// Package event handles the herdr plugin events that Brain listens to (HLD §4.2, D4). herdr
// runs `brain event <name>` once for each event; the command does its work and exits. There
// is no listener process.
package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

// busyTimeout is how long the close waits for the store lock. [NEW] 5 s: the event runs
// after the close, so nobody waits for it.
const busyTimeout = 5 * time.Second

// Main runs `brain event <name>` with args after "event". environ is the process
// environment that herdr gives the event command.
func Main(ctx context.Context, args, environ []string) error {
	if len(args) != 1 || args[0] != "workspace-closed" {
		return fmt.Errorf("usage: brain event workspace-closed")
	}
	bf, err := paths.BindingsFile()
	if err != nil {
		return err
	}
	return WorkspaceClosed(ctx, environ, bf)
}

// Closed is the part of a workspace.closed event that Brain uses.
type Closed struct {
	Session     string // herdr session name; "default" when HERDR_SESSION is not set
	Socket      string // HERDR_SOCKET_PATH
	WorkspaceID string // data.workspace_id of HERDR_PLUGIN_EVENT_JSON
}

// ParseClosed reads a workspace.closed event from environ. A close event has no
// HERDR_PANE_ID, so it does not go through the resolver (D4).
func ParseClosed(environ []string) (Closed, error) {
	var ev struct {
		Data struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(lookup(environ, "HERDR_PLUGIN_EVENT_JSON")), &ev); err != nil {
		return Closed{}, fmt.Errorf("HERDR_PLUGIN_EVENT_JSON: %w", err)
	}
	c := Closed{
		Session:     lookup(environ, "HERDR_SESSION"),
		Socket:      lookup(environ, "HERDR_SOCKET_PATH"),
		WorkspaceID: ev.Data.WorkspaceID,
	}
	if c.Session == "" {
		c.Session = "default" // the default session has no HERDR_SESSION (M0)
	}
	switch {
	case c.WorkspaceID == "":
		return Closed{}, errors.New("HERDR_PLUGIN_EVENT_JSON has no data.workspace_id")
	case c.Socket == "":
		return Closed{}, errors.New("HERDR_SOCKET_PATH is not set")
	}
	return c, nil
}

// WorkspaceClosed marks the closed workspace of the event in environ closed, in the workspace
// map and the store of its binding (§9.4 "Close"). bindingsFile is the bindings file. An
// unbound session, a binding of another session, or an unknown workspace changes nothing.
func WorkspaceClosed(ctx context.Context, environ []string, bindingsFile string) (err error) {
	c, err := ParseClosed(environ)
	if err != nil {
		return err
	}
	bs, _, err := bindings.Read(bindingsFile)
	if err != nil {
		return err
	}
	b, ok, err := bindings.BySocket(bs, c.Socket)
	if err != nil || !ok || b.Session != c.Session {
		return err
	}
	st, err := store.Open(ctx, b.Store, busyTimeout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	return workspace.For(b).Close(ctx, st, c.WorkspaceID)
}

// lookup returns the value of key in environ; the last entry wins, as in os/exec.
func lookup(environ []string, key string) string {
	var v string
	for _, kv := range environ {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}
