package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// The M2 counts of brain status (E15-S2). Each one is a raw count with no score, and
// each one comes with the sessions observed and the capture gaps of the same span. They read
// ids, kinds and times only, never text.

// Coverage is what capture saw in a span: the sessions with an observation, and the gap
// observations. A partial read is not a gap: it holds text.
type Coverage struct {
	Sessions, Gaps int
}

// Week is the counts of one week.
type Week struct {
	Start time.Time
	Coverage
	// Corrections counts the correction candidates with evidence in the week: the repeats.
	Corrections int
	// Proposals counts the proposals made in the week.
	Proposals int
	// Decided counts the proposals decided in the week by result: approved, edited (approved
	// with an edit), rejected and changed (you asked for a change).
	Decided map[string]int
	// ReviewSeconds holds the review time of each decided proposal that has one, sorted.
	ReviewSeconds []int64
	// Shown counts the live suggestions shown in the week, and Suggested counts them by
	// result; a suggestion that waits for you has none. Matched counts them by reason; a row
	// of an older Brain counts under "" (E20-S9).
	Shown     int
	Suggested map[SuggestionResult]int
	Matched   map[SuggestionReason]int
}

// The review results of Week.Decided. edited is not a proposal status: it is an approved
// proposal whose revision text is not the proposal text.
const (
	ReviewApproved = "approved"
	ReviewEdited   = "edited"
	ReviewRejected = "rejected"
	ReviewChanged  = "changed"
)

// Weeks counts the weeks that start at starts, the newest first. Each week ends where the
// next newer one starts; the newest one has no end, so a time after the call still counts.
func Weeks(ctx context.Context, tx *sql.Tx, starts []time.Time) ([]Week, error) {
	ws := make([]Week, len(starts))
	for i, s := range starts {
		ws[i] = Week{Start: s, Decided: map[string]int{}, Suggested: map[SuggestionResult]int{},
			Matched: map[SuggestionReason]int{}}
	}
	if len(starts) == 0 {
		return ws, nil
	}
	since := timeArg(starts[len(starts)-1])
	week := func(at time.Time) *Week {
		i := slices.IndexFunc(starts, func(s time.Time) bool { return !at.Before(s) })
		return &ws[i]
	}
	// A session or a candidate counts once in each week: one row for each day is enough, as a
	// week starts at midnight UTC.
	sessions := map[*Week]map[int64]bool{}
	err := eachRow(ctx, tx, `SELECT DISTINCT g.session_id, substr(o.at, 1, 10) || 'T00:00:00Z' FROM observations o
		JOIN segments g ON g.id = o.segment_id WHERE o.at >= ?`, func(rows *sql.Rows) error {
		var id int64
		var at time.Time
		if err := rows.Scan(&id, timeDest{&at}); err != nil {
			return err
		}
		w := week(at)
		if sessions[w] == nil {
			sessions[w] = map[int64]bool{}
		}
		if !sessions[w][id] {
			sessions[w][id] = true
			w.Sessions++
		}
		return nil
	}, since)
	if err != nil {
		return nil, fmt.Errorf("count sessions observed: %w", err)
	}
	err = eachRow(ctx, tx, `SELECT at FROM observations WHERE status = ? AND at >= ?`, func(rows *sql.Rows) error {
		var at time.Time
		if err := rows.Scan(timeDest{&at}); err != nil {
			return err
		}
		week(at).Gaps++
		return nil
	}, ObservationGap, since)
	if err != nil {
		return nil, fmt.Errorf("count capture gaps: %w", err)
	}
	corrections := map[*Week]map[int64]bool{}
	err = eachRow(ctx, tx, `SELECT DISTINCT e.candidate_id, substr(e.at, 1, 10) || 'T00:00:00Z' FROM evidence_links e
		JOIN candidates c ON c.id = e.candidate_id WHERE c.kind = ? AND e.at >= ?`, func(rows *sql.Rows) error {
		var id int64
		var at time.Time
		if err := rows.Scan(&id, timeDest{&at}); err != nil {
			return err
		}
		w := week(at)
		if corrections[w] == nil {
			corrections[w] = map[int64]bool{}
		}
		if !corrections[w][id] {
			corrections[w][id] = true
			w.Corrections++
		}
		return nil
	}, CandidateCorrection, since)
	if err != nil {
		return nil, fmt.Errorf("count correction candidates: %w", err)
	}
	err = eachRow(ctx, tx, `SELECT created FROM proposals WHERE created >= ?`, func(rows *sql.Rows) error {
		var at time.Time
		if err := rows.Scan(timeDest{&at}); err != nil {
			return err
		}
		week(at).Proposals++
		return nil
	}, since)
	if err != nil {
		return nil, fmt.Errorf("count proposals: %w", err)
	}
	if err := decided(ctx, tx, since, week); err != nil {
		return nil, err
	}
	err = eachRow(ctx, tx, `SELECT shown_at, COALESCE(result, ''), COALESCE(reason, '') FROM suggestions
		WHERE shown_at >= ?`,
		func(rows *sql.Rows) error {
			var at time.Time
			var r SuggestionResult
			var why SuggestionReason
			if err := rows.Scan(timeDest{&at}, &r, &why); err != nil {
				return err
			}
			w := week(at)
			w.Shown++
			w.Matched[why]++
			if r != "" {
				w.Suggested[r]++
			}
			return nil
		}, since)
	if err != nil {
		return nil, fmt.Errorf("count live suggestions: %w", err)
	}
	for i := range ws {
		slices.Sort(ws[i].ReviewSeconds)
	}
	return ws, nil
}

