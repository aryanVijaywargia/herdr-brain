package store

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// scanSession adds one scanned session of binding default with one observation for each
// excerpt, one minute apart, and returns the session id and the observations, with no turn.
func scanSession(t *testing.T, tx *sql.Tx, id string, excerpts ...string) (int64, []FindingObs) {
	t.Helper()
	ctx := context.Background()
	key := SessionKey{HerdrSession: "default", Harness: "claude", Kind: KeyAgent, ID: id}
	sid, err := OpenSession(ctx, tx, key, "", "k1", testNow)
	if err != nil {
		t.Fatal(err)
	}
	seg, err := StartSegment(ctx, tx, sid, "", "k1", "visible", testNow)
	if err != nil {
		t.Fatal(err)
	}
	var obs []FindingObs
	for i, e := range excerpts {
		o := Observation{CaptureIdentity: fmt.Sprintf("claude:%s-%d", id, i), Source: SourceHook, SegmentID: seg,
			Event: "prompt", Excerpt: e, Status: ObservationComplete, Counts: true,
			At: testNow.Add(time.Duration(i) * time.Minute)}
		oid, err := AddObservation(ctx, tx, o)
		if err != nil {
			t.Fatal(err)
		}
		obs = append(obs, FindingObs{Observation: oid, Identity: o.CaptureIdentity})
	}
	return sid, obs
}

// inTurn is o in turn.
func inTurn(o FindingObs, turn int64) FindingObs {
	o.Turn = turn
	return o
}

func readScan(t *testing.T, s *Store) (Scan, bool) {
	t.Helper()
	var sc Scan
	var ok bool
	write(t, s, func(tx *sql.Tx) (err error) {
		sc, ok, err = HistoryScan(context.Background(), tx)
		return err
	})
	return sc, ok
}

// AC 1: the scan row holds only ids, numbers, kinds, scope keys and times, never the text of
// the sessions that it read. A second request keeps the one row.
func TestHistoryScanRowHoldsNoText(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	if _, ok := readScan(t, s); ok {
		t.Fatal("HistoryScan of a new store = a row, want none")
	}
	write(t, s, func(tx *sql.Tx) error {
		if err := RequestScan(ctx, tx, testNow); err != nil {
			return err
		}
		return RequestScan(ctx, tx, testNow.Add(time.Hour))
	})
	sc, ok := readScan(t, s)
	if !ok || sc.Status != ScanPending || !sc.Requested.Equal(testNow) || !sc.Claimed.IsZero() || sc.Binding != "" {
		t.Fatalf("HistoryScan = %+v, %v, want one pending row of the first request, not claimed", sc, ok)
	}

	secret := "use pnpm, not npm"
	write(t, s, func(tx *sql.Tx) error {
		// No findings is an empty list.
		if err := SetScanFindings(ctx, tx, sc.ID, nil); err != nil {
			return err
		}
		_, obs := scanSession(t, tx, "s1", secret, secret+" again")
		if err := SetScanCounts(ctx, tx, sc.ID, map[string]ScanCounts{
			"claude": {Found: 3, Read: 1, Skipped: map[SkipReason]int{SkipKnown: 1, SkipUnknownFormat: 1}},
		}); err != nil {
			return err
		}
		if err := AddScanTurns(ctx, tx, sc.ID, []ScanTurn{{ID: 7, Tokens: 48200, Known: true}, {ID: 8}}); err != nil {
			return err
		}
		return SetScanFindings(ctx, tx, sc.ID, []Finding{{ID: 1, Kind: CandidateCorrection, Scope: "k1",
			Obs: []FindingObs{inTurn(obs[0], 7), inTurn(obs[1], 8)}}})
	})

	var row []string
	write(t, s, func(tx *sql.Tx) error {
		r := make([]sql.NullString, 10)
		dest := make([]any, len(r))
		for i := range r {
			dest[i] = &r[i]
		}
		if err := tx.QueryRowContext(ctx, `SELECT CAST(id AS TEXT), binding, status, requested, claimed, finished,
			counts, turns, findings, progress FROM history_scans`).Scan(dest...); err != nil {
			return err
		}
		for _, v := range r {
			row = append(row, v.String)
		}
		return nil
	})
	want := []string{"1", "", "pending", "2026-09-28T10:30:15.123Z", "", "",
		`{"claude":{"found":3,"read":1,"skipped":{"known":1,"unknown_format":1}}}`,
		`{"7":48200,"8":null}`,
		`[{"id":1,"kind":"correction","scope":"k1","obs":[{"observation":1,"identity":"claude:s1-0","turn":7},{"observation":2,"identity":"claude:s1-1","turn":8}]}]`,
		`{}`}
	if !reflect.DeepEqual(row, want) {
		t.Errorf("history_scans row = %q,\nwant %q", row, want)
	}
	if strings.Contains(strings.Join(row, " "), "pnpm") {
		t.Errorf("history_scans row holds session text: %q", row)
	}
}

