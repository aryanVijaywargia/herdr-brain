package store

import (
	"context"
	"database/sql"
	"slices"
)

// Around returns the stored observations of the segment of observation id that come up to n
// before it and up to n after it, in capture order (E20-S4, the evidence view). The text of a
// segment never joins another segment (§6.2 step 7), so the neighbours stay in it. It reads the
// store only: no transcript.
func Around(ctx context.Context, tx *sql.Tx, id int64, n int) (before, after []Observation, err error) {
	const near = `SELECT o.id, o.capture_identity, o.source, o.segment_id, o.event, o.excerpt, o.truncated, o.status,
		o.sender, o.counts, o.brain_origin, o.at, COALESCE(o.enriched_by, 0) FROM observations o
		JOIN observations x ON x.id = ?1 AND o.segment_id = x.segment_id `
	before, err = query(ctx, tx, near+`WHERE o.at < x.at OR o.at = x.at AND o.id < x.id
		ORDER BY o.at DESC, o.id DESC LIMIT ?2`, scanObservation, id, n)
	if err != nil {
		return nil, nil, err
	}
	after, err = query(ctx, tx, near+`WHERE o.at > x.at OR o.at = x.at AND o.id > x.id
		ORDER BY o.at, o.id LIMIT ?2`, scanObservation, id, n)
	if err != nil {
		return nil, nil, err
	}
	slices.Reverse(before)
	return before, after, nil
}

func scanObservation(rows *sql.Rows) (Observation, error) {
	var o Observation
	err := rows.Scan(&o.ID, &o.CaptureIdentity, &o.Source, &o.SegmentID, &o.Event, &o.Excerpt,
		&o.Truncated, &o.Status, &o.Sender, &o.Counts, &o.BrainOrigin, timeDest{&o.At}, &o.EnrichedBy)
	return o, err
}
