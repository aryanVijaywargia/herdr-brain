package brained

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// revertNow is the clock of the revert tests.
var revertNow = time.Now().UTC().Truncate(time.Second)

// revertStore opens a store in a new folder with workspace ws_1 at root (nil for no root) and
// one ended codex session in it, with the user text reason when it is not "". It returns the
// store.
func revertStore(t *testing.T, root *string, reason string) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir(), busyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	key := store.SessionKey{HerdrSession: "s1", Harness: "codex", Kind: store.KeyAgent, ID: "a1"}
	start := revertNow.Add(-time.Hour)
	err = st.Write(ctx, func(tx *sql.Tx) error {
		if err := store.InsertWorkspace(ctx, tx, store.Workspace{Key: "ws_1", HerdrSession: "s1", HerdrWorkspaceID: "w1",
			Label: "notes-api", RootFolder: root, Status: store.WorkspaceLive, LastSeen: start}); err != nil {
			return err
		}
		sid, err := store.OpenSession(ctx, tx, key, "w1:p1", "ws_1", start)
		if err != nil {
			return err
		}
		seg, err := store.StartSegment(ctx, tx, sid, "w1:p1", "ws_1", "recent_unwrapped", start)
		if err != nil {
			return err
		}
		if reason != "" {
			if _, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: "reason", Source: store.SourceHerdr,
				SegmentID: seg, Event: "idle", Excerpt: reason, Status: store.ObservationComplete,
				Sender: store.SenderUnconfirmed, Counts: true, At: start.Add(time.Minute)}); err != nil {
				return err
			}
		}
		return store.EndSession(ctx, tx, key, start.Add(2*time.Minute))
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// gitObservations returns the git observations of st.
func gitObservations(t *testing.T, st *store.Store) []store.Observation {
	t.Helper()
	var obs []store.Observation
	err := st.Write(context.Background(), func(tx *sql.Tx) error {
		all, err := store.Observations(context.Background(), tx)
		for _, o := range all {
			if o.Source == store.SourceGit {
				obs = append(obs, o)
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return obs
}

// editFile writes content to name in dir.
func editFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// AC #283 1: a revert commit that is newer than the start of brained is a git observation in
// the segment of its time, and it counts once: the next pass adds nothing.
func TestRevertCommit(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	root := repo(t, "a.txt")
	editFile(t, root, "b.txt", "b")
	git(t, root, "add", "b.txt")
	git(t, root, "commit", "-q", "-m", "add b")
	bad := git(t, root, "rev-parse", "HEAD")
	git(t, root, "revert", "--no-edit", bad)
	st := revertStore(t, &root, "")

	if err := NewReverts(revertNow.Add(time.Hour)).Pass(ctx, st, "s1", true, revertNow); err != nil {
		t.Fatal(err)
	}
	if obs := gitObservations(t, st); len(obs) != 0 {
		t.Fatalf("a revert older than the start of brained: %+v, want none", obs)
	}
	r := NewReverts(revertNow.Add(-2 * time.Hour))
	for range 2 {
		if err := r.Pass(ctx, st, "s1", true, revertNow); err != nil {
			t.Fatal(err)
		}
	}
	obs := gitObservations(t, st)
	if len(obs) != 1 {
		t.Fatalf("git observations = %+v, want 1", obs)
	}
	o := obs[0]
	if want := `git revert: commit ` + git(t, root, "rev-parse", "--short=7", "HEAD") + ` "Revert \"add b\"" undoes commit ` +
		bad[:7] + "; files: b.txt"; o.Excerpt != want || o.Sender != store.SenderUnconfirmed || !o.Counts {
		t.Errorf("observation = %+v, want excerpt %q, unconfirmed, counts", o, want)
	}
}

// AC #283 2, 3 and 4, the S7 shape: codex changes src/notes.js, you put it back with git
// checkout and say why. The revert is a git observation, and it reaches the extractor beside
// your reason as evidence of the same correction: 2 occurrences, the correction gate (§7.2).
func TestRevertCheckoutS7(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	root := repo(t, "notes.js", "log.js")
	st := revertStore(t, &root, "don't cache the notes file, measure first")
	r := NewReverts(revertNow)

	editFile(t, root, "notes.js", "cache with a file metadata check")
	editFile(t, root, "log.js", "agent edit")
	if err := r.Pass(ctx, st, "s1", true, revertNow); err != nil {
		t.Fatal(err)
	}
	editFile(t, root, "log.js", "your edit after the agent") // a normal edit: not a revert
	if obs := gitObservations(t, st); len(obs) != 0 {
		t.Fatalf("after the agent change: %+v, want none", obs)
	}
	git(t, root, "checkout", "--", "notes.js")
	if err := r.Pass(ctx, st, "s1", true, revertNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := r.Pass(ctx, st, "s1", true, revertNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	obs := gitObservations(t, st)
	if len(obs) != 1 || !strings.Contains(obs[0].Excerpt, "notes.js went back") || strings.Contains(obs[0].Excerpt, "log.js") {
		t.Fatalf("git observations = %+v, want one revert of notes.js", obs)
	}

	err := st.Write(ctx, func(tx *sql.Tx) error {
		return store.SetExtractor(ctx, tx, store.ExtractorSetting{On: true, CLI: store.ExtractorClaude, Changed: revertNow})
	})
	if err != nil {
		t.Fatal(err)
	}
	cli := &revertCLI{}
	if err := (extract.Pass{Store: st, Runner: cli, HerdrSession: "s1", DefaultOn: true,
		Now: func() time.Time { return revertNow.Add(time.Hour) }}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cli.prompt, "git: the work-tree change of notes.js went back") ||
		!strings.Contains(cli.prompt, "don't cache the notes file") {
		t.Fatalf("extractor input = %q, want the revert and the reason", cli.prompt)
	}
	var cands []store.Candidate
	err = st.Write(ctx, func(tx *sql.Tx) (err error) {
		cands, err = store.Candidates(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Kind != store.CandidateCorrection || cands[0].Count != 2 || cands[0].Priority != 1 {
		t.Errorf("candidates = %+v, want one correction with count 2 at the gate", cands)
	}
}

// AC #283 3: a revert alone is one occurrence of a correction, below the gate of 2.
func TestRevertAloneBelowGate(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	root := repo(t, "notes.js")
	st := revertStore(t, &root, "")
	r := NewReverts(revertNow)
	editFile(t, root, "notes.js", "agent change")
	if err := r.Pass(ctx, st, "s1", true, revertNow); err != nil {
		t.Fatal(err)
	}
	git(t, root, "checkout", "--", "notes.js")
	if err := r.Pass(ctx, st, "s1", true, revertNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	err := st.Write(ctx, func(tx *sql.Tx) error {
		return store.SetExtractor(ctx, tx, store.ExtractorSetting{On: true, CLI: store.ExtractorClaude, Changed: revertNow})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := (extract.Pass{Store: st, Runner: &revertCLI{}, HerdrSession: "s1", DefaultOn: true,
		Now: func() time.Time { return revertNow.Add(time.Hour) }}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	var cands []store.Candidate
	err = st.Write(ctx, func(tx *sql.Tx) (err error) {
		cands, err = store.Candidates(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Kind != store.CandidateCorrection || cands[0].Count != 1 || cands[0].Priority != 0 {
		t.Errorf("candidates = %+v, want one correction with count 1 below the gate", cands)
	}
}

// AC #283 4: a workspace with no git, or with no root, gives nothing and no error; learning off
// stores nothing.
func TestRevertNoGit(t *testing.T) {
	gitEnv(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", os.TempDir())
	ctx := context.Background()
	plain := t.TempDir()
	for _, root := range []*string{&plain, nil} {
		st := revertStore(t, root, "")
		r := NewReverts(revertNow)
		for range 2 {
			if err := r.Pass(ctx, st, "s1", true, revertNow); err != nil {
				t.Fatalf("Pass(no git) = %v", err)
			}
		}
		if obs := gitObservations(t, st); len(obs) != 0 {
			t.Errorf("no git: %+v, want none", obs)
		}
	}

	root := repo(t, "notes.js")
	st := revertStore(t, &root, "")
	r := NewReverts(revertNow)
	editFile(t, root, "notes.js", "agent change")
	if err := r.Pass(ctx, st, "s1", false, revertNow); err != nil {
		t.Fatal(err)
	}
	git(t, root, "checkout", "--", "notes.js")
	if err := r.Pass(ctx, st, "s1", false, revertNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if obs := gitObservations(t, st); len(obs) != 0 {
		t.Errorf("learning off: %+v, want none", obs)
	}
}

// revertCLI is a fake claude extractor: one correction claim with every ref of the prompt.
type revertCLI struct{ prompt string }

var revertRef = regexp.MustCompile(`\[(e\d+)\]`)

func (c *revertCLI) Run(_ context.Context, cmd extract.Command) ([]byte, error) {
	c.prompt = strings.Join(cmd.Argv, " ")
	refs := []string{}
	for _, m := range revertRef.FindAllStringSubmatch(c.prompt, -1) {
		refs = append(refs, m[1])
	}
	return json.Marshal(map[string]any{"is_error": false, "structured_output": map[string]any{"claims": []any{
		map[string]any{"kind": "correction", "situation": "speeding up a read path", "rule": "measure before you add a cache",
			"scope_guess": "workspace", "evidence_refs": refs, "same_as": nil, "conflicts_with": nil,
			"touches_permission": false, "expected_benefit": "no cache that you revert"}}}})
}

// Sol review of #283: a file that goes back goes into the segment of the agent whose change it
// was, also when a newer agent started in the workspace since.
func TestRevertBackOwner(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	root := repo(t, "notes.js")
	st := revertStore(t, &root, "")
	r := NewReverts(revertNow)
	editFile(t, root, "notes.js", "agent change")
	if err := r.Pass(ctx, st, "s1", true, revertNow); err != nil {
		t.Fatal(err)
	}
	var newer int64
	err := st.Write(ctx, func(tx *sql.Tx) error {
		key := store.SessionKey{HerdrSession: "s1", Harness: "claude", Kind: store.KeyAgent, ID: "b1"}
		sid, err := store.OpenSession(ctx, tx, key, "w1:p2", "ws_1", revertNow)
		if err != nil {
			return err
		}
		newer, err = store.StartSegment(ctx, tx, sid, "w1:p2", "ws_1", "visible", revertNow)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	git(t, root, "checkout", "--", "notes.js")
	if err := r.Pass(ctx, st, "s1", true, revertNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	obs := gitObservations(t, st)
	if len(obs) != 1 || obs[0].SegmentID == newer {
		t.Errorf("git observations = %+v, want one in the segment of the first agent, not %d", obs, newer)
	}
}

// Sol review of #283: a change that is committed and then reverted with a revert commit
// between two passes is one revert, not two.
func TestRevertCommitThenRevert(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	root := repo(t, "notes.js")
	st := revertStore(t, &root, "")
	r := NewReverts(revertNow.Add(-2 * time.Hour))
	editFile(t, root, "notes.js", "agent change")
	if err := r.Pass(ctx, st, "s1", true, revertNow); err != nil {
		t.Fatal(err)
	}
	git(t, root, "commit", "-q", "-am", "cache")
	git(t, root, "revert", "--no-edit", "HEAD")
	if err := r.Pass(ctx, st, "s1", true, revertNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if obs := gitObservations(t, st); len(obs) != 1 || !strings.HasPrefix(obs[0].Excerpt, "git revert:") {
		t.Errorf("git observations = %+v, want the revert commit only", obs)
	}
}

// Sol review of #283: a revert commit made while learning is off never comes back, and the
// work tree starts from a baseline after learning is on again (§6.7).
func TestRevertLearningOffDropped(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	root := repo(t, "notes.js")
	st := revertStore(t, &root, "")
	r := NewReverts(revertNow.Add(-2 * time.Hour))
	editFile(t, root, "notes.js", "agent change")
	git(t, root, "commit", "-q", "-am", "cache")
	git(t, root, "revert", "--no-edit", "HEAD")
	editFile(t, root, "notes.js", "agent change again")
	if err := r.Pass(ctx, st, "s1", false, revertNow); err != nil {
		t.Fatal(err)
	}
	git(t, root, "checkout", "--", "notes.js")
	if err := r.Pass(ctx, st, "s1", true, revertNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if obs := gitObservations(t, st); len(obs) != 0 {
		t.Errorf("git observations = %+v, want none", obs)
	}
}

// Sol review of #283: git keeps whole seconds, so a commit in the start second of a segment
// belongs to it.
func TestSegmentAtSecond(t *testing.T) {
	start := revertNow.Add(300 * time.Millisecond)
	if _, ok := segmentAt([]store.Segment{{ID: 1, Started: start}}, revertNow); !ok {
		t.Error("segmentAt(commit in the start second) = none, want the segment")
	}
}
