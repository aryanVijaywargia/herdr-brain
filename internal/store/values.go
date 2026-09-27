package store

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"
)

// timeLayout is UTC RFC 3339 with a fixed three-digit fraction, so text order is time order.
// [NEW] the millisecond precision.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// timeArg is the column value of t. The zero time is NULL.
func timeArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(timeLayout)
}

// timeDest scans a time column into t. NULL gives the zero time.
type timeDest struct{ t *time.Time }

func (d timeDest) Scan(v any) error {
	switch v := v.(type) {
	case nil:
		*d.t = time.Time{}
		return nil
	case string:
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return fmt.Errorf("parse time: %w", err)
		}
		*d.t = t
		return nil
	}
	return fmt.Errorf("scan time: got %T, want text", v)
}

// jsonList is a list column: a JSON array in one TEXT column. A nil list is stored as [].
type jsonList[T any] []T

func (l jsonList[T]) Value() (driver.Value, error) {
	if l == nil {
		l = jsonList[T]{}
	}
	b, err := json.Marshal([]T(l))
	if err != nil {
		return nil, fmt.Errorf("encode list: %w", err)
	}
	return string(b), nil
}

// listDest scans a list column into p.
type listDest[T any] struct{ p *[]T }

func (d listDest[T]) Scan(v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("scan list: got %T, want text", v)
	}
	if err := json.Unmarshal([]byte(s), d.p); err != nil {
		return fmt.Errorf("decode list: %w", err)
	}
	return nil
}
