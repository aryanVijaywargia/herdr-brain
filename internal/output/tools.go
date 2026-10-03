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
func Runnable(ctx context.Context, s *store.Store, out string, id int64) (store.Tool, string, error) {
	var it store.Item
	var t store.Tool
	var cur store.ItemRevision
	err := s.Write(ctx, func(tx *sql.Tx) (err error) {
		if it, err = store.ItemByID(ctx, tx, id); err != nil || it.Kind != store.KindTool {
			return err
		}
		if t, err = store.ToolByID(ctx, tx, id); err != nil {
			return err
		}
		cur, err = store.ItemRevisionByKey(ctx, tx, id, it.CurrentRevision)
		return err
	})
	if err != nil {
		return store.Tool{}, "", err
	}
	switch {
	case it.Kind != store.KindTool:
		return store.Tool{}, "", fmt.Errorf("item %d is a %s, not a tool: %w", id, it.Kind, ErrMayNotRun)
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

func ptrString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
