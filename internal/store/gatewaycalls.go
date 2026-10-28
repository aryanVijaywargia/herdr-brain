package store

import (
	"context"
	"database/sql"
	"time"
)

// GatewayResult is the result of a call of the MCP gateway.
type GatewayResult string

// The gateway call results.
const (
	GatewayOK      GatewayResult = "ok"
	GatewayRefused GatewayResult = "refused" // a check refused the call, so nothing ran
	GatewayFailed  GatewayResult = "failed"  // the call passed the checks but did not end well
)

// GatewayCall is one row of gateway_calls: the gateway tool that was called, the result and
// the time.
type GatewayCall struct {
	ID     int64
	Method string
	Result GatewayResult
	At     time.Time
}

// AddGatewayCall adds c and returns its id.
func AddGatewayCall(ctx context.Context, tx *sql.Tx, c GatewayCall) (int64, error) {
	return insertRow(ctx, tx, "gateway call", `INSERT INTO gateway_calls (method, result, at) VALUES (?, ?, ?)`,
		c.Method, c.Result, timeArg(c.At))
}

// GatewayCalls returns every gateway call, in id order.
func GatewayCalls(ctx context.Context, tx *sql.Tx) ([]GatewayCall, error) {
	return query(ctx, tx, `SELECT id, method, result, at FROM gateway_calls ORDER BY id`,
		func(rows *sql.Rows) (GatewayCall, error) {
			var c GatewayCall
			err := rows.Scan(&c.ID, &c.Method, &c.Result, timeDest{&c.At})
			return c, err
		})
}
