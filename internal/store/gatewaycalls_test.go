package store

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"
)

// A gateway call row keeps its method, its result and its time; the table takes only the four
// gateway tools and the three results.
func TestGatewayCalls(t *testing.T) {
	s := openM1(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	want := []GatewayCall{
		{ID: 1, Method: "find_tool", Result: GatewayOK, At: at},
		{ID: 2, Method: "run_tool", Result: GatewayRefused, At: at.Add(time.Second)},
		{ID: 3, Method: "remember", Result: GatewayFailed, At: at.Add(2 * time.Second)},
	}
	write(t, s, func(tx *sql.Tx) error {
		for _, c := range want {
			if _, err := AddGatewayCall(ctx, tx, c); err != nil {
				return err
			}
		}
		got, err := GatewayCalls(ctx, tx)
		if err != nil {
			return err
		}
		if !slices.EqualFunc(got, want, func(a, b GatewayCall) bool {
			return a.ID == b.ID && a.Method == b.Method && a.Result == b.Result && a.At.Equal(b.At)
		}) {
			t.Errorf("GatewayCalls() = %+v, want %+v", got, want)
		}
		return nil
	})
	for _, c := range []GatewayCall{
		{Method: "tools/list", Result: GatewayOK, At: at},
		{Method: "recall", Result: "ran", At: at},
	} {
		err := s.Write(ctx, func(tx *sql.Tx) error {
			_, err := AddGatewayCall(ctx, tx, c)
			return err
		})
		if err == nil {
			t.Errorf("AddGatewayCall(%+v) = nil error, want a CHECK error", c)
		}
	}
}
