package brief

import (
	"strings"
	"testing"
)

// The item text of a claim is "When <situation>: <rule>" with no ".:" and no double end mark
// (#284).
func TestClaimText(t *testing.T) {
	for _, tc := range []struct{ situation, rule, want string }{
		{"Running the full project check in notes-api.", "Run npm run lint && npm test && npm run build.",
			"When running the full project check in notes-api: Run npm run lint && npm test && npm run build."},
		{"installing packages", "use pnpm", "When installing packages: use pnpm"},
		{"before a release:", "update CHANGELOG.md.", "Before a release: update CHANGELOG.md."},
		{"NPM installs!", "use pnpm..", "When NPM installs: use pnpm."},
		{"GitHub pushes", "run the tests first", "When GitHub pushes: run the tests first"},
		{"  ", "Use pnpm.", "Use pnpm."},
	} {
		if got := ClaimText(tc.situation, tc.rule); got != tc.want {
			t.Errorf("ClaimText(%q, %q) = %q, want %q", tc.situation, tc.rule, got, tc.want)
		}
	}
}

// Old item text with ".:" or a double end mark reads clean; an ellipsis stays (#284).
func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"Running the full project check in notes-api.: Run npm run lint && npm test": "Running the full project check in notes-api: Run npm run lint && npm test",
		"Use pnpm..":       "Use pnpm.",
		"Is it done?!":     "Is it done?",
		"Wait for it...":   "Wait for it...",
		"Use npm for all.": "Use npm for all.",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

// The brief shows an old item with ".:" clean.
func TestOldItemTextIsClean(t *testing.T) {
	w, err := Workspace([]Item{{ID: 5, Text: "Running the full project check in notes-api.: Run npm run lint.."}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "- [5] Running the full project check in notes-api: Run npm run lint.\n"; !strings.Contains(w, want) {
		t.Errorf("workspace part = %q, want the line %q", w, want)
	}
}
