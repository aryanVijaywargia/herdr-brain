package brief

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// render builds both parts and the whole brief. A part error goes into the output in place
// of the brief, so the golden file of a failed build holds the error text.
func render(global, workspace []Item, skills ...[]Skill) string {
	var gs, ws []Skill
	if len(skills) == 2 {
		gs, ws = skills[0], skills[1]
	}
	g, gErr := Global(global, gs...)
	w, wErr := Workspace(workspace, ws...)
	if err := errors.Join(gErr, wErr); err != nil {
		return "error: " + err.Error() + "\n"
	}
	return Text(g, w)
}

func TestGolden(t *testing.T) {
	npm := Item{ID: 3, Text: "Use npm for installs."}
	tests := []struct {
		name            string
		global          []Item
		workspace       []Item
		globalSkills    []Skill
		workspaceSkills []Skill
	}{
		{name: "empty"},
		{name: "global-only", global: []Item{
			{ID: 7, Text: "Run the tests before a commit."},
			npm,
			{ID: 12, Text: "Write commit messages in the imperative.\nKeep the subject under 72 characters.\nNo trailing period.\n"},
		}},
		{
			name:      "global-and-workspace",
			global:    []Item{npm},
			workspace: []Item{{ID: 20, Text: "The API server listens on port 8080."}, {ID: 15, Text: "Deploy with make deploy.\nNever deploy on Fridays."}},
		},
		{
			name:      "exception",
			global:    []Item{npm},
			workspace: []Item{{ID: 9, Text: "Use pnpm in this repo.", ExceptionTo: 3}},
		},
		{
			name:      "conflict",
			global:    []Item{npm, {ID: 5, Text: "Use tabs for indentation."}},
			workspace: []Item{{ID: 9, Text: "Use yarn in this repo.", ConflictsWith: []int64{5, 3}}},
		},
		{
			// AC E16-S3: global and workspace skill entries, one with a required-tool note.
			name:   "skills",
			global: []Item{npm},
			globalSkills: []Skill{{Name: "release-notes", Description: "Write the release notes for a new version tag.",
				Path: "/home/u/.local/share/brain/main/skills/41/r1/SKILL.md", NotAvailable: "gh in codex"}},
			workspace: []Item{{ID: 15, Text: "Run the tests with make test."}},
			workspaceSkills: []Skill{
				{Name: "sqlite-migration", Description: "Add a database migration for the SQLite cache of the CLI.",
					Path: "/home/u/.local/share/brain/main/skills/43/r2/SKILL.md"},
				{Name: "pg-migration", Description: "Add a database migration for the Postgres orders service.",
					Path: "/home/u/.local/share/brain/main/skills/42/r1/SKILL.md"},
			},
		},
		{
			name:            "skills-only",
			workspaceSkills: []Skill{{Name: "deploy", Description: "Deploy the site.", Path: "/s/skills/7/r1/SKILL.md"}},
		},
		{
			// AC E16-S6 (S72): a disputed item and a disputed skill stay in the brief, with the mark.
			name:   "disputed",
			global: []Item{npm},
			workspace: []Item{
				{ID: 15, Text: "Run the tests with make test."},
				{ID: 16, Text: "The config loader is in internal/config/load.go.",
					Dispute: Dispute{Anchors: []string{"internal/config/load.go"}, At: time.Date(2026, 10, 3, 23, 30, 0, 0, time.FixedZone("x", -3600))}},
				{ID: 17, Text: "Docs: docs/a.md and docs/b.md.", ConflictsWith: []int64{3},
					Dispute: Dispute{Anchors: []string{"docs/a.md", "docs/b.md"}, At: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)}},
			},
			workspaceSkills: []Skill{{Name: "deploy", Description: "Deploy with scripts/deploy.sh.", Path: "/s/skills/7/r1/SKILL.md",
				Dispute: Dispute{Anchors: []string{"scripts/deploy.sh"}, At: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}}},
		},
		{
			name:      "over-budget",
			global:    []Item{{ID: 1, Text: strings.Repeat("g", globalBudget)}},
			workspace: []Item{{ID: 2, Text: strings.Repeat("w", workspaceBudget)}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := render(tt.global, tt.workspace, tt.globalSkills, tt.workspaceSkills)
			path := filepath.Join("testdata", tt.name+".golden")
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("render = \n%s\nwant (%s)\n%s", got, path, want)
			}
		})
	}
}

