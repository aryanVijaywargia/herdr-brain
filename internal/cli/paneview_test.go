package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// inZone runs the test in a fixed local zone, +05:30, so that the local times are the same on
// every machine. The cli tests do not run in parallel.
func inZone(t *testing.T) {
	t.Helper()
	old := time.Local
	time.Local = time.FixedZone("IST", 5*3600+1800)
	t.Cleanup(func() { time.Local = old })
}

// paneScreen runs the review pane of proposal id with the keys, in a pane of rows and cols (0
// for no size), and returns the screen.
func (f *fixture) paneScreen(t *testing.T, id int64, k string, rows, cols int) string {
	t.Helper()
	var out bytes.Buffer
	p := &reviewPane{Env: f.env(f.sock, "w14:p1", "w14", &out), id: id, keys: typed(k)}
	if rows > 0 {
		p.size = func() (int, int) { return rows, cols }
	}
	if err := p.run(context.Background()); err != nil {
		t.Fatalf("pane(%q) = %v\n%s", k, err, out.String())
	}
	return out.String()
}

// paneGolden compares screen with testdata/pane/<name>.txt, with each clear as a <clear> line,
// the temp folder as <tmp> and each claim id as cl_<id>. go test -update rewrites the file.
func (f *fixture) paneGolden(t *testing.T, name, screen string) {
	t.Helper()
	dir := filepath.Dir(f.store)
	canon, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	screen = strings.ReplaceAll(screen, clearScreen, "<clear>\n")
	screen = strings.ReplaceAll(strings.ReplaceAll(screen, canon, "<tmp>"), dir, "<tmp>")
	screen = claimID.ReplaceAllString(screen, "cl_<id>")
	path := filepath.Join("testdata", "pane", name+".txt")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(screen), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if screen != string(want) {
		t.Errorf("pane screen:\n%s\nwant (%s):\n%s", screen, path, want)
	}
}

var claimID = regexp.MustCompile(`cl_[0-9a-f]{16}`)

const (
	itemKeys    = "a approve  e edit  c change  r reject  l later  s scope  p pin  x conflict  d detail  q close\n"
	disputeKeys = "k keep  e edit  t retire  l later  d detail  q close\n"
)

// AC #254: the pane shows the text, the scope and the effect first, and the source lines
// after; "new item" or "changes item <id>"; local times; the claim id and the revision only
// with d; the key line at the bottom for each kind. Golden screens.
func TestPaneViewGolden(t *testing.T) {
	cases := []struct {
		name       string
		proposal   func(t *testing.T, f *fixture) int64
		keys       string
		rows, cols int
		bottom     string // the key line that ends the screen
	}{
		{name: "new-item", proposal: func(t *testing.T, f *fixture) int64 { return f.remember(t, "use pnpm") }, keys: "q", bottom: itemKeys},
		{name: "new-item-detail", proposal: func(t *testing.T, f *fixture) int64 { return f.remember(t, "use pnpm") }, keys: "dq", bottom: itemKeys},
		{name: "change", proposal: func(t *testing.T, f *fixture) int64 {
			id := f.approved(t, "use pnpm")
			f.mustReview(t, "--item", fmt.Sprint(id), "--edit", "use pnpm 9")
			r := f.rows(t)
			return r.proposals[len(r.proposals)-1].ID
		}, keys: "q", bottom: itemKeys},
		{name: "dispute", proposal: func(t *testing.T, f *fixture) int64 {
			_, git := f.gitRepo(t, "config/load.go")
			f.approved(t, loaderText)
			git("mv", "config/load.go", "config/loader.go")
			f.stale(t)
			return f.pendingDispute(t).ID
		}, keys: "q", bottom: disputeKeys},
		{name: "skill", proposal: func(t *testing.T, f *fixture) int64 { return f.skillDraft(t, skillText("deploy", "one")) }, keys: "q", bottom: itemKeys},
		// The suggestions of a command chain (E17-S8): the script and the tool draft.
		{name: "script", proposal: func(t *testing.T, f *fixture) int64 { s, _ := f.chainProposals(t); return s }, keys: "q",
			bottom: "a approve  r reject  l later  d detail  q close\n"},
		{name: "tool-draft", proposal: func(t *testing.T, f *fixture) int64 { _, d := f.chainProposals(t); return d }, keys: "q", bottom: itemKeys},
		// A pane of 16 rows cuts the skill view and keeps the keys at the bottom.
		{name: "skill-short-pane", proposal: func(t *testing.T, f *fixture) int64 { return f.skillDraft(t, skillText("deploy", "one")) },
			keys: "q", rows: 16, cols: 80, bottom: itemKeys},
		// A narrow pane counts the wrapped rows; d shows the detail above the long text.
		{name: "skill-narrow-pane-detail", proposal: func(t *testing.T, f *fixture) int64 {
			return f.skillDraft(t, skillText("deploy", "one"))
		}, keys: "dq", rows: 16, cols: 40, bottom: itemKeys},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inZone(t)
			f := newFixture(t)
			screen := f.paneScreen(t, c.proposal(t, f), c.keys, c.rows, c.cols)
			if !strings.HasSuffix(screen, "\n"+c.bottom) {
				t.Errorf("screen =\n%s\nwant it to end with the keys %q", screen, c.bottom)
			}
			last := strings.TrimPrefix(screen[strings.LastIndex(screen, clearScreen):], clearScreen)
			if n := screenRows(last, c.cols); c.rows > 0 && n > c.rows {
				t.Errorf("screen takes %d rows, want at most %d:\n%s", n, c.rows, last)
			}
			f.paneGolden(t, c.name, screen)
		})
	}
}