// decided counts the proposals decided since since into their weeks. An approve writes the
// revision after the base revision of the proposal; a draft revision of brain remember holds
// the proposal id too, so the revision number picks the approved one. The review time is
// review_seconds when a surface recorded it, else the time from the newest shown suggestion
// of the proposal to the decision, when the decision came from a suggestion. A proposal
// that Brain superseded is not your decision.
func decided(ctx context.Context, tx *sql.Tx, since any, week func(time.Time) *Week) error {
	err := eachRow(ctx, tx, `SELECT p.decided,
		CASE WHEN p.status = ?1 AND p.text != '' AND (SELECT r.text FROM item_revisions r
			WHERE r.item_id = p.item_id AND r.revision = p.base_revision + 1 AND r.proposal_id = p.id) != p.text THEN ?2 ELSE p.status END,
		p.review_seconds, (SELECT max(s.shown_at) FROM suggestions s WHERE s.proposal_id = p.id
			AND s.result IN (?3, ?4, ?5))
		FROM proposals p WHERE p.status IN (?1, ?6, ?7) AND p.decided >= ?8`,
		func(rows *sql.Rows) error {
			var at, shown time.Time
			var result string
			var secs sql.NullInt64
			if err := rows.Scan(timeDest{&at}, &result, &secs, timeDest{&shown}); err != nil {
				return err
			}
			w := week(at)
			w.Decided[result]++
			switch {
			case secs.Valid:
				w.ReviewSeconds = append(w.ReviewSeconds, secs.Int64)
			case !shown.IsZero() && !at.Before(shown):
				w.ReviewSeconds = append(w.ReviewSeconds, int64(at.Sub(shown)/time.Second))
			}
			return nil
		}, ProposalApproved, ReviewEdited, SuggestionApproved, SuggestionRejected, SuggestionChanged,
		ProposalRejected, ProposalChanged, since)
	if err != nil {
		return fmt.Errorf("count decided proposals: %w", err)
	}
	return nil
}

// Observed is the coverage of all time: the sessions with an observation and the gap
// observations. It is the span of the outcome counts.
func Observed(ctx context.Context, tx *sql.Tx) (Coverage, error) {
	var c Coverage
	err := tx.QueryRowContext(ctx, `SELECT
		(SELECT count(DISTINCT g.session_id) FROM observations o JOIN segments g ON g.id = o.segment_id),
		(SELECT count(*) FROM observations WHERE status = ?)`, ObservationGap).Scan(&c.Sessions, &c.Gaps)
	if err != nil {
		return Coverage{}, fmt.Errorf("count sessions observed: %w", err)
	}
	return c, nil
}

// OutcomeCount is the outcomes of one kind on one item revision.
type OutcomeCount struct {
	ItemID, Revision int64
	Kind             OutcomeKind
	// Observations are the examples: the observation ids of the rows, the newest first.
	Observations []int64
}

