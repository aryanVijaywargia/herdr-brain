// Package brief renders the text of the brief that the hook gives an agent at session start
// (HLD §8.1): the global part, the workspace part and the whole brief. It reads no store and
// writes no file; the compiler (E6-S2) gives it the active items.
package brief

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The frame of every brief (§8.1). The marker text is a Brain choice [NEW] (§8.1).
const (
	beginMarker = "<!-- brain:begin v1 -->"
	endMarker   = "<!-- brain:end -->"
	precedence  = `Precedence: for ordinary preferences, a workspace item wins over a global item. ` +
		`It never relaxes a global permission or safety rule. A "conflicts with" mark wins over this line.`
	// remember tells agents how to save a lesson; the call makes a draft (§8.1, #253).
	remember       = `When the user asks you to remember something for later, run: brain remember "<lesson>"`
	header         = precedence + "\n" + remember + "\n"
	globalLabel    = "## Global (all workspaces)\n"
	workspaceLabel = "## This workspace\n"
	// frame is the text that every brief has around its two parts.
	frame = beginMarker + "\n" + header + endMarker
)

// The budgets of §8.1, in bytes of UTF-8. A byte count is never below the character count that
// Claude Code limits, so the byte count is the simple safe choice [NEW]. The frame counts
// against the global part, and each part counts its own label, so the two part budgets add up
// to the whole budget [NEW]: a brief whose parts fit is never over 9,500.
const (
	briefBudget     = 9500
	globalBudget    = 3800
	workspaceBudget = 5700
	// maxItemLines is the limit for one item of the brief (§8.1).
	maxItemLines = 3
)

// Item is one active item as the brief shows it. Its fields come from the item's current
// revision (store.ItemRevision holds them, store.Item does not have the text).
type Item struct {
	ID   int64
	Text string
	// ExceptionTo is the global item that this workspace item is an exception to (§7.4), or 0.
	ExceptionTo int64
	// ConflictsWith are the items that this item has an unresolved conflict with (§7.4).
	ConflictsWith []int64
	// Dispute marks an item whose anchor moved or was deleted, until you decide (§9.1).
	Dispute Dispute
	// Anchors are the paths of the item's anchors. The part keeps room for their mark.
	Anchors []string
}

// Dispute is the mark of a disputed item or skill (§9.1, G7): the paths of the anchors that
// moved or were deleted, the paths of those that moved, and the time of the check that found
// them. A Dispute with no anchors is no mark.
type Dispute struct {
	Anchors []string
	Moved   []string
	At      time.Time
}

// Gone is the text of the anchors of d, each with what happened to it: "<anchor> moved" or
// "<anchor> deleted". [NEW] several anchors are joined with ", ".
func (d Dispute) Gone() string {
	words := make([]string, len(d.Anchors))
	for i, a := range d.Anchors {
		words[i] = a + " deleted"
		if slices.Contains(d.Moved, a) {
			words[i] = a + " moved"
		}
	}
	return strings.Join(words, ", ")
}

// mark is the text of d: " (disputed: <anchor> moved on <date>)" or " (disputed: <anchor>
// deleted on <date>)", with the date in UTC.
func (d Dispute) mark() string {
	if len(d.Anchors) == 0 {
		return ""
	}
	return " (disputed: " + d.Gone() + " on " + d.At.UTC().Format("2006-01-02") + ")"
}

// Skill is one active skill as the brief lists it (§8.2): its name, its one-line description,
// the absolute path of the file of its revision, and the note on a required tool that a CLI
// does not have ("" for none). The agent reads the file when the skill is relevant.
type Skill struct {
	Name        string
	Description string
	Path        string
	// NotAvailable comes from the reviewed text, for example "gh in codex".
	NotAvailable string
	// Dispute marks a skill whose anchor moved or was deleted, until you decide (§9.1).
	Dispute Dispute
	// Anchors are the paths of the skill's anchors. The part keeps room for their mark.
	Anchors []string
	// Tools are the Brain tools that the skill requires. The part keeps room for their note.
	Tools []string
	// Stopped are the required Brain tools that are not approved or are stopped (§8.2, E17-S5).
	Stopped []string
}

