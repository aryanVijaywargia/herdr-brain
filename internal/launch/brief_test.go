package launch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// The Claude session that the user's args name for the brief (review of #444): an explicit
// ID only. A fork gets a new ID, and --continue or --resume with no ID name none.
func TestArgsSession(t *testing.T) {
	d := mustLoad(t, "claude")
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"hi"}, ""},
		{[]string{"--resume", "abc"}, "abc"},
		{[]string{"-r", "abc", "hi"}, "abc"},
		{[]string{"--resume=abc"}, "abc"},
		{[]string{"--resume"}, ""},
		{[]string{"--continue"}, ""},
		{[]string{"--resume", "abc", "--fork-session"}, ""},
		{[]string{"--resume", "abc", "--fork-session", "--session-id", "new"}, "new"},
		{[]string{"--session-id", "s1"}, "s1"},
		{[]string{"--resume", "a", "--resume", "b"}, "b"},
	} {
		if got := parse(d, tt.args).session(); got != tt.want {
			t.Errorf("parse(%q).session() = %q, want %q", tt.args, got, tt.want)
		}
	}
}

// keepBrief writes a launch folder of launch record id in the store folder dir with the brief
// file text and the receipt of brief: it names the revisions revs.
func keepBrief(t *testing.T, dir string, id int64, text, brief string, revs ...store.RevisionRef) string {
	t.Helper()
	folder := paths.Launch(dir, id)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(brief))
	h := hex.EncodeToString(sum[:])
	data, err := json.Marshal(store.Delivery{BriefHash: &h, ItemRevisions: revs, Result: store.ResultEmitted})
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string]string{StartFile: string(data), briefFile: text} {
		if err := os.WriteFile(filepath.Join(folder, name), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return folder
}

func readBrief(t *testing.T, folder string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(folder, briefFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Review of #444: forget cuts the Brain brief out of the brief.md of each launch whose receipt
// names the item, and keeps the user's own text before it. A brief of other items, and a cut
// brief, stay as they are. BriefsHold reports a brief that still holds the item.
func TestForgetBriefs(t *testing.T) {
	dir := t.TempDir()
	brief := "<brain>\nitem 1 text\n\nmore\n</brain>"
	alone := keepBrief(t, dir, 1, brief, brief, store.RevisionRef{Item: 1, Revision: 1})
	merged := keepBrief(t, dir, 2, "USER\n\nline\n\n"+brief, brief, store.RevisionRef{Item: 1, Revision: 1})
	other := keepBrief(t, dir, 3, brief, brief, store.RevisionRef{Item: 2, Revision: 1})
	if held, err := BriefsHold(dir, 1); err != nil || !held {
		t.Fatalf("BriefsHold(1) = %t, %v, want true", held, err)
	}
	for range 2 {
		if err := ForgetBriefs(dir, 1); err != nil {
			t.Fatal(err)
		}
	}
	for folder, want := range map[string]string{alone: "", merged: "USER\n\nline", other: brief} {
		if got := readBrief(t, folder); got != want {
			t.Errorf("brief.md of %s = %q, want %q", filepath.Base(folder), got, want)
		}
	}
	if held, err := BriefsHold(dir, 1); err != nil || held {
		t.Errorf("BriefsHold(1) after ForgetBriefs = %t, %v, want false", held, err)
	}
	if err := ForgetBriefs(t.TempDir(), 1); err != nil {
		t.Errorf("ForgetBriefs of a store with no launches = %v", err)
	}
}

// Review of #444: a forget that ran between the compile of the brief and the write of brief.md
// erased the text of a revision of the brief: the launch cuts the brief itself. A brief whose
// items keep their text stays.
func TestCutForgotten(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, dir, busyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test
	var ids [2]int64
	err = st.Write(ctx, func(tx *sql.Tx) error {
		for i, text := range []string{"kept item", "forgotten item"} {
			r := store.ItemRevision{Text: text, Scope: store.ScopeGlobal, State: store.StateActive, Author: store.AuthorYou,
				Origin: store.OriginRemember, Created: time.Now()}
			id, _, err := store.CreateItem(ctx, tx, store.Item{ClaimID: text, Kind: store.KindFact, Created: time.Now()}, r)
			if err != nil {
				return err
			}
			ids[i] = id
		}
		return store.EraseItem(ctx, tx, ids[1], time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	brief := "<brain>forgotten item</brain>"
	kept := keepBrief(t, dir, 1, brief, brief, store.RevisionRef{Item: ids[0], Revision: 1})
	cut := keepBrief(t, dir, 2, "USER\n\n"+brief, brief, store.RevisionRef{Item: ids[0], Revision: 1}, store.RevisionRef{Item: ids[1], Revision: 1})
	for _, folder := range []string{kept, cut} {
		if err := cutForgotten(ctx, dir, folder); err != nil {
			t.Fatal(err)
		}
	}
	if got := readBrief(t, kept); got != brief {
		t.Errorf("brief.md of a brief with no forgotten item = %q, want %q", got, brief)
	}
	if got := readBrief(t, cut); got != "USER" {
		t.Errorf("brief.md of a brief with a forgotten item = %q, want the user's text only", got)
	}
}
