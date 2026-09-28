package output

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// part is one file of a generation: its manifest name, its text and the item revisions in it.
type part struct {
	name      string
	text      string
	revisions []store.RevisionRef
}

// render reads the newest generation and the active items in one store transaction, and
// renders the parts of that generation: the global part and one part for each workspace. So
// the text and the revisions in the manifest come from the same read. gen is 0 when no commit
// recorded a generation yet.
func render(ctx context.Context, s *store.Store) (gen int64, parts []part, err error) {
	var revs []store.ItemRevision
	var keys []string
	err = s.Write(ctx, func(tx *sql.Tx) error {
		if gen, err = store.NewestGeneration(ctx, tx); err != nil {
			return err
		}
		if revs, err = store.ActiveRevisions(ctx, tx); err != nil {
			return err
		}
		keys, err = store.WorkspaceKeys(ctx, tx)
		return err
	})
	if err != nil || gen == 0 {
		return 0, nil, err
	}
	global, byKey := split(revs)
	revision := make(map[int64]int64, len(revs))
	for _, r := range revs {
		revision[r.ItemID] = r.Revision
	}
	p, err := renderPart(globalName, global, revision, brief.Global)
	if err != nil {
		return 0, nil, err
	}
	parts = append(parts, p)
	for _, k := range keys {
		p, err := renderPart(workspaceName(k), byKey[k], revision, brief.Workspace)
		if err != nil {
			return 0, nil, fmt.Errorf("workspace %s: %w", k, err)
		}
		parts = append(parts, p)
	}
	return gen, parts, nil
}

// split sorts the active items into the global items and the items of each workspace. One
// global part serves every workspace, so a conflict between a global item and a workspace
// item shows on the workspace item only: in the global part it would show in every workspace.
func split(revs []store.ItemRevision) (global []brief.Item, byKey map[string][]brief.Item) {
	scope := make(map[int64]store.Scope, len(revs))
	for _, r := range revs {
		scope[r.ItemID] = r.Scope
	}
	// toGlobal holds, for each workspace item, the global items that list it as a conflict.
	toGlobal := make(map[int64][]int64)
	byKey = make(map[string][]brief.Item)
	for _, r := range revs {
		it := brief.Item{ID: r.ItemID, Text: r.Text}
		if r.ExceptionToItem != nil {
			it.ExceptionTo = *r.ExceptionToItem
		}
		if r.Scope == store.ScopeWorkspace {
			it.ConflictsWith = r.ConflictsWith
			byKey[*r.WorkspaceKey] = append(byKey[*r.WorkspaceKey], it)
			continue
		}
		for _, id := range r.ConflictsWith {
			if scope[id] == store.ScopeWorkspace {
				toGlobal[id] = append(toGlobal[id], r.ItemID)
				continue
			}
			it.ConflictsWith = append(it.ConflictsWith, id)
		}
		global = append(global, it)
	}
	for _, items := range byKey {
		for i, it := range items {
			ids := append(slices.Clone(it.ConflictsWith), toGlobal[it.ID]...)
			items[i].ConflictsWith = slices.Compact(slices.Sorted(slices.Values(ids)))
		}
	}
	return global, byKey
}

// renderPart renders one part with fn and lists the item revisions in it.
func renderPart(name string, items []brief.Item, revision map[int64]int64,
	fn func([]brief.Item) (string, error)) (part, error) {
	text, err := fn(items)
	if err != nil {
		return part{}, err
	}
	p := part{name: name, text: text, revisions: []store.RevisionRef{}}
	for _, it := range items {
		p.revisions = append(p.revisions, store.RevisionRef{Item: it.ID, Revision: revision[it.ID]})
	}
	return p, nil
}