// AC 4: one caller wins the pending row. A restart of the same binding claims its reading row
// again with its first claim time; another binding gets nothing.
func TestClaimScan(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var stores []*Store
	for range 2 {
		st, err := Open(ctx, dir, testTimeout)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { closeTest(t, st) })
		stores = append(stores, st)
	}
	a, b := stores[0], stores[1]
	write(t, a, func(tx *sql.Tx) error { return RequestScan(ctx, tx, testNow) })

	var wg sync.WaitGroup
	won := make([]bool, 2)
	errs := make([]error, 2)
	for i, s := range []*Store{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Write(ctx, func(tx *sql.Tx) (err error) {
				_, won[i], err = ClaimScan(ctx, tx, fmt.Sprintf("session-%d", i), testNow)
				return err
			})
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || won[0] == won[1] {
		t.Fatalf("two claims at once = %v %v, errors %v %v; want one winner", won[0], won[1], errs[0], errs[1])
	}
	winner, loser := "session-0", "session-1"
	if won[1] {
		winner, loser = loser, winner
	}
	if sc, _ := readScan(t, a); sc.Status != ScanReading || sc.Binding != winner || !sc.Claimed.Equal(testNow) {
		t.Errorf("scan after the claim = %+v, want reading by %s at the claim time", sc, winner)
	}

	later := testNow.Add(time.Hour)
	for binding, want := range map[string]bool{winner: true, loser: false} {
		write(t, a, func(tx *sql.Tx) error {
			sc, ok, err := ClaimScan(ctx, tx, binding, later)
			if ok != want || ok && !sc.Claimed.Equal(testNow) {
				t.Errorf("ClaimScan(%s) again = %+v, %v; want %v with the first claim time", binding, sc, ok, want)
			}
			return err
		})
	}
	write(t, a, func(tx *sql.Tx) error {
		if err := SetScanStatus(ctx, tx, 1, ScanDone, later); err != nil {
			return err
		}
		sc, ok, err := ClaimScan(ctx, tx, winner, later)
		if ok {
			t.Errorf("ClaimScan of a done scan = %+v, want nothing to claim", sc)
		}
		return err
	})
	if sc, _ := readScan(t, a); sc.Status != ScanDone || !sc.Finished.Equal(later) {
		t.Errorf("scan = %+v, want done at %v", sc, later)
	}
}

// AC 4: the progress of each scope moves only from the value that the caller read.
func TestMoveScanProgress(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	write(t, s, func(tx *sql.Tx) error {
		if err := RequestScan(ctx, tx, testNow); err != nil {
			return err
		}
		for _, c := range []struct {
			scope    string
			from, to int
			moved    bool
		}{
			{"k1", 0, 3, true},
			{"k1", 0, 5, false},
			{"", 0, 2, true},
			{"k1", 3, 5, true},
		} {
			moved, err := MoveScanProgress(ctx, tx, 1, c.scope, c.from, c.to)
			if err != nil || moved != c.moved {
				t.Errorf("MoveScanProgress(%q, %d, %d) = %v, %v; want %v", c.scope, c.from, c.to, moved, err, c.moved)
			}
		}
		for scope, want := range map[string]int{"k1": 5, "": 2, "k2": 0} {
			if got, err := ScanProgress(ctx, tx, 1, scope); err != nil || got != want {
				t.Errorf("ScanProgress(%q) = %d, %v; want %d", scope, got, err, want)
			}
		}
		return nil
	})
}

