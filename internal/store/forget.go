package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ForgetPending reports whether an output generation still holds the text of an item with an
// open forget. The forget flow marks such a generation with outputs.holds_forgotten_text, and
// forget is done only when no generation holds the text (§6.7). Until then the hook withholds.
func ForgetPending(ctx context.Context, tx *sql.Tx) (bool, error) {
	var pending bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM outputs WHERE holds_forgotten_text = 1)`).
		Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("read forget state: %w", err)
	}
	return pending, nil
}
