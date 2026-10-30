package hook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/launch"
	"github.com/aryanVijaywargia/herdr-brain/internal/resolver"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// launchBusyTimeout is how long the brief of a launch waits for the store lock: 5 s, as the
// launch command's other store writes.
const launchBusyTimeout = 5 * time.Second

// LaunchBrief compiles the start brief of a new launch in pane paneID of workspace workspaceID
// on the herdr server of binding b, as the start hook gives it to a new agent session, for a
// CLI that takes the brief with a flag (E24-S4). agentSession is the agent session that the
// user's args name, such as the session of a resume; "" when they name none. The last session
// line leaves it out, as the start hook leaves out its own session. LaunchBrief keeps the
// receipt of the brief in the launch folder dir (launch.StartFile), and the start hook of the
// launch writes it with the agent session of its input. It returns the brief text, "" when the
// receipt withholds the brief. A store with no built brief yet gives no brief and keeps no
// receipt, as the start hook writes none then.
func LaunchBrief(ctx context.Context, b bindings.Binding, workspaceID, paneID, agentSession, dir string, now func() time.Time) (text string, err error) {
	d, key, err := delivery(&resolver.Resolved{Binding: b, PaneID: paneID, WorkspaceID: workspaceID})
	if err != nil {
		return "", err
	}
	s, err := store.Open(ctx, b.Store, launchBusyTimeout)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	h := Hook{Now: now}
	err = s.Write(ctx, func(tx *sql.Tx) (err error) {
		d, text, err = h.compile(ctx, tx, b.Store, d, key, agentSession)
		return err
	})
	// The out folder has no current generation before the first approved item.
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	// 0600: the launch files are only yours.
	if err := os.WriteFile(filepath.Join(dir, launch.StartFile), data, 0o600); err != nil {
		return "", err
	}
	return text, nil
}

// launched is the start hook of a launched session (E24-S4), with the start input in. The
// pane is the pane of the launch record, after launch.Check. A CLI that takes the brief with a
// launch flag (Claude) has the receipt of the launch's brief in the launch folder: the hook
// writes that receipt for the agent session of in and prints nothing. Else (Codex) the hook
// prints the brief of the current generation, as the start hook of a session that Brain did
// not launch. Each receipt names the launch record.
func (h Hook) launched(ctx context.Context, in adapter.Input, stdout io.Writer) (err error) {
	l, err := h.checkLaunch(ctx)
	if err != nil {
		return err
	}
	s, err := store.Open(ctx, l.Binding.Store, busyTimeout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	rc := &receipts{s: s, profile: h.Profile.Kind, agentSession: in.SessionID, transcript: in.TranscriptPath,
		now: h.Now, launch: l.ID}
	flag, err := launch.FlagBrief(l.CLI)
	if err != nil {
		return err
	}
	if !flag {
		r := &resolver.Resolved{Binding: l.Binding, PaneID: l.PaneID, WorkspaceID: l.WorkspaceID, Store: rc}
		return h.deliver(ctx, rc, r, stdout)
	}
	// A launch that could not compile its brief wrote no start file: no brief, so no receipt.
	data, err := os.ReadFile(filepath.Join(h.Launch, launch.StartFile))
	if err != nil {
		return err
	}
	var d store.Delivery
	if err := json.Unmarshal(data, &d); err != nil {
		return fmt.Errorf("read %s: %w", launch.StartFile, err)
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		if err := launchRunning(ctx, tx, l.ID); err != nil {
			return err
		}
		return rc.insert(ctx, tx, d)
	})
}

// checkLaunch checks the launch folder of the hook with launch.Check, for the CLI of the
// profile of the hook.
func (h Hook) checkLaunch(ctx context.Context) (launch.Launch, error) {
	// The herdr lookup of the check ends by the lookup deadline, as the resolver's does.
	lctx, cancel := context.WithDeadline(ctx, h.Start.Add(lookupDeadline))
	l, err := launch.Check(lctx, h.Bindings, h.Launch, h.Environ)
	cancel()
	if err != nil {
		return launch.Launch{}, err
	}
	if l.CLI != h.Profile.Kind {
		return launch.Launch{}, fmt.Errorf("launch of %s, hook of profile %s", l.CLI, h.Profile.Kind)
	}
	return l, nil
}
