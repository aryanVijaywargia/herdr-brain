package store

import (
	"fmt"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"
)

// unlistedTextFields returns the TEXT columns of the live schema that are in neither
// ItemTextFields nor noItemTextFields.
func unlistedTextFields(t *testing.T, s *Store) []Field {
	t.Helper()
	listed := append(ItemTextFields(), noItemTextFields()...)
	var unlisted []Field
	for _, table := range tableNames(t, s) {
		for _, c := range columns(t, s, table) {
			f := Field{table, c.name}
			if c.typ == "TEXT" && !slices.Contains(listed, f) {
				unlisted = append(unlisted, f)
			}
		}
	}
	return unlisted
}

// AC: the schema test passes on the M1 schema. Every TEXT column is in one of the two lists.
func TestEveryTextColumnIsListed(t *testing.T) {
	s := openM1(t)
	for _, f := range unlistedTextFields(t, s) {
		t.Errorf("%s.%s is TEXT and in neither ItemTextFields nor noItemTextFields", f.Table, f.Column)
	}
}

// Each listed field is a TEXT column of the live schema, and in one list only, so forget
// does not query a column that is gone.
func TestListedFieldsExist(t *testing.T) {
	s := openM1(t)
	text := map[Field]bool{}
	for _, table := range tableNames(t, s) {
		for _, c := range columns(t, s, table) {
			text[Field{table, c.name}] = c.typ == "TEXT"
		}
	}
	seen := map[Field]bool{}
	for _, f := range append(ItemTextFields(), noItemTextFields()...) {
		if !text[f] {
			t.Errorf("%s.%s is listed but is not a TEXT column", f.Table, f.Column)
		}
		if seen[f] {
			t.Errorf("%s.%s is listed twice", f.Table, f.Column)
		}
		seen[f] = true
	}
}

// AC: the schema test fails when a test migration adds an unlisted text column, and it
// names that column.
func TestUnlistedTextColumnFails(t *testing.T) {
	fsys := fstest.MapFS{}
	ms, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range ms {
		b, err := fs.ReadFile(migrations, name)
		if err != nil {
			t.Fatal(err)
		}
		fsys[name] = &fstest.MapFile{Data: b}
	}
	next := len(ms) + 1
	fsys[fmt.Sprintf("migrations/%04d_notes.sql", next)] = &fstest.MapFile{Data: []byte(
		"CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL) STRICT;" +
			"ALTER TABLE items ADD COLUMN summary TEXT;")}
	s := openTest(t, t.TempDir(), fsys)
	defer closeTest(t, s)

	want := []Field{{"items", "summary"}, {"notes", "body"}}
	if got := unlistedTextFields(t, s); !slices.Equal(got, want) {
		t.Errorf("unlisted = %v, want %v", got, want)
	}
}
