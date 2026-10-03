package output

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"

	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/tool"
)

// revFile is one file of a skill or tool revision (§8.2, §10.1): its absolute path, its text
// and its (item id, revision) pair.
type revFile struct {
	path string
	text string
	ref  store.RevisionRef
}

// files are the revision files that one generation lists.
type files struct {
	skills, tools []revFile
}

// part is one file of a generation: its manifest name, its text and the item revisions in it.
type part struct {
	name      string
	text      string
	revisions []store.RevisionRef
}

// state is what one store read gives the renderer: the newest generation, the active item
// revisions, the active skills and tools, the dispute marks and the workspace keys.
type state struct {
	gen                 int64
	revs, skills, tools []store.ItemRevision
	keys                []string
	m                   marks
}

// read reads the state that render renders, in tx.
func read(ctx context.Context, tx *sql.Tx) (st state, err error) {
	if st.gen, err = store.NewestGeneration(ctx, tx); err != nil {
		return state{}, err
	}
	if st.revs, err = store.ActiveRevisions(ctx, tx); err != nil {
		return state{}, err
	}
	if st.skills, err = store.ActiveSkills(ctx, tx); err != nil {
		return state{}, err
	}
	if st.tools, err = store.ActiveTools(ctx, tx); err != nil {
		return state{}, err
	}
	if st.m.disputes, err = store.Disputes(ctx, tx); err != nil {
		return state{}, err
	}
	if st.m.anchors, err = store.AnchorPaths(ctx, tx); err != nil {
		return state{}, err
	}
	st.keys, err = store.WorkspaceKeys(ctx, tx)
	return st, err
}

// render reads the newest generation and the active items in one store transaction, and
// renders the parts of that generation: the global part and one part for each workspace. So
// the text and the revisions in the manifest come from the same read. Each active skill goes
// into the skill list of its scope part, with the path of its revision file in the skills
// folder of the out folder out, and into fs (§8.2). Each file of each active tool goes into fs
// too (§10.1). gen is 0 when no commit recorded a generation yet.
func render(ctx context.Context, s *store.Store, out string) (gen int64, parts []part, fs files, err error) {
	var st state
	err = s.Write(ctx, func(tx *sql.Tx) (err error) {
		st, err = read(ctx, tx)
		return err
	})
	if err != nil || st.gen == 0 {
		return 0, nil, files{}, err
	}
	parts, skills, err := st.render(skillsDir(out))
	if err != nil {
		return 0, nil, files{}, err
	}
	return st.gen, parts, files{skills: skills, tools: toolFiles(st.tools, toolsDir(out))}, nil
}

// toolFiles lists the files of each tool revision in revs, in the tools folder dir, in name
// order for each revision.
func toolFiles(revs []store.ItemRevision, dir string) []revFile {
	var fs []revFile
	for _, r := range revs {
		byName := tool.Files(r.Text)
		for _, name := range slices.Sorted(maps.Keys(byName)) {
			fs = append(fs, revFile{path: revisionPath(dir, r.ItemID, r.Revision, name), text: byName[name],
				ref: store.RevisionRef{Item: r.ItemID, Revision: r.Revision}})
		}
	}
	return fs
}

// CheckGlobal renders the global part from the store state in tx, with the skill files in
// the out folder out, and returns a *brief.BudgetError when it is over its budget (§8.1). A
// promotion runs it after its write, in the same transaction, so a full global part refuses
// the promotion and nothing is written (§9.3).
func CheckGlobal(ctx context.Context, tx *sql.Tx, out string) error {
	st, err := read(ctx, tx)
	if err != nil {
		return err
	}
	st.keys = nil // the workspace parts are not this check's matter
	_, _, err = st.render(skillsDir(out))
	return err
}

