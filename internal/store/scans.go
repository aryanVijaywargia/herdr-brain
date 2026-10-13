package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// ScanStatus is the status of the history scan (§6.8, §11).
type ScanStatus string

// The scan statuses.
const (
	ScanPending    ScanStatus = "pending"    // setup wrote it; no brained claimed it yet
	ScanReading    ScanStatus = "reading"    // a brained of Binding reads the session files
	ScanExtracting ScanStatus = "extracting" // the read ended; scan batches wait or run
	ScanDone       ScanStatus = "done"       // every scan batch is committed
)

// SkipReason says why the scan did not read a session file (§6.8 "Privacy and forget").
type SkipReason string

// The skip reasons.
const (
	SkipNotInteractive SkipReason = "not_interactive"
	SkipFork           SkipReason = "fork"
	SkipKnown          SkipReason = "known"
	SkipSharedCheckout SkipReason = "shared_checkout"
	SkipLearningOff    SkipReason = "learning_off"
	SkipCompressed     SkipReason = "compressed"
	SkipUnknownFormat  SkipReason = "unknown_format"
)

// ScanCounts are the session counts of one CLI in the scan.
type ScanCounts struct {
	Found   int                `json:"found"`
	Read    int                `json:"read"`
	Skipped map[SkipReason]int `json:"skipped,omitempty"`
}

// FindingPrompt is the kind of a repeated-prompt finding. The other findings have the kind of
// a candidate (§7.1).
const FindingPrompt CandidateKind = "prompt"

// ScanGates returns the gates of the scan findings: the live gates g, and a repeated prompt at
// 2 times [NEW] (§6.8 "Repeats").
func ScanGates(g Gates) Gates {
	s := maps.Clone(g)
	s[FindingPrompt] = Gate{Min: 2}
	return s
}

// Scan is the history scan row of the store. The row holds no text.
type Scan struct {
	ID        int64
	Binding   string // the herdr session of the brained that claimed it; "" before the claim
	Status    ScanStatus
	Requested time.Time
	Claimed   time.Time             // zero before the claim
	Finished  time.Time             // zero until done
	Counts    map[string]ScanCounts // by harness
}

// ScanTurn is one turn of a scanned session: a prompt and everything up to the next prompt.
type ScanTurn struct {
	ID     int64
	Tokens int64 // the tokens that the file records; 0 when !Known
	Known  bool  // false for a turn with no usage field: its tokens are unknown, never 0
}

// Finding is one repeat of the scan, as ids only (§6.8 "Repeats"). ID is its place in the
// list; Scope is the workspace key, "" for the global pool.
type Finding struct {
	ID    int           `json:"id"`
	Kind  CandidateKind `json:"kind"`
	Scope string        `json:"scope"`
	Obs   []FindingObs  `json:"obs"`
}

// FindingObs is one observation of a finding and the turn it belongs to. Identity is the
// capture identity of the observation: a forget deletes the row, and the identity check stops a
// reused row id, as ObservationHeld does.
type FindingObs struct {
	Observation int64  `json:"observation"`
	Identity    string `json:"identity"`
	Turn        int64  `json:"turn"`
}

const scanColumns = `id, COALESCE(binding, ''), status, requested, claimed, finished, counts`

func scanScan(row *sql.Row) (Scan, error) {
	var s Scan
	var counts string
	err := row.Scan(&s.ID, &s.Binding, &s.Status, timeDest{&s.Requested}, timeDest{&s.Claimed},
		timeDest{&s.Finished}, &counts)
	if err != nil {
		return Scan{}, err
	}
	if err := json.Unmarshal([]byte(counts), &s.Counts); err != nil {
		return Scan{}, fmt.Errorf("decode scan counts: %w", err)
	}
	return s, nil
}

// HistoryScan returns the scan row of the store; false when the store has none.
func HistoryScan(ctx context.Context, tx *sql.Tx) (Scan, bool, error) {
	s, err := scanScan(tx.QueryRowContext(ctx, `SELECT `+scanColumns+` FROM history_scans`))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Scan{}, false, nil
	case err != nil:
		return Scan{}, false, fmt.Errorf("read history scan: %w", err)
	}
	return s, true, nil
}

