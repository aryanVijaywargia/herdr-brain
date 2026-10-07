package store

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"
)

// AC E20-S2 #266 3: a recall hangs on the newest receipt of the pane in its binding, and on a
// receipt of the agent session that herdr shows, when it shows one.
func TestRecallReceipt(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	write(t, s, func(tx *sql.Tx) error {
		var ids []int64
		for _, d := range []Delivery{receipt("default", "w1:p1", "a", 1, testNow), receipt("default", "w1:p1", "b", 1, testNow),
			receipt("default", "w1:p2", "a", 1, testNow), receipt("work", "w1:p1", "a", 1, testNow)} {
			id, err := InsertDelivery(ctx, tx, d)
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		for _, c := range []struct {
			pane, agent string
			want        int64 // 0 for none
		}{{"w1:p1", "", ids[1]}, {"w1:p1", "a", ids[0]}, {"w1:p1", "c", 0}, {"w1:p9", "", 0}} {
			id, ok, err := RecallReceipt(ctx, tx, "default", c.pane, c.agent)
			if err != nil || ok != (c.want != 0) || id != c.want {
				t.Errorf("RecallReceipt(%s, %q) = %d, %v, %v, want %d", c.pane, c.agent, id, ok, err, c.want)
			}
		}
		return nil
	})
}

// AC E20-S2 #266 3: the tied receipt of a session names the revisions that a recall on it
// returned up to the observation, in front of its own, the newest recall first. A recall after
// the observation does not count.
func TestTiedReceiptRecalls(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	write(t, s, func(tx *sql.Tx) error {
		obs, err := observe(ctx, tx, SessionKey{HerdrSession: "default", Harness: "claude", Kind: KeyAgent, ID: "a"}, "w1:p1",
			testNow, testNow.Add(10*time.Minute))
		if err != nil {
			return err
		}
		d, err := InsertDelivery(ctx, tx, receipt("default", "w1:p1", "a", 1, testNow.Add(time.Minute)))
		if err != nil {
			return err
		}
		for _, r := range []Recall{
			{DeliveryID: d, ItemRevisions: []RevisionRef{{Item: 2, Revision: 1}}, At: testNow.Add(2 * time.Minute)},
			{DeliveryID: d, ItemRevisions: []RevisionRef{{Item: 1, Revision: 2}}, At: testNow.Add(5 * time.Minute)},
			{DeliveryID: d, ItemRevisions: []RevisionRef{{Item: 3, Revision: 1}}, At: testNow.Add(20 * time.Minute)},
		} {
			if _, err := InsertRecall(ctx, tx, r); err != nil {
				return err
			}
		}
		got, ok, err := TiedReceipt(ctx, tx, obs)
		want := []RevisionRef{{Item: 1, Revision: 2}, {Item: 2, Revision: 1}, {Item: 1, Revision: 1}}
		if err != nil || !ok || got.ID != d || !slices.Equal(got.ItemRevisions, want) {
			t.Errorf("TiedReceipt() = %+v, %v, %v, want receipt %d with revisions %v", got, ok, err, d, want)
		}
		return nil
	})
}
