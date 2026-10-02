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
	globalLabel    = "## Global (all workspaces)\n"
	workspaceLabel = "## This workspace\n"
	// frame is the text that every brief has around its two parts.
	frame = beginMarker + "\n" + precedence + "\n" + endMarker
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
}

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
	return part("global", globalLabel, len(frame), globalBudget, items, skills)
}

// Workspace renders the workspace part from the active items and skills of one workspace.
func Workspace(items []Item, skills ...Skill) (string, error) {
	return part("workspace", workspaceLabel, 0, workspaceBudget, items, skills)
}

// GlobalSize returns the size of a rendered global part as its budget counts it, with the
// frame, and the budget (§8.1).
func GlobalSize(global string) (size, limit int) { return len(frame) + len(global), globalBudget }

// WorkspaceSize returns the size of a rendered workspace part and its budget (§8.1).
func WorkspaceSize(workspace string) (size, limit int) { return len(workspace), workspaceBudget }

// Text puts the frame around the two parts and returns the whole brief.
func Text(global, workspace string) string {
	return beginMarker + "\n" + precedence + "\n" + global + workspace + endMarker
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

// part renders a label, the items in id order and the skills in name order, so the same items
// give the same bytes. extra is the size that counts against the budget beyond the part text.
func part(name, label string, extra, limit int, items []Item, skills []Skill) (string, error) {
	items = slices.SortedFunc(slices.Values(items), func(a, b Item) int { return cmp.Compare(a.ID, b.ID) })
	var b strings.Builder
	b.WriteString(label)
	for _, it := range items {
		if err := writeItem(&b, it); err != nil {
			return "", err
		}
	}
	writeSkills(&b, skills)
	if size := extra + b.Len(); size > limit {
		return "", &BudgetError{Part: name, Size: size, Limit: limit}
	}
	return b.String(), nil
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
		b.WriteString("\n")
	}
}

// writeItem writes one item: its id and first line, then up to two more lines, indented. The
// exception and conflict marks go at the end of the last line.
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
	b.WriteString("\n")
	return nil
}
