package store

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"
)

// The forget flow finds the audit rows of an item by the revision pairs on either side, at any
// revision, and never by the text.
func TestAppliedEditsWithItem(t *testing.T) {
	s := openM1(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	edits := []struct {
		before, after []RevisionRef
	}{
		{nil, []RevisionRef{{Item: 7, Revision: 1}}},
		{[]RevisionRef{{Item: 7, Revision: 1}}, []RevisionRef{{Item: 7, Revision: 1}, {Item: 8, Revision: 1}}},
		{[]RevisionRef{{Item: 8, Revision: 1}}, []RevisionRef{{Item: 8, Revision: 2}}},
		{[]RevisionRef{{Item: 7, Revision: 2}}, nil},
		{[]RevisionRef{{Item: 17, Revision: 1}}, []RevisionRef{{Item: 70, Revision: 7}}},
	}
	var ids []int64
	write(t, s, func(tx *sql.Tx) error {
		for i, e := range edits {
			id, err := InsertAppliedEdit(ctx, tx, AppliedEdit{File: "global.md", Generation: int64(i + 1),
				ContentHash: "h", Applied: now, RevisionsBefore: e.before, RevisionsAfter: e.after})
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})

	for _, c := range []struct {
		item int64
		want []int64
	}{
		{7, []int64{ids[0], ids[1], ids[3]}},
		{8, []int64{ids[1], ids[2]}},
		{9, nil},
	} {
		var got []int64
		write(t, s, func(tx *sql.Tx) (err error) {
			got, err = AppliedEditsWithItem(ctx, tx, c.item)
			return err
		})
		if !slices.Equal(got, c.want) {
			t.Errorf("AppliedEditsWithItem(%d) = %v, want %v", c.item, got, c.want)
		}
	}
}