// screenRows is the rows that screen takes in a terminal of cols columns (0 for no wrap): each
// line takes one row for each cols runes, and the cursor row after the last newline counts.
func screenRows(screen string, cols int) int {
	n := 0
	for _, l := range strings.Split(screen, "\n") {
		if cols <= 0 {
			n++
			continue
		}
		n += max(1, (utf8.RuneCountInString(l)+cols-1)/cols)
	}
	return n
}

// AC #254: the text you type stays on one row of a sized pane, so the keys stay on the screen;
// a long text shows its end.
func TestPaneEntryRow(t *testing.T) {
	f := newFixture(t)
	id := f.remember(t, "use pnpm")
	var out bytes.Buffer
	text := strings.Repeat("x", 60) + "yz"
	p := &reviewPane{Env: f.env(f.sock, "w14:p1", "w14", &out), id: id, keys: typed("e" + text + "\x1b" + "q"),
		size: func() (int, int) { return 16, 40 }}
	if err := p.run(context.Background()); err != nil {
		t.Fatalf("pane = %v", err)
	}
	screen := out.String()
	const prompt = "new text (Enter approves, Esc goes back): "
	_, last, _ := strings.Cut(screen[strings.LastIndex(screen, "\x1b8\x1b[J"+prompt):], prompt)
	last, _, _ = strings.Cut(last, clearScreen)
	if last != "" {
		// The prompt is 42 runes: wider than the pane, so no text fits after it.
		t.Errorf("entry after the prompt = %q, want none in a 40-column pane", last)
	}
	p = &reviewPane{size: func() (int, int) { return 16, 80 }}
	if got := entry([]rune(text), len(prompt), p); got != "..."+text[len(text)-34:] || len(prompt)+utf8.RuneCountInString(got) >= 80 {
		t.Errorf("entry = %q, want the end of the text on one row", got)
	}
	if got := entry([]rune("short"), len(prompt), p); got != "short" {
		t.Errorf("entry = %q, want short", got)
	}
}

// chainProposals makes the script proposal and the tool draft of one command chain, and returns
// their ids.
func (f *fixture) chainProposals(t *testing.T) (script, draft int64) {
	t.Helper()
	for i := range 3 {
		f.chainSession(t, "ws_a14", fmt.Sprintf("s%d", i))
	}
	f.extractChains(t)
	s, d := scriptAndTool(t, f.rows(t))
	return s.ID, d.ID
}

// AC #254: the times are local: the remember at 12:00 UTC shows 17:30 at +05:30.
func TestReviewShowLocalTime(t *testing.T) {
	inZone(t)
	f := newFixture(t)
	id := f.remember(t, "use pnpm")
	if out := f.mustReview(t, fmt.Sprint(id)); !strings.Contains(out, "source:   remember, sender unconfirmed, 2026-09-28 17:30\n") {
		t.Errorf("show =\n%s\nwant the local time 2026-09-28 17:30", out)
	}
}

// AC #254: brain review <id> --detail adds the claim id and the revision; --detail goes with a
// proposal id alone.
func TestReviewDetail(t *testing.T) {
	f := newFixture(t)
	id := f.remember(t, "use pnpm")
	out := f.mustReview(t, fmt.Sprint(id), "--detail")
	for _, want := range []string{"proposal 1 (pending): new item\n", "\nclaim:    cl_",
		"\nrevision: item 2 revision 2, from revision 1 (now 1)\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("show --detail =\n%s\nwant %q", out, want)
		}
	}
	for _, args := range [][]string{{"--detail"}, {fmt.Sprint(id), "--detail", "--approve"}, {fmt.Sprint(id), "--detail", "--later"},
		{"--all", "--detail"}} {
		if _, err := f.review(t, args...); err == nil || !strings.Contains(err.Error(), "--detail goes with a proposal id alone") {
			t.Errorf("review %q = %v, want the --detail error", args, err)
		}
	}
}