// RevisionOutcomes returns the comebacks and the uncertain matches of each item revision, by
// item, the newest revision first, then by kind. A gap or an unknown delivery names no revision:
// DeliveryGaps counts those by adapter.
func RevisionOutcomes(ctx context.Context, tx *sql.Tx) ([]OutcomeCount, error) {
	var cs []OutcomeCount
	err := eachRow(ctx, tx, `SELECT item_id, revision, kind, observation_id FROM outcomes WHERE kind IN (?, ?)
		ORDER BY item_id, revision DESC, kind, at DESC, id DESC`, func(rows *sql.Rows) error {
		var c OutcomeCount
		var obs int64
		if err := rows.Scan(&c.ItemID, &c.Revision, &c.Kind, &obs); err != nil {
			return err
		}
		if n := len(cs); n > 0 && cs[n-1].ItemID == c.ItemID && cs[n-1].Revision == c.Revision && cs[n-1].Kind == c.Kind {
			cs[n-1].Observations = append(cs[n-1].Observations, obs)
			return nil
		}
		c.Observations = []int64{obs}
		cs = append(cs, c)
		return nil
	}, OutcomeComeback, OutcomeUncertain)
	if err != nil {
		return nil, fmt.Errorf("count outcomes: %w", err)
	}
	return cs, nil
}

// DeliveryGap is the delivery of the observed sessions of one adapter in one binding.
// Each count is a count of sessions.
type DeliveryGap struct {
	Coverage
	// NoID: herdr gave no agent session id, and no single receipt ties by the capture row of
	// the session (TiedReceipt).
	NoID int
	// NoReceipt: no receipt of the agent session id. Withheld: its newest receipt is withheld.
	NoReceipt, Withheld int
	// Gap and Unknown: sessions with a delivery_gap or a delivery_unknown outcome.
	Gap, Unknown int
}

// DeliveryGaps counts the observed sessions of harness profile in the binding of herdr session
// session. A receipt joins a session by binding, harness and agent session id, as in
// LatestReceipt.
func DeliveryGaps(ctx context.Context, tx *sql.Tx, session, profile string) (DeliveryGap, error) {
	var d DeliveryGap
	err := tx.QueryRowContext(ctx, `WITH observed AS (SELECT * FROM sessions s WHERE s.herdr_session = ?1
			AND s.harness = ?2 AND EXISTS (SELECT 1 FROM segments g JOIN observations o ON o.segment_id = g.id
			WHERE g.session_id = s.id)),
		newest AS (SELECT s.id, s.agent_session_id, (SELECT d.result FROM deliveries d WHERE d.herdr_session = ?1
			AND d.profile = ?2 AND d.agent_session_id = s.agent_session_id ORDER BY d.id DESC LIMIT 1) AS result
			FROM observed s)
		SELECT (SELECT count(*) FROM observed),
			(SELECT count(*) FROM observations o JOIN segments g ON g.id = o.segment_id
				WHERE g.session_id IN (SELECT id FROM observed) AND o.status = ?3),
			(SELECT count(*) FROM newest WHERE agent_session_id IS NOT NULL AND result IS NULL),
			(SELECT count(*) FROM newest WHERE result = ?4),
			(SELECT count(DISTINCT session_id) FROM outcomes WHERE kind = ?5 AND session_id IN (SELECT id FROM observed)),
			(SELECT count(DISTINCT session_id) FROM outcomes WHERE kind = ?6 AND session_id IN (SELECT id FROM observed))`,
		session, profile, ObservationGap, ResultWithheld, OutcomeGap, OutcomeUnknown).Scan(
		&d.Sessions, &d.Gaps, &d.NoReceipt, &d.Withheld, &d.Gap, &d.Unknown)
	if err != nil {
		return DeliveryGap{}, fmt.Errorf("count delivery gaps of %s: %w", profile, err)
	}
	var last []int64
	err = eachRow(ctx, tx, `SELECT max(o.id) FROM sessions s JOIN segments g ON g.session_id = s.id
		JOIN observations o ON o.segment_id = g.id
		WHERE s.herdr_session = ? AND s.harness = ? AND s.agent_session_id IS NULL GROUP BY s.id`,
		func(rows *sql.Rows) error {
			var obs int64
			err := rows.Scan(&obs)
			last = append(last, obs)
			return err
		}, session, profile)
	if err != nil {
		return DeliveryGap{}, fmt.Errorf("count delivery gaps of %s: %w", profile, err)
	}
	for _, obs := range last {
		r, ok, err := TiedReceipt(ctx, tx, obs)
		switch {
		case err != nil:
			return DeliveryGap{}, err
		case !ok:
			d.NoID++
		case r.Result == ResultWithheld:
			d.Withheld++
		}
	}
	return d, nil
}

// eachRow runs q and calls row for each row.
func eachRow(ctx context.Context, tx *sql.Tx, q string, row func(*sql.Rows) error, args ...any) error {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
	for rows.Next() {
		if err := row(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