// toolNote is the note of a skill entry on its required Brain tools that are not approved or
// are stopped, or "" for none. [NEW] the note form.
func toolNote(stopped []string) string {
	if len(stopped) == 0 {
		return ""
	}
	return " (Brain tool not approved or stopped: " + strings.Join(stopped, ", ") + ")"
}

// Tool is one approved tool as the brief catalog lists it (§10.1): its name, its one-line
// description and the names of its inputs.
type Tool struct {
	Name        string
	Description string
	Inputs      []string
}

// The tool catalog at the end of the workspace part (§10.1, E17-S5). catalogSize is the §10.1
// default. [NEW] the heading, the instruction line and the entry form
// "- <name>: <description> Inputs: <input>, <input>".
const (
	catalogSize  = 10
	catalogLabel = "### Tools\n"
	catalogHint  = "Run a tool with: brain run <name> --<input> <value>. brain tools lists all tools and their inputs.\n"
)

// The skill list at the end of a part. [NEW] the heading, the instruction line and the entry
// form "- <name>: <description> File: <path>"; E16-S1 tested them with Claude and Codex.
const (
	skillsLabel = "### Skills\n"
	skillsHint  = "Read the file of a skill before you do a task that matches it.\n"
)

// BudgetError says that a part of the brief is over its budget. The part is not built: Brain
// never cuts items to make a part fit.
type BudgetError struct {
	Part  string
	Size  int
	Limit int
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("the %s part of the brief is %d characters, over its limit of %d: remove or merge an item",
		e.Part, e.Size, e.Limit)
}

// Global renders the global part from the active global items and global skills. Its size,
// with the frame of the brief, must fit the global budget; the skill list counts in it (§8.1).
func Global(items []Item, skills ...Skill) (string, error) {
	return part("global", globalLabel, len(frame), globalBudget, items, skills, nil)
}

// Workspace renders the workspace part from the active items and skills of one workspace, with
// no tool catalog.
func Workspace(items []Item, skills ...Skill) (string, error) {
	return WorkspaceCatalog(items, nil, skills...)
}

// WorkspaceCatalog is Workspace with the catalog of the tools that the workspace's panes can
// run: the first catalogSize of tools, in the order given (§10.1). The catalog counts in the
// part budget (§8.1).
func WorkspaceCatalog(items []Item, tools []Tool, skills ...Skill) (string, error) {
	return part("workspace", workspaceLabel, 0, workspaceBudget, items, skills, tools[:min(len(tools), catalogSize)])
}

// GlobalSize returns the size of a rendered global part as its budget counts it, with the
// frame, and the budget (§8.1).
func GlobalSize(global string) (size, limit int) { return len(frame) + len(global), globalBudget }

// WorkspaceSize returns the size of a rendered workspace part and its budget (§8.1).
func WorkspaceSize(workspace string) (size, limit int) { return len(workspace), workspaceBudget }

// Text puts the frame around the two parts and returns the whole brief.
func Text(global, workspace string) string {
	return beginMarker + "\n" + header + global + workspace + endMarker
}

// LastSession adds the line that names the last session of the workspace at the end of the
// workspace part (E15-S4, §8.1). The hook adds it at print time, so it is in no built part and
// in no manifest. A workspace part that the generation does not hold yet gets its label first.
// The time is in the zone of at, to the minute. [NEW] the line form.
func LastSession(workspace, harness string, at time.Time, transcript string) string {
	if workspace == "" {
		workspace = workspaceLabel
	}
	return workspace + "Last session in this workspace: " + harness + ", " + at.Format("2006-01-02 15:04") +
		", transcript " + transcript + "\n"
}