// TestLargestBrief fills both parts to their budgets exactly. The whole brief, with the markers,
// the precedence line and the labels, must still be within the whole budget.
func TestLargestBrief(t *testing.T) {
	global := fill(t, len(frame)+len(globalLabel), globalBudget)
	workspace := fill(t, len(workspaceLabel), workspaceBudget)
	g, err := Global(global)
	if err != nil {
		t.Fatalf("Global(full) error = %v, want nil", err)
	}
	w, err := Workspace(workspace)
	if err != nil {
		t.Fatalf("Workspace(full) error = %v, want nil", err)
	}
	if got := len(Text(g, w)); got != briefBudget {
		t.Errorf("len(Text(largest)) = %d, want %d", got, briefBudget)
	}
}

// fill returns items whose part, with used bytes of fixed text, is exactly limit bytes.
func fill(t *testing.T, used, limit int) []Item {
	t.Helper()
	var items []Item
	for id := int64(1); used < limit; id++ {
		overhead := len("- [" + strconv.FormatInt(id, 10) + "] \n")
		n := min(100, limit-used-overhead) // [NEW] test item length
		if n < 1 {
			t.Fatalf("fill: %d bytes left, too few for item %d", limit-used, id)
		}
		items = append(items, Item{ID: id, Text: strings.Repeat("x", n)})
		used += overhead + n
	}
	return items
}

func TestBudgetError(t *testing.T) {
	tests := []struct {
		name  string
		build func([]Item, ...Skill) (string, error)
		used  int
		limit int
		part  string
	}{
		{name: "global", build: Global, used: len(frame) + len(globalLabel), limit: globalBudget, part: "global"},
		{name: "workspace", build: Workspace, used: len(workspaceLabel), limit: workspaceBudget, part: "workspace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items := fill(t, tt.used, tt.limit)
			items[len(items)-1].Text += "x"
			_, err := tt.build(items)
			var be *BudgetError
			if !errors.As(err, &be) {
				t.Fatalf("build(one byte over) error = %v, want *BudgetError", err)
			}
			want := BudgetError{Part: tt.part, Size: tt.limit + 1, Limit: tt.limit}
			if *be != want {
				t.Errorf("build(one byte over) error = %+v, want %+v", *be, want)
			}
		})
	}
}

func TestItemLines(t *testing.T) {
	if _, err := Global([]Item{{ID: 4, Text: "one\ntwo\nthree\n"}}); err != nil {
		t.Errorf("Global(3 lines) error = %v, want nil", err)
	}
	_, err := Workspace([]Item{{ID: 4, Text: "one\ntwo\nthree\nfour"}})
	if want := "item 4 has 4 lines, over the brief limit of 3"; err == nil || err.Error() != want {
		t.Errorf("Workspace(4 lines) error = %v, want %q", err, want)
	}
}

// AC E16-S3: the skill list counts in the part budget (§8.1). A part whose items fit exactly
// is over its budget with one skill more.
func TestSkillBudget(t *testing.T) {
	sk := Skill{Name: "deploy", Description: "Deploy the site.", Path: "/s/skills/7/r1/SKILL.md"}
	list := len(skillsLabel + skillsHint + "- deploy: Deploy the site. File: /s/skills/7/r1/SKILL.md\n")
	items := fill(t, len(workspaceLabel)+list, workspaceBudget)
	w, err := Workspace(items, sk)
	if err != nil {
		t.Fatalf("Workspace(full with skill) error = %v, want nil", err)
	}
	if len(w) != workspaceBudget {
		t.Errorf("len(Workspace(full with skill)) = %d, want %d", len(w), workspaceBudget)
	}
	items[len(items)-1].Text += "x"
	var be *BudgetError
	if _, err := Workspace(items, sk); !errors.As(err, &be) {
		t.Errorf("Workspace(one byte over with skill) error = %v, want *BudgetError", err)
	}
}

