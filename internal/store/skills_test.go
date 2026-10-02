package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func skillText(name string) string {
	return "---\nname: " + name + "\ndescription: Do the " + name + " task.\nrequired-tools: none\n" +
		"example: \"Do it.\"\n---\n\n# Steps\n\n1. Do it.\n"
}

func TestParseSkill(t *testing.T) {
	sk, err := ParseSkill("---\nname: release-notes\ndescription: Write the release notes.\nrequired-tools: git, gh\n" +
		"not-available: gh in codex\nexample: \"Notes for v1.4.0.\"\n---\n\n# Steps\n\n1. Read the tags.\n")
	if err != nil {
		t.Fatalf("ParseSkill() = %v", err)
	}
	want := Skill{Name: "release-notes", Description: "Write the release notes.", RequiredTools: "git, gh",
		NotAvailable: "gh in codex", Example: `"Notes for v1.4.0."`, Steps: "# Steps\n\n1. Read the tags."}
	if sk != want {
		t.Errorf("ParseSkill() = %+v, want %+v", sk, want)
	}

	for name, text := range map[string]string{
		"no header":       "name: a\n",
		"no header end":   "---\nname: a\n",
		"unknown key":     "---\nname: a\ndescription: d\nrequired-tools: none\nexample: e\nowner: me\n---\nsteps\n",
		"repeated key":    "---\nname: a\nname: b\ndescription: d\nrequired-tools: none\nexample: e\n---\nsteps\n",
		"missing example": "---\nname: a\ndescription: d\nrequired-tools: none\n---\nsteps\n",
		"bad name":        "---\nname: Release Notes\ndescription: d\nrequired-tools: none\nexample: e\n---\nsteps\n",
		"no steps":        "---\nname: a\ndescription: d\nrequired-tools: none\nexample: e\n---\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSkill(text); err == nil {
				t.Errorf("ParseSkill(%q) = nil, want an error", text)
			}
		})
	}
}

// One active skill for each name in each scope (§8.2): the same name in another scope passes.
func TestCheckSkill(t *testing.T) {
	s := openM1(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	k1, k2 := "k1", "k2"
	write(t, s, func(tx *sql.Tx) error {
		for _, k := range []string{k1, k2} {
			if err := InsertWorkspace(ctx, tx, Workspace{Key: k, HerdrSession: "default", HerdrWorkspaceID: k,
				Label: k, Status: WorkspaceLive, LastSeen: now}); err != nil {
				return err
			}
		}
		return nil
	})
	add := func(key *string, state State, text string) int64 {
		t.Helper()
		r := ItemRevision{Text: text, Scope: ScopeGlobal, WorkspaceKey: key, State: state, Author: AuthorYou,
			Origin: OriginReview, Created: now}
		if key != nil {
			r.Scope = ScopeWorkspace
		}
		var id int64
		write(t, s, func(tx *sql.Tx) (err error) {
			id, _, err = CreateItem(ctx, tx, Item{ClaimID: text, Kind: KindSkill, Created: now}, r)
			return err
		})
		return id
	}
	check := func(id int64) error {
		return s.Write(ctx, func(tx *sql.Tx) error { return CheckSkill(ctx, tx, id) })
	}
	first := add(&k1, StateActive, skillText("deploy"))
	if err := check(first); err != nil {
		t.Fatalf("CheckSkill(first) = %v", err)
	}
	clash := add(&k1, StateActive, skillText("deploy")+"\n")
	if err := check(clash); !errors.Is(err, ErrSkillName) {
		t.Errorf("CheckSkill(same name, same workspace) = %v, want ErrSkillName", err)
	}
	for name, id := range map[string]int64{
		"other workspace": add(&k2, StateActive, skillText("deploy")),
		"global":          add(nil, StateActive, skillText("deploy")),
		"draft":           add(&k1, StateDraft, skillText("deploy")),
		"other name":      add(&k1, StateActive, skillText("deploy-docs")),
	} {
		if err := check(id); err != nil {
			t.Errorf("CheckSkill(%s) = %v, want nil", name, err)
		}
	}
	bad := add(&k2, StateActive, "not a skill")
	if err := check(bad); err == nil || errors.Is(err, ErrSkillName) {
		t.Errorf("CheckSkill(bad text) = %v, want a parse error", err)
	}
}
