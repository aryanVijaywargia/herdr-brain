package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/hook"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// skillText is a skill text with name and a marker in its steps.
func skillText(name, marker string) string {
	return "---\nname: " + name + "\ndescription: Do the " + name + " task.\nrequired-tools: none\n" +
		"example: \"Do it.\"\n---\n\n# Steps\n\n1. Say " + marker + ".\n"
}

// skillDraft saves a draft skill in workspace w14 (ws_a14) with one pending proposal, as
// E16-S4 will, and returns the proposal id.
func (f *fixture) skillDraft(t *testing.T, text string) int64 {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	key := "ws_a14"
	pid, err := saveDraft(context.Background(), s, draft{text: text, kind: store.KindSkill, scope: store.ScopeWorkspace,
		workspaceKey: &key, target: "workspace w14 (shop)"}, testNow)
	if err != nil {
		t.Fatalf("saveDraft() = %v", err)
	}
	return pid
}

// approvedSkill drafts text and approves it; it returns the item id.
func (f *fixture) approvedSkill(t *testing.T, text string) int64 {
	t.Helper()
	p := f.skillDraft(t, text)
	f.mustReview(t, fmt.Sprint(p), "--approve")
	return *f.rows(t).proposal(t, p).ItemID
}

// changeSkill proposes args (--edit or --retire) for item id and approves the proposal.
func (f *fixture) changeSkill(t *testing.T, id int64, args ...string) {
	t.Helper()
	f.mustReview(t, append([]string{"--item", fmt.Sprint(id)}, args...)...)
	r := f.rows(t)
	f.mustReview(t, fmt.Sprint(r.proposals[len(r.proposals)-1].ID), "--approve")
}

func (f *fixture) skillFile(id, rev int64) string {
	return filepath.Join(f.store, "skills", fmt.Sprint(id), fmt.Sprintf("r%d", rev), "SKILL.md")
}

// fileTree lists every file under dir with its bytes; a missing dir holds none.
func fileTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		files[path] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// AC: two skill drafts with the same name in one scope: the second approval refuses and asks
// for a new name, and no file changes. Another name passes.
func TestApproveSkillNameClash(t *testing.T) {
	f := newFixture(t)
	first := f.skillDraft(t, skillText("deploy", "one"))
	second := f.skillDraft(t, skillText("deploy", "two"))
	f.mustReview(t, fmt.Sprint(first), "--approve")
	before, newest := fileTree(t, filepath.Dir(f.store)), f.rows(t).newest

	_, err := f.review(t, fmt.Sprint(second), "--approve")
	if !errors.Is(err, store.ErrSkillName) || !strings.Contains(err.Error(), "new name") {
		t.Fatalf("Review(second --approve) = %v, want ErrSkillName that asks for a new name", err)
	}
	if after := fileTree(t, filepath.Dir(f.store)); !mapsEqualExceptDB(before, after) {
		t.Error("files changed after the refused approval")
	}
	r := f.rows(t)
	if p := r.proposal(t, second); p.Status != store.ProposalPending || r.newest != newest {
		t.Errorf("after the refusal: proposal %s, newest generation %d, want pending and %d", p.Status, r.newest, newest)
	}

	f.mustReview(t, fmt.Sprint(second), "--approve", "--edit", skillText("deploy-docs", "two"))
	if b := f.brief(t); !strings.Contains(b, "- deploy: ") || !strings.Contains(b, "- deploy-docs: ") {
		t.Errorf("brief = %q, want both skills", b)
	}
}

// mapsEqualExceptDB compares two file trees, leaving out the store database files, whose
// bytes a refused transaction can still touch (the WAL).
func mapsEqualExceptDB(a, b map[string]string) bool {
	keep := func(m map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			if !strings.Contains(filepath.Base(k), ".db") {
				out[k] = v
			}
		}
		return out
	}
	x, y := keep(a), keep(b)
	if len(x) != len(y) {
		return false
	}
	for k, v := range x {
		if y[k] != v {
			return false
		}
	}
	return true
}

// AC: forget of a skill with two revisions removes all its files and reports done.
func TestForgetSkill(t *testing.T) {
	f := newFixture(t)
	id := f.approvedSkill(t, skillText("deploy", "one"))
	f.changeSkill(t, id, "--edit", skillText("deploy", "two"))
	r2, r3 := f.skillFile(id, 2), f.skillFile(id, 3)
	for _, p := range []string{r2, r3} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("revision file: %v", err)
		}
	}
	out := f.mustForget(t, clock(), fmt.Sprint(id))
	if !strings.HasSuffix(out, fmt.Sprintf("forget item %d: done\n", id)) {
		t.Errorf("Forget() printed %q, want done", out)
	}
	if files := fileTree(t, filepath.Join(f.store, "skills", fmt.Sprint(id))); len(files) != 0 {
		t.Errorf("skill files after forget: %v, want none", files)
	}
}

