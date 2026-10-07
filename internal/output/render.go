package output

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/brief"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
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
// revisions, the active skills, tools and automations, every tools row, the dispute marks and
// the workspace keys.
type state struct {
	gen                 int64
	revs, skills, tools []store.ItemRevision
	automations         []store.Automation
	rows                toolRows
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
	if st.automations, err = store.ActiveAutomations(ctx, tx); err != nil {
		return state{}, err
	}
	if st.rows, err = store.Tools(ctx, tx); err != nil {
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
// not a brief item: its text is its contract, and the catalog of each workspace part lists it
// (§10.1, E17-S5). An automation is not one either: its text is its binding (§10.4).
func (st state) render(dir string) (parts []part, files []revFile, err error) {
	global, byKey, revision := st.items()
	globalSkills, skillsByKey, files, err := splitSkills(st.skills, st.m, st.rows, dir)
	if err != nil {
		return nil, nil, err
	}
	p, err := renderPart(globalName, global, revision, globalSkills, brief.Global)
	if err != nil {
		return nil, nil, err
	}
	parts = append(parts, p)
	for _, k := range st.keys {
		catalog, err := st.rows.catalog(k)
		if err != nil {
			return nil, nil, fmt.Errorf("workspace %s: %w", k, err)
		}
		p, err := renderPart(workspaceName(k), byKey[k], revision, skillsByKey[k],
			func(items []brief.Item, skills ...brief.Skill) (string, error) {
				return brief.WorkspaceCatalog(items, catalog, skills...)
			})
		if err != nil {
			return nil, nil, fmt.Errorf("workspace %s: %w", k, err)
		}
		parts = append(parts, p)
	}
	return parts, files, nil
}

// items sorts the active brief items of st into the global items and the items of each
// workspace, with their marks, and maps each item to its revision. A skill, a tool or an
// automation is not a brief item.
func (st state) items() (global []brief.Item, byKey map[string][]brief.Item, revision map[int64]int64) {
	own := make(map[int64]bool, len(st.skills)+len(st.tools)+len(st.automations))
	for _, r := range slices.Concat(st.skills, st.tools) {
		own[r.ItemID] = true
	}
	for _, a := range st.automations {
		own[a.ID] = true
	}
	revs := slices.DeleteFunc(st.revs, func(r store.ItemRevision) bool { return own[r.ItemID] })
	global, byKey = split(revs, st.m)
	revision = make(map[int64]int64, len(revs))
	for _, r := range revs {
		revision[r.ItemID] = r.Revision
	}
	return global, byKey, revision
}

// Items reads, in tx, the items that the brief of a pane of workspace key delivers, with the
// same selection and the same marks as the built parts: the global items, and the items of key
// when key is not "" (§8.6, E20-S2). A pane of an unmapped or withheld workspace passes "": its
// brief has the global part only (§8.3 step 4). revision maps each item to its revision.
func Items(ctx context.Context, tx *sql.Tx, key string) (items []brief.Item, revision map[int64]int64, err error) {
	st, err := read(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	global, byKey, revision := st.items()
	if key == "" {
		return global, revision, nil
	}
	return append(global, byKey[key]...), revision, nil
}

// toolRows are the tools rows, in approval order (store.Tools).
type toolRows []store.Tool

// sees says whether a pane of workspace key sees tool t: a global tool, or a tool of key. ""
// sees the global tools only.
func sees(t store.Tool, key string) bool {
	return t.Scope == store.ScopeGlobal || (key != "" && t.WorkspaceKey != nil && *t.WorkspaceKey == key)
}

// catalog lists the ready tools that the panes of workspace key see, in approval order, with
// the names of their inputs in name order (§10.1). brief.WorkspaceCatalog keeps the first 10.
func (rs toolRows) catalog(key string) ([]brief.Tool, error) {
	var ts []brief.Tool
	for _, t := range rs {
		if !t.Ready() || !sees(t, key) {
			continue
		}
		c, err := tool.Parse(t.Contract)
		if err != nil {
			return nil, fmt.Errorf("tool item %d: %w", t.ID, err)
		}
		ts = append(ts, brief.Tool{Name: t.Name, Description: c.Description, Inputs: slices.Sorted(maps.Keys(c.Inputs))})
	}
	return ts, nil
}

// required finds the Brain tools among the required tools of a skill (a list joined by
// commas, or "none") that a pane of workspace key sees, and of them the ones that brain run
// refuses: not approved, or stopped (§8.2). Each name resolves as brain run finds it
// (store.ToolByName): a tool of the workspace first, then a global one; the approved one, then
// the newest. A retired tool is not found, so a name with only retired tools is not approved.
// A name that no tools row has is not a Brain tool, such as gh. The build does not read the
// tool files: a hand-edited tool file stops the build itself (§7.5 step 5).
func (rs toolRows) required(list, key string) (brain, stopped []string) {
	for _, name := range strings.Split(list, ",") {
		name = strings.TrimSpace(name)
		var found, pick *store.Tool
		for i, t := range rs {
			if t.Name != name || !sees(t, key) {
				continue
			}
			found = &rs[i]
			if t.State != store.StateRetired && (pick == nil || before(t, *pick)) {
				pick = &rs[i]
			}
		}
		if found == nil {
			continue
		}
		brain = append(brain, name)
		if pick == nil || !pick.Ready() {
			stopped = append(stopped, name)
		}
	}
	return brain, stopped
}

// before says whether brain run finds tool a before tool b of the same name (store.ToolByName).
func before(a, b store.Tool) bool {
	if a.Scope != b.Scope {
		return a.Scope == store.ScopeWorkspace
	}
	if (a.ApprovedHash != nil) != (b.ApprovedHash != nil) {
		return a.ApprovedHash != nil
	}
	return a.ID > b.ID
}

// skillEntry is one skill of a part: its brief entry and its file.
type skillEntry struct {
	entry brief.Skill
	file  revFile
}

// splitSkills sorts the active skills into the global skills and the skills of each
// workspace, with the mark of each disputed one and the note on its required Brain tools in
// rows, and lists the file of each one in the skills folder dir.
func splitSkills(skills []store.ItemRevision, m marks, rows toolRows, dir string) (global []skillEntry,
	byKey map[string][]skillEntry, files []revFile, err error) {
	byKey = make(map[string][]skillEntry)
	for _, r := range skills {
		sk, err := store.ParseSkill(r.Text)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("item %d: %w", r.ItemID, err)
		}
		f := revFile{path: skillPath(dir, r.ItemID, r.Revision), text: r.Text,
			ref: store.RevisionRef{Item: r.ItemID, Revision: r.Revision}}
		brain, stopped := rows.required(sk.RequiredTools, ptrString(r.WorkspaceKey))
		e := skillEntry{entry: brief.Skill{Name: sk.Name, Description: sk.Description, Path: f.path,
			NotAvailable: sk.NotAvailable, Dispute: m.dispute(r.ItemID), Anchors: m.anchors[r.ItemID],
			Tools: brain, Stopped: stopped}, file: f}
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
	return brief.Dispute{Anchors: d.Anchors, Moved: signal.Moved(d.Diff), At: d.At}
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