// RequestScan writes the pending scan row of the store, the yes of setup. A store with a row
// keeps it: the scan runs once (§6.8 "Once").
func RequestScan(ctx context.Context, tx *sql.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO history_scans (id, status, requested) VALUES (1, ?, ?)
		ON CONFLICT DO NOTHING`, ScanPending, timeArg(now))
	if err != nil {
		return fmt.Errorf("request history scan: %w", err)
	}
	return nil
}

// ClaimScan claims the scan for the brained of binding: a pending row, or a reading row of
// the same binding, which a restart reads again with its first claim time (§6.8 "Where it
// runs"). It returns false when there is nothing to claim. One caller wins.
func ClaimScan(ctx context.Context, tx *sql.Tx, binding string, now time.Time) (Scan, bool, error) {
	s, err := scanScan(tx.QueryRowContext(ctx, `UPDATE history_scans SET status = ?1, binding = ?2,
		claimed = COALESCE(claimed, ?3) WHERE status = ?4 OR status = ?1 AND binding = ?2 RETURNING `+scanColumns,
		ScanReading, binding, timeArg(now), ScanPending))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Scan{}, false, nil
	case err != nil:
		return Scan{}, false, fmt.Errorf("claim history scan: %w", err)
	}
	return s, true, nil
}

// SetScanStatus sets the status of scan id. The status done records now as its end.
func SetScanStatus(ctx context.Context, tx *sql.Tx, id int64, status ScanStatus, now time.Time) error {
	var finished any
	if status == ScanDone {
		finished = timeArg(now)
	}
	_, err := tx.ExecContext(ctx, `UPDATE history_scans SET status = ?, finished = ? WHERE id = ?`, status, finished, id)
	if err != nil {
		return fmt.Errorf("set history scan status: %w", err)
	}
	return nil
}

// SetScanCounts replaces the session counts of scan id, by harness.
func SetScanCounts(ctx context.Context, tx *sql.Tx, id int64, counts map[string]ScanCounts) error {
	return setScanJSON(ctx, tx, id, "counts", counts)
}

// AddScanTurns adds turns to scan id. A turn with a known id is replaced, so a second read of
// a session counts its tokens once.
func AddScanTurns(ctx context.Context, tx *sql.Tx, id int64, turns []ScanTurn) error {
	all, err := scanTurns(ctx, tx, id)
	if err != nil {
		return err
	}
	for _, t := range turns {
		all[t.ID] = nil
		if t.Known {
			all[t.ID] = &t.Tokens
		}
	}
	return setScanJSON(ctx, tx, id, "turns", all)
}

// SetScanFindings replaces the findings of scan id, in finding order. The list is fixed at the
// end of the read (§6.8 "Repeats").
func SetScanFindings(ctx context.Context, tx *sql.Tx, id int64, findings []Finding) error {
	if findings == nil {
		findings = []Finding{}
	}
	return setScanJSON(ctx, tx, id, "findings", findings)
}

// ScanProgress returns the last committed finding id of scope in scan id: 0 for none.
func ScanProgress(ctx context.Context, tx *sql.Tx, id int64, scope string) (int, error) {
	p, err := scanProgress(ctx, tx, id)
	return p[scope], err
}

// MoveScanProgress moves the last committed finding id of scope in scan id from from to to.
// The caller moves it in the transaction that commits the scan batch (§6.8 "Proposals", G3).
// It returns false and changes nothing when the progress is no longer from.
func MoveScanProgress(ctx context.Context, tx *sql.Tx, id int64, scope string, from, to int) (bool, error) {
	p, err := scanProgress(ctx, tx, id)
	if err != nil || p[scope] != from {
		return false, err
	}
	p[scope] = to
	return true, setScanJSON(ctx, tx, id, "progress", p)
}

func scanProgress(ctx context.Context, tx *sql.Tx, id int64) (map[string]int, error) {
	p := map[string]int{}
	return p, readScanJSON(ctx, tx, id, "progress", &p)
}

func scanTurns(ctx context.Context, tx *sql.Tx, id int64) (map[int64]*int64, error) {
	t := map[int64]*int64{}
	return t, readScanJSON(ctx, tx, id, "turns", &t)
}

// readScanJSON decodes the JSON column of scan id into v. column is a fixed name of this file.
func readScanJSON(ctx context.Context, tx *sql.Tx, id int64, column string, v any) error {
	var s string
	if err := tx.QueryRowContext(ctx, `SELECT `+column+` FROM history_scans WHERE id = ?`, id).Scan(&s); err != nil {
		return fmt.Errorf("read history scan %s: %w", column, err)
	}
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return fmt.Errorf("decode history scan %s: %w", column, err)
	}
	return nil
}

// setScanJSON writes v as JSON to the column of scan id. column is a fixed name of this file.
func setScanJSON(ctx context.Context, tx *sql.Tx, id int64, column string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode history scan %s: %w", column, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE history_scans SET `+column+` = ? WHERE id = ?`, string(b), id); err != nil {
		return fmt.Errorf("write history scan %s: %w", column, err)
	}
	return nil
}

// ScanReport holds the report numbers of the scan, from the observations that still exist
// (§6.8 "Repeats"). A forget removes its observations from the numbers.
type ScanReport struct {
	Scan      Scan
	All       TurnTokens // every turn that the scan read
	Repeats   []Repeat
	InRepeats TurnTokens // the distinct turns of every repeat
}