// render renders the parts of st, with the skill files in the skills folder dir. A tool is
// not a brief item: its text is its contract, and the catalog lists it (E17-S5).
func (st state) render(dir string) (parts []part, files []revFile, err error) {
	own := make(map[int64]bool, len(st.skills)+len(st.tools))
	for _, r := range slices.Concat(st.skills, st.tools) {
		own[r.ItemID] = true
	}
	revs := slices.DeleteFunc(st.revs, func(r store.ItemRevision) bool { return own[r.ItemID] })
	global, byKey := split(revs, st.m)
	revision := make(map[int64]int64, len(revs))
	for _, r := range revs {
		revision[r.ItemID] = r.Revision
	}
	globalSkills, skillsByKey, files, err := splitSkills(st.skills, st.m, dir)
	if err != nil {
		return nil, nil, err
	}
	p, err := renderPart(globalName, global, revision, globalSkills, brief.Global)
	if err != nil {
		return nil, nil, err
	}
	parts = append(parts, p)
	for _, k := range st.keys {
		p, err := renderPart(workspaceName(k), byKey[k], revision, skillsByKey[k], brief.Workspace)
		if err != nil {
			return nil, nil, fmt.Errorf("workspace %s: %w", k, err)
		}
		parts = append(parts, p)
	}
	return parts, files, nil
}

// skillEntry is one skill of a part: its brief entry and its file.
type skillEntry struct {
	entry brief.Skill
	file  revFile
}

// splitSkills sorts the active skills into the global skills and the skills of each
// workspace, with the mark of each disputed one, and lists the file of each one in the skills
// folder dir.
func splitSkills(skills []store.ItemRevision, m marks, dir string) (global []skillEntry, byKey map[string][]skillEntry,
	files []revFile, err error) {
	byKey = make(map[string][]skillEntry)
	for _, r := range skills {
		sk, err := store.ParseSkill(r.Text)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("item %d: %w", r.ItemID, err)
		}
		f := revFile{path: skillPath(dir, r.ItemID, r.Revision), text: r.Text,
			ref: store.RevisionRef{Item: r.ItemID, Revision: r.Revision}}
		e := skillEntry{entry: brief.Skill{Name: sk.Name, Description: sk.Description, Path: f.path,
			NotAvailable: sk.NotAvailable, Dispute: m.dispute(r.ItemID), Anchors: m.anchors[r.ItemID]}, file: f}
		files = append(files, f)
		if r.Scope == store.ScopeWorkspace {
			byKey[*r.WorkspaceKey] = append(byKey[*r.WorkspaceKey], e)
			continue
		}
		global = append(global, e)
	}
	return global, byKey, files, nil
}

// split sorts the active items into the global items and the items of each workspace. One
// global part serves every workspace, so a conflict between a global item and a workspace
// item shows on the workspace item only: in the global part it would show in every workspace.
// A disputed item gets its mark (§9.1).
func split(revs []store.ItemRevision, m marks) (global []brief.Item, byKey map[string][]brief.Item) {
	scope := make(map[int64]store.Scope, len(revs))
	for _, r := range revs {
		scope[r.ItemID] = r.Scope
	}
	// toGlobal holds, for each workspace item, the global items that list it as a conflict.
	toGlobal := make(map[int64][]int64)
	byKey = make(map[string][]brief.Item)
	for _, r := range revs {
		it := brief.Item{ID: r.ItemID, Text: r.Text, Dispute: m.dispute(r.ItemID), Anchors: m.anchors[r.ItemID]}
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

// marks holds what the brief needs for the dispute marks (§9.1): the dispute of each disputed
// item and the anchor paths of each anchored item, by item id.
type marks struct {
	disputes map[int64]store.Disputed
	anchors  map[int64][]string
}

// dispute is the brief mark of item id: none when the item is not disputed.
func (m marks) dispute(id int64) brief.Dispute {
	d := m.disputes[id]
	return brief.Dispute{Anchors: d.Anchors, At: d.At}
}

// renderPart renders one part with fn and lists the item revisions in it, the skill revisions
// too: the receipt names the skill text that the session got (§8.2).
func renderPart(name string, items []brief.Item, revision map[int64]int64, skills []skillEntry,
	fn func([]brief.Item, ...brief.Skill) (string, error)) (part, error) {
	entries := make([]brief.Skill, len(skills))
	for i, sk := range skills {
		entries[i] = sk.entry
	}
	text, err := fn(items, entries...)
	if err != nil {
		return part{}, err
	}
	p := part{name: name, text: text, revisions: []store.RevisionRef{}}
	for _, it := range items {
		p.revisions = append(p.revisions, store.RevisionRef{Item: it.ID, Revision: revision[it.ID]})
	}
	for _, sk := range skills {
		p.revisions = append(p.revisions, sk.file.ref)
	}
	return p, nil
}