// TestDisputeBudget checks that the budget keeps room for the dispute mark of every anchor
// (§9.1): a full part with anchors still fits after a dispute marks an item, with the same
// size, and an anchored item with no room for its mark is over the budget at once.
func TestDisputeBudget(t *testing.T) {
	anchors := []string{"a.go", "b.go"}
	room := len(" (disputed: a.go, b.go changed on 2026-10-03)")
	items := fill(t, len(workspaceLabel)+room, workspaceBudget)
	items[0].Anchors = anchors
	w, err := Workspace(items)
	if err != nil {
		t.Fatalf("Workspace(full with room) error = %v, want nil", err)
	}
	size, _ := WorkspaceSize(w)
	items[0].Dispute = Dispute{Anchors: []string{"a.go"}, At: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)}
	if w, err = Workspace(items); err != nil {
		t.Fatalf("Workspace(full, disputed) error = %v, want nil", err)
	}
	if !strings.Contains(w, " (disputed: a.go changed on 2026-10-03)\n") {
		t.Errorf("part = %q, want the mark", w)
	}
	if got, _ := WorkspaceSize(w); got <= size || got > workspaceBudget {
		t.Errorf("size after the dispute = %d, want more than %d and within %d", got, size, workspaceBudget)
	}
	items[0].Dispute = Dispute{Anchors: anchors, At: items[0].Dispute.At}
	if w, err = Workspace(items); err != nil || len(w) != workspaceBudget {
		t.Errorf("Workspace(full, both anchors disputed) = %d bytes, %v, want exactly the budget", len(w), err)
	}
	items[0].Dispute = Dispute{}
	items[1].Anchors = []string{"c.go"}
	var be *BudgetError
	if _, err := Workspace(items); !errors.As(err, &be) {
		t.Errorf("Workspace(an anchor with no room) error = %v, want *BudgetError", err)
	}
}

// TestDeterministic checks that the input order does not change the bytes.
func TestDeterministic(t *testing.T) {
	a := []Item{{ID: 2, Text: "b", ConflictsWith: []int64{9, 1}}, {ID: 1, Text: "a"}, {ID: 3, Text: "c"}}
	b := []Item{{ID: 3, Text: "c"}, {ID: 1, Text: "a"}, {ID: 2, Text: "b", ConflictsWith: []int64{1, 9}}}
	sa := []Skill{{Name: "b", Description: "b.", Path: "/b"}, {Name: "a", Description: "a.", Path: "/a"}}
	sb := []Skill{sa[1], sa[0]}
	if x, y := render(a, a, sa, sa), render(b, b, sb, sb); x != y {
		t.Errorf("render(order a) = %q, render(order b) = %q, want the same", x, y)
	}
	if a[0].ID != 2 || a[0].ConflictsWith[0] != 9 {
		t.Errorf("render changed its input: %+v", a)
	}
}

// The sizes that brain status shows are the sizes that the budget counts: a full part is at
// its limit.
func TestSizes(t *testing.T) {
	g, err := Global(fill(t, len(frame)+len(globalLabel), globalBudget))
	if err != nil {
		t.Fatal(err)
	}
	w, err := Workspace(fill(t, len(workspaceLabel), workspaceBudget))
	if err != nil {
		t.Fatal(err)
	}
	if size, limit := GlobalSize(g); size != globalBudget || limit != globalBudget {
		t.Errorf("GlobalSize(full) = %d, %d, want %d, %d", size, limit, globalBudget, globalBudget)
	}
	if size, limit := WorkspaceSize(w); size != workspaceBudget || limit != workspaceBudget {
		t.Errorf("WorkspaceSize(full) = %d, %d, want %d, %d", size, limit, workspaceBudget, workspaceBudget)
	}
}

// AC E15-S4: the last session line goes at the end of the workspace part, in the zone of the
// time; a workspace part that the generation does not hold yet gets its label first.
func TestLastSession(t *testing.T) {
	at := time.Date(2026, 10, 2, 14, 5, 59, 0, time.FixedZone("IST", 5*3600+1800))
	line := "Last session in this workspace: claude, 2026-10-02 14:05, transcript /t/s.jsonl\n"
	part, err := Workspace([]Item{{ID: 4, Text: "use make"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, workspace, want string }{
		{"workspace part", part, part + line},
		{"no workspace part", "", workspaceLabel + line},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LastSession(tc.workspace, "claude", at, "/t/s.jsonl"); got != tc.want {
				t.Errorf("LastSession(%q) = %q, want %q", tc.workspace, got, tc.want)
			}
		})
	}
}
