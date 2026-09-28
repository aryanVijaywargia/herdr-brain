package brief

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// render builds both parts and the whole brief. A part error goes into the output in place
// of the brief, so the golden file of a failed build holds the error text.
func render(global, workspace []Item) string {
	g, gErr := Global(global)
	w, wErr := Workspace(workspace)
	if err := errors.Join(gErr, wErr); err != nil {
		return "error: " + err.Error() + "\n"
	}
	return Text(g, w)
}

func TestGolden(t *testing.T) {
	npm := Item{ID: 3, Text: "Use npm for installs."}
	tests := []struct {
		name      string
		global    []Item
		workspace []Item
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
			name:      "over-budget",
			global:    []Item{{ID: 1, Text: strings.Repeat("g", globalBudget)}},
			workspace: []Item{{ID: 2, Text: strings.Repeat("w", workspaceBudget)}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := render(tt.global, tt.workspace)
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
		build func([]Item) (string, error)
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

// TestDeterministic checks that the input order does not change the bytes.
func TestDeterministic(t *testing.T) {
	a := []Item{{ID: 2, Text: "b", ConflictsWith: []int64{9, 1}}, {ID: 1, Text: "a"}, {ID: 3, Text: "c"}}
	b := []Item{{ID: 3, Text: "c"}, {ID: 1, Text: "a"}, {ID: 2, Text: "b", ConflictsWith: []int64{1, 9}}}
	if x, y := render(a, a), render(b, b); x != y {
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