// AC: a kept conflict file keeps forget of a skill pending (S11, §7.5 step 8); brain repair
// --confirm then lets forget report done.
func TestForgetSkillPendingOnConflict(t *testing.T) {
	f := newFixture(t)
	id := f.approvedSkill(t, skillText("deploy", "one"))
	f.changeSkill(t, id, "--edit", skillText("deploy", "two"))
	path := f.skillFile(id, 3)
	if err := os.WriteFile(path, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := clock()
	out := f.mustForget(t, now, fmt.Sprint(id))
	if !strings.Contains(out, "pending") || !strings.Contains(out, path) || strings.Contains(out, "done") {
		t.Errorf("Forget() printed %q, want pending with the conflict file", out)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "mine" {
		t.Errorf("conflict file = %q, %v, want it kept", b, err)
	}

	var buf bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &buf)
	e.Now = now
	if err := Repair(context.Background(), e, []string{"--confirm"}); err != nil {
		t.Fatalf("Repair(--confirm) = %v", err)
	}
	if out := f.mustForget(t, now, fmt.Sprint(id)); !strings.HasSuffix(out, fmt.Sprintf("forget item %d: done\n", id)) {
		t.Errorf("second Forget() printed %q, want done", out)
	}
}

// AC S11 (skill part): a hand-edited listed SKILL.md stays, status and brain repair show the
// conflict, and brain repair --confirm writes it again.
func TestSkillConflictRepair(t *testing.T) {
	f := newStatusFixture(t)
	id := f.approvedSkill(t, skillText("deploy", "one"))
	path := f.skillFile(id, 2)
	if err := os.WriteFile(path, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := f.mustStatus(t); !strings.Contains(out, "output conflicts: 1") || !strings.Contains(out, path) {
		t.Errorf("Status() = %q, want the skill conflict", out)
	}
	var buf bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &buf)
	if err := Repair(context.Background(), e, nil); err != nil {
		t.Fatalf("Repair() = %v", err)
	}
	if !strings.Contains(buf.String(), "conflict: ") || !strings.Contains(buf.String(), path) {
		t.Errorf("Repair() printed %q, want the skill conflict", buf.String())
	}
	if b, _ := os.ReadFile(path); string(b) != "mine" {
		t.Errorf("file after brain repair = %q, want your edit kept", b)
	}
	buf.Reset()
	if err := Repair(context.Background(), e, []string{"--confirm"}); err != nil {
		t.Fatalf("Repair(--confirm) = %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != skillText("deploy", "one") {
		t.Errorf("file after --confirm = %q, %v, want the approved text", b, err)
	}
	if !strings.Contains(buf.String(), "skill file "+path+": written again") {
		t.Errorf("Repair(--confirm) printed %q, want the file written again", buf.String())
	}
}

// AC S61 (skill part), in a test home: after build, approve, retire and forget, Brain wrote no
// file into ~/.claude/skills, ~/.codex or the repo; its skill files stay in the store.
func TestSkillFilesStayInStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	f := newFixture(t)
	repo := filepath.Join(filepath.Dir(f.store), "proj")
	id := f.approvedSkill(t, skillText("deploy", "one"))
	if b := f.brief(t); !strings.Contains(b, f.skillFile(id, 2)) {
		t.Fatalf("brief = %q, want the store path", b)
	}
	f.changeSkill(t, id, "--retire")
	f.mustForget(t, clock(), fmt.Sprint(id))
	for _, dir := range []string{filepath.Join(home, ".claude", "skills"), filepath.Join(home, ".codex"), repo} {
		if files := fileTree(t, dir); len(files) != 0 {
			t.Errorf("files in %s: %v, want none", dir, slices.Sorted(maps.Keys(files)))
		}
	}
}

// hookStart runs the claude start hook in pane w14:p1 for agent session session and returns
// its receipt.
func (f *fixture) hookStart(t *testing.T, session string) store.Delivery {
	t.Helper()
	p, err := adapter.Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	e := f.env(f.sock, "w14:p1", "w14", nil)
	// The hook's lookup deadline counts from Start, so the hook uses the real clock.
	h := hook.Hook{Profile: p, Environ: e.Environ, Bindings: f.file, Pane: e.Pane, Start: time.Now(), Now: time.Now}
	var out bytes.Buffer
	if err := h.Run(context.Background(), strings.NewReader(`{"session_id":"`+session+`","cwd":"/tmp"}`), &out); err != nil {
		t.Fatalf("hook Run() = %v", err)
	}
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var d store.Delivery
	write(t, s, func(tx *sql.Tx) error {
		var ok bool
		var err error
		d, ok, err = store.LatestReceipt(context.Background(), tx, "default", "claude", session)
		if err == nil && !ok {
			err = fmt.Errorf("no receipt of %s", session)
		}
		return err
	})
	return d
}

// AC §5.5, E7-S2: a session that got revision 2 of a skill can still read that file after the
// next revision, and its receipt names revision 2; a new session gets and names revision 3.
func TestSkillReceiptNamesRevision(t *testing.T) {
	f := newFixture(t)
	id := f.approvedSkill(t, skillText("deploy", "one"))
	first := f.hookStart(t, "s1")
	f.changeSkill(t, id, "--edit", skillText("deploy", "two"))
	second := f.hookStart(t, "s2")

	if !slices.Contains(first.ItemRevisions, store.RevisionRef{Item: id, Revision: 2}) {
		t.Errorf("first receipt pairs = %v, want item %d revision 2", first.ItemRevisions, id)
	}
	if b, err := os.ReadFile(f.skillFile(id, 2)); err != nil || string(b) != skillText("deploy", "one") {
		t.Errorf("revision 2 file after revision 3 = %q, %v, want its text", b, err)
	}
	if !slices.Contains(second.ItemRevisions, store.RevisionRef{Item: id, Revision: 3}) {
		t.Errorf("second receipt pairs = %v, want item %d revision 3", second.ItemRevisions, id)
	}
}