// part renders a label, the items in id order, the skills in name order and the tools in the
// order given, so the same items give the same bytes. extra is the size that counts against the
// budget beyond the part text. The budget counts the room for the dispute mark of every anchor
// too, so a later dispute always fits (§9.1 "inside the budget"): the build that marks an item
// never fails. In the same way it counts the room for the note on every required Brain tool of
// a skill, so a tool that stops later does not fail the build.
func part(name, label string, extra, limit int, items []Item, skills []Skill, tools []Tool) (string, error) {
	items = slices.SortedFunc(slices.Values(items), func(a, b Item) int { return cmp.Compare(a.ID, b.ID) })
	var b strings.Builder
	b.WriteString(label)
	for _, it := range items {
		if err := writeItem(&b, it); err != nil {
			return "", err
		}
	}
	writeSkills(&b, skills)
	writeCatalog(&b, tools)
	for _, it := range items {
		extra += markRoom(it.Anchors, it.Dispute)
	}
	for _, sk := range skills {
		extra += markRoom(sk.Anchors, sk.Dispute) + len(toolNote(sk.Tools)) - len(toolNote(sk.Stopped))
	}
	if size := extra + b.Len(); size > limit {
		return "", &BudgetError{Part: name, Size: size, Limit: limit}
	}
	return b.String(), nil
}

// markRoom is the room that the mark of anchors needs beyond the mark d that the text holds:
// a dispute names some of the anchors, never more, and "deleted" is the longer word.
func markRoom(anchors []string, d Dispute) int {
	return max(0, len(Dispute{Anchors: anchors}.mark())-len(d.mark()))
}

// writeSkills writes the skill list, one line for each skill, or nothing when there is none.
func writeSkills(b *strings.Builder, skills []Skill) {
	if len(skills) == 0 {
		return
	}
	b.WriteString(skillsLabel + skillsHint)
	for _, sk := range slices.SortedFunc(slices.Values(skills), func(a, b Skill) int { return cmp.Compare(a.Name, b.Name) }) {
		b.WriteString("- " + sk.Name + ": " + sk.Description + " File: " + sk.Path)
		if sk.NotAvailable != "" {
			b.WriteString(" (not available: " + sk.NotAvailable + ")")
		}
		b.WriteString(toolNote(sk.Stopped) + sk.Dispute.mark() + "\n")
	}
}

// writeCatalog writes the tool catalog, one line for each tool, or nothing when there is none.
func writeCatalog(b *strings.Builder, tools []Tool) {
	if len(tools) == 0 {
		return
	}
	b.WriteString(catalogLabel + catalogHint)
	for _, t := range tools {
		b.WriteString("- " + t.Name + ": " + strings.Join(strings.Fields(t.Description), " "))
		if len(t.Inputs) > 0 {
			b.WriteString(" Inputs: " + strings.Join(t.Inputs, ", "))
		}
		b.WriteString("\n")
	}
}

// writeItem writes one item: its id and first line, then up to two more lines, indented. The
// exception, conflict and dispute marks go at the end of the last line.
func writeItem(b *strings.Builder, it Item) error {
	lines := strings.Split(strings.TrimRight(it.Text, "\n"), "\n")
	if len(lines) > maxItemLines {
		return fmt.Errorf("item %d has %d lines, over the brief limit of %d", it.ID, len(lines), maxItemLines)
	}
	b.WriteString("- [" + strconv.FormatInt(it.ID, 10) + "] ")
	b.WriteString(strings.Join(lines, "\n  "))
	if it.ExceptionTo != 0 {
		fmt.Fprintf(b, " (exception to global item %d)", it.ExceptionTo)
	}
	for _, id := range slices.Sorted(slices.Values(it.ConflictsWith)) {
		fmt.Fprintf(b, " (conflicts with %d; ask the user)", id)
	}
	b.WriteString(it.Dispute.mark() + "\n")
	return nil
}