// TurnTokens are the recorded tokens of a set of turns, and the count of its turns with no
// usage field.
type TurnTokens struct {
	Tokens  int64
	Unknown int
}

// add adds one turn: its recorded tokens, or nil for a turn with no usage field.
func (t *TurnTokens) add(tokens *int64) {
	if tokens == nil {
		t.Unknown++
		return
	}
	t.Tokens += *tokens
}

// Repeat is one finding at the gate of its kind, with the numbers of its observations that
// still exist.
type Repeat struct {
	Finding  Finding // Obs holds only the observations that still exist
	Sessions int
	Turns    TurnTokens // over its distinct turns
	Newest   int64      // the id of its newest observation
	Excerpt  string     // the excerpt of Newest
}

// HistoryScanReport returns the report numbers of scan id. It keeps a finding only while its
// observations reach the gate of its kind in gates (ScanGates). The repeats come most tokens
// first, then in finding order.
func HistoryScanReport(ctx context.Context, tx *sql.Tx, id int64, gates Gates) (ScanReport, error) {
	s, err := scanScan(tx.QueryRowContext(ctx, `SELECT `+scanColumns+` FROM history_scans WHERE id = ?`, id))
	if err != nil {
		return ScanReport{}, fmt.Errorf("read history scan: %w", err)
	}
	turns, err := scanTurns(ctx, tx, id)
	if err != nil {
		return ScanReport{}, err
	}
	var findings []Finding
	if err := readScanJSON(ctx, tx, id, "findings", &findings); err != nil {
		return ScanReport{}, err
	}
	r := ScanReport{Scan: s}
	for _, t := range turns {
		r.All.add(t)
	}
	inRepeats := map[int64]bool{}
	for _, f := range findings {
		rep, ok, err := repeat(ctx, tx, f, turns, gates)
		if err != nil {
			return ScanReport{}, err
		}
		if !ok {
			continue
		}
		r.Repeats = append(r.Repeats, rep)
		for _, o := range rep.Finding.Obs {
			if !inRepeats[o.Turn] {
				inRepeats[o.Turn] = true
				r.InRepeats.add(turns[o.Turn])
			}
		}
	}
	slices.SortStableFunc(r.Repeats, func(a, b Repeat) int { return cmp.Compare(b.Turns.Tokens, a.Turns.Tokens) })
	return r, nil
}

// repeat returns the numbers of finding f from its observations that still exist, and whether
// they reach the gate of its kind.
func repeat(ctx context.Context, tx *sql.Tx, f Finding, turns map[int64]*int64, gates Gates) (Repeat, bool, error) {
	if len(f.Obs) == 0 {
		return Repeat{}, false, nil
	}
	ids := make([]any, len(f.Obs))
	identity := map[int64]string{}
	for i, o := range f.Obs {
		ids[i] = o.Observation
		identity[o.Observation] = o.Identity
	}
	type held struct {
		id, session       int64
		identity, excerpt string
	}
	// Newest last: the time of the observation, then its id.
	hs, err := query(ctx, tx, `SELECT o.id, g.session_id, o.capture_identity, o.excerpt FROM observations o
		JOIN segments g ON g.id = o.segment_id WHERE o.id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)
		ORDER BY o.at, o.id`, func(rows *sql.Rows) (held, error) {
		var h held
		err := rows.Scan(&h.id, &h.session, &h.identity, &h.excerpt)
		return h, err
	}, ids...)
	if err != nil {
		return Repeat{}, false, fmt.Errorf("read finding observations: %w", err)
	}
	hs = slices.DeleteFunc(hs, func(h held) bool { return identity[h.id] != h.identity })
	if len(hs) == 0 {
		return Repeat{}, false, nil
	}
	exists := map[int64]bool{}
	sessions := map[int64]bool{}
	for _, h := range hs {
		exists[h.id] = true
		sessions[h.session] = true
	}
	rep := Repeat{Finding: f, Sessions: len(sessions)}
	rep.Finding.Obs = slices.DeleteFunc(slices.Clone(f.Obs), func(o FindingObs) bool { return !exists[o.Observation] })
	if !gates.Reached(Candidate{Kind: f.Kind, Count: len(rep.Finding.Obs), Sessions: rep.Sessions}) {
		return Repeat{}, false, nil
	}
	seen := map[int64]bool{}
	for _, o := range rep.Finding.Obs {
		if !seen[o.Turn] {
			seen[o.Turn] = true
			rep.Turns.add(turns[o.Turn])
		}
	}
	rep.Newest, rep.Excerpt = hs[len(hs)-1].id, hs[len(hs)-1].excerpt
	return rep, true, nil
}
