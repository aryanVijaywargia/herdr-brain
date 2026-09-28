package store

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"
)

func TestReceipts(t *testing.T) {
	_, s := newTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	str := func(v string) *string { return &v }
	ds := []Delivery{
		{Profile: "claude", HerdrSession: str("default"), PaneID: "w1:p1", AgentSessionID: str("a"), Result: ResultWithheld, WithheldReason: str("not a member"), At: at},
		{Profile: "claude", HerdrSession: str("default"), PaneID: "w1:p1", AgentSessionID: str("a"), Result: ResultEmitted, At: at},
		{Profile: "claude", HerdrSession: str("other"), PaneID: "w1:p1", AgentSessionID: str("b"), Result: ResultEmitted, At: at},
		{Profile: "codex", HerdrSession: str("default"), PaneID: "w1:p2", AgentSessionID: str("b"), Result: ResultUnmapped, At: at},
		{Profile: "claude", HerdrSession: str("default"), PaneID: "w1:p3", Result: ResultWithheld, WithheldReason: str("pane not live"), At: at},
		{Profile: "claude", HerdrSession: str("default"), PaneID: "w1:p4", Result: ResultWithheld, WithheldReason: str("not a member"), At: at},
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		for _, d := range ds {
			if _, err := InsertDelivery(ctx, tx, d); err != nil {
				return err
			}
		}
		// The newest receipt of the agent session wins.
		if d, ok, err := LatestReceipt(ctx, tx, "default", "claude", "a"); err != nil || !ok || d.Result != ResultEmitted {
			t.Errorf("LatestReceipt(a) = %v, %v, %v, want emitted", d.Result, ok, err)
		}
		// Agent session b has receipts in another herdr session and of another harness only.
		if _, ok, err := LatestReceipt(ctx, tx, "default", "claude", "b"); err != nil || ok {
			t.Errorf("LatestReceipt(b) = %v, %v, want none", ok, err)
		}
		cs, err := WithheldCounts(ctx, tx, "default", "claude")
		want := []WithheldCount{{"not a member", 2}, {"pane not live", 1}}
		if err != nil || !slices.Equal(cs, want) {
			t.Errorf("WithheldCounts() = %v, %v, want %v", cs, err, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
