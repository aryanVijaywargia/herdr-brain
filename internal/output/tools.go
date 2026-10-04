package output

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/tool"
)

// ErrMayNotRun means that a tool may not run now: it is not active, its contract is not the
// one that you approved, or its files on disk are missing or changed (§10.1, S11, S21).
var ErrMayNotRun = errors.New("the tool may not run")

// MayRun says whether tool item id in the store whose out folder is out may run, and returns
// its tools row when it may. It may run only when the item is an active tool, the hash that
// you approved is its contract hash, that hash is the hash of its current revision and scope,
// and each file of that revision on disk holds the text of the revision. Any other state wraps
// ErrMayNotRun.
func MayRun(ctx context.Context, s *store.Store, out string, id int64) (store.Tool, error) {
	t, _, err := Runnable(ctx, s, out, id)
	return t, err
}

// Runnable is MayRun, and it also returns the folder of the checked revision, whose run.sh
// brain run (E17-S4) runs. brain run calls it before each run.
func Runnable(ctx context.Context, s *store.Store, out string, id int64) (t store.Tool, dir string, err error) {
	err = s.Write(ctx, func(tx *sql.Tx) (err error) {
		t, dir, err = runnable(ctx, tx, out, id)
		return err
	})
	return t, dir, err
}

// runnable is Runnable in tx.
func runnable(ctx context.Context, tx *sql.Tx, out string, id int64) (store.Tool, string, error) {
	it, err := store.ItemByID(ctx, tx, id)
	if err != nil {
		return store.Tool{}, "", err
	}
	if it.Kind != store.KindTool {
		return store.Tool{}, "", fmt.Errorf("item %d is a %s, not a tool: %w", id, it.Kind, ErrMayNotRun)
	}
	t, err := store.ToolByID(ctx, tx, id)
	if err != nil {
		return store.Tool{}, "", err
	}
	cur, err := store.ItemRevisionByKey(ctx, tx, id, it.CurrentRevision)
	if err != nil {
		return store.Tool{}, "", err
	}
	switch {
	case it.State != store.StateActive:
		return store.Tool{}, "", fmt.Errorf("tool %d is %s: %w", id, it.State, ErrMayNotRun)
	case t.ApprovedHash == nil || *t.ApprovedHash != t.ContractHash ||
		tool.Hash(cur.Text, string(cur.Scope), ptrString(cur.WorkspaceKey)) != t.ContractHash:
		return store.Tool{}, "", fmt.Errorf("tool %d: the contract is not the one that you approved; approve it again: %w",
			id, ErrMayNotRun)
	}
	byName := tool.Files(cur.Text)
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		path := revisionPath(toolsDir(out), id, cur.Revision, name)
		b, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return store.Tool{}, "", fmt.Errorf("tool %d: %s is missing; brain repair writes it: %w", id, path, ErrMayNotRun)
		case err != nil:
			return store.Tool{}, "", err
		case string(b) != byName[name]:
			return store.Tool{}, "", fmt.Errorf("tool %d: %s changed outside Brain, a conflict; brain repair --confirm writes it again: %w",
				id, path, ErrMayNotRun)
		}
	}
	return t, filepath.Dir(revisionPath(toolsDir(out), id, cur.Revision, tool.TomlName)), nil
}

// AutomationMayRun says whether automation item id in tx, in the store whose out folder is out,
// may run now, and returns its automations row when it may (§10.4). It may run only when the
// item is an active automation, the binding is on (not paused), its tool is approved at the
// pinned contract hash, and the tool may run (the checks of MayRun, the tool files too). Any
// other state wraps ErrMayNotRun, with the reason. It takes the transaction of its caller, so
// the claim and the launch mark of a run (E17-S11) check in the transaction that writes them.
func AutomationMayRun(ctx context.Context, tx *sql.Tx, out string, id int64) (store.Automation, error) {
	it, err := store.ItemByID(ctx, tx, id)
	if err != nil {
		return store.Automation{}, err
	}
	if it.Kind != store.KindAutomation {
		return store.Automation{}, fmt.Errorf("item %d is a %s, not an automation: %w", id, it.Kind, ErrMayNotRun)
	}
	if it.State != store.StateActive {
		return store.Automation{}, fmt.Errorf("automation %d is %s: %w", id, it.State, ErrMayNotRun)
	}
	a, err := store.AutomationByID(ctx, tx, id)
	if err != nil {
		return store.Automation{}, err
	}
	if !a.Enabled {
		return store.Automation{}, fmt.Errorf("automation %d is paused: %w", id, ErrMayNotRun)
	}
	t, err := store.ToolByID(ctx, tx, a.ToolID)
	if err != nil {
		return store.Automation{}, err
	}
	if t.ApprovedHash == nil || *t.ApprovedHash != a.ContractHash {
		return store.Automation{}, fmt.Errorf("automation %d: the contract of tool %s changed after your approval of the binding; approve it again (brain automation add): %w",
			id, t.Name, ErrMayNotRun)
	}
	if _, _, err := runnable(ctx, tx, out, a.ToolID); err != nil {
		return store.Automation{}, fmt.Errorf("automation %d: %w", id, err)
	}
	return a, nil
}

func ptrString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