// AC 4: the report numbers come from the observations that still exist. A forget takes its
// observations out, and a finding below its gate leaves the report. Turns with no usage show
// apart, and a turn counts once.
func TestHistoryScanReport(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	var forget int64
	write(t, s, func(tx *sql.Tx) error {
		if err := RequestScan(ctx, tx, testNow); err != nil {
			return err
		}
		s1, a := scanSession(t, tx, "s1", "use pnpm", "use pnpm again", "go test ./...")
		_, b := scanSession(t, tx, "s2", "use pnpm, please", "go test ./...")
		forget = s1
		if err := AddScanTurns(ctx, tx, 1, []ScanTurn{
			{ID: 1, Tokens: 1000, Known: true}, {ID: 2, Tokens: 500, Known: true}, {ID: 3},
			{ID: 4, Tokens: 9000, Known: true}, {ID: 5, Tokens: 20, Known: true},
		}); err != nil {
			return err
		}
		// A second read of turn 2 replaces it.
		if err := AddScanTurns(ctx, tx, 1, []ScanTurn{{ID: 2, Tokens: 500, Known: true}}); err != nil {
			return err
		}
		return SetScanFindings(ctx, tx, 1, []Finding{
			{ID: 1, Kind: CandidateCorrection, Scope: "k1", Obs: []FindingObs{inTurn(a[0], 1), inTurn(a[1], 1), inTurn(b[0], 3)}},
			{ID: 2, Kind: FindingPrompt, Scope: "k1", Obs: []FindingObs{inTurn(a[2], 2), inTurn(b[1], 4)}},
			{ID: 3, Kind: CandidateLesson, Scope: "", Obs: []FindingObs{inTurn(b[1], 4)}},
			{ID: 4, Kind: CandidateProcedure, Scope: "k1", Obs: []FindingObs{inTurn(a[0], 1)}},
			// A reused row id with another capture identity is not the observation of the finding.
			{ID: 5, Kind: CandidateLesson, Scope: "", Obs: []FindingObs{{Observation: b[1].Observation, Identity: "claude:gone-0", Turn: 4}}},
		})
	})
	report := func() ScanReport {
		var r ScanReport
		write(t, s, func(tx *sql.Tx) (err error) {
			r, err = HistoryScanReport(ctx, tx, 1, ScanGates(DefaultGates()))
			return err
		})
		return r
	}

	r := report()
	if r.All != (TurnTokens{10520, 4, 1}) || r.InRepeats != (TurnTokens{10500, 3, 1}) {
		t.Errorf("tokens = all %+v, in repeats %+v; want {10520 4 1} and {10500 3 1}", r.All, r.InRepeats)
	}
	var got []string
	for _, rep := range r.Repeats {
		got = append(got, fmt.Sprintf("%d %s %d sessions %d+%d %q", rep.Finding.ID, rep.Finding.Kind, rep.Sessions,
			rep.Turns.Tokens, rep.Turns.Unknown, rep.Excerpt))
	}
	want := []string{
		`2 prompt 2 sessions 9500+0 "go test ./..."`,
		`3 lesson 1 sessions 9000+0 "go test ./..."`,
		`1 correction 2 sessions 1000+1 "use pnpm again"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("repeats = %q,\nwant %q (most tokens first; the procedure is below its gate; finding 5 has no observation)", got, want)
	}

	write(t, s, func(tx *sql.Tx) error { return ForgetSession(ctx, tx, forget) })
	got = nil
	for _, rep := range report().Repeats {
		got = append(got, fmt.Sprintf("%d %d %d %q", rep.Finding.ID, len(rep.Finding.Obs), rep.Turns.Tokens, rep.Excerpt))
	}
	if want := []string{`3 1 9000 "go test ./..."`}; !reflect.DeepEqual(got, want) {
		t.Errorf("repeats after a forget = %q, want %q", got, want)
	}
}

// AC 1: a scan batch has the stable id (scan, scope, first and last finding id), and one scan
// batch is active at a time. A live batch of the same session stays apart.
func TestScanBatchIndexes(t *testing.T) {
	ctx := context.Background()
	s := openM1(t)
	add := func(scan any, first, last int, status string) error {
		return s.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO extraction_batches (session_id, workspace_key, first_capture,
				last_capture, start_watermark, last_observation, status, model_calls, reason, at, scan_id, first_finding,
				last_finding) VALUES (1, 'k1', ?, ?, 0, 0, ?, 0, '', 'x', ?, ?, ?)`,
				fmt.Sprintf("f%d", first), fmt.Sprintf("l%d", last), status, scan, first, last)
			return err
		})
	}
	write(t, s, func(tx *sql.Tx) error {
		scanSession(t, tx, "s1")
		return RequestScan(ctx, tx, testNow)
	})
	for _, c := range []struct {
		name        string
		scan        any
		first, last int
		status      string
		ok          bool
	}{
		{"scan batch", 1, 1, 3, "active", true},
		{"live batch of the same session", nil, 0, 0, "active", true},
		{"second active scan batch", 1, 4, 6, "active", false},
		{"same scan batch again", 1, 1, 3, "committed", false},
		{"next scan batch", 1, 4, 6, "committed", true},
	} {
		if err := add(c.scan, c.first, c.last, c.status); (err == nil) != c.ok {
			t.Errorf("%s: insert = %v, want ok %v", c.name, err, c.ok)
		}
	}
}
