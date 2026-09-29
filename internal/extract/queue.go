package extract

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// Caps are the limits of extraction (§7.3 caps). A later config can set them; a Pass with the
// zero value uses DefaultCaps.
type Caps struct {
	Calls      int // extractor calls in one day
	Proposals  int // new proposals in one day
	InputBytes int // the excerpt bytes of one call
}

// DefaultCaps are the §7.3 defaults: 10 calls and 5 new proposals a day. [NEW] 32 KiB of
// excerpts for each call: the prompt is one argument of the CLI, and it stays far under the
// argument limit of macOS and Linux (1 MiB and more).
func DefaultCaps() Caps { return Caps{Calls: 10, Proposals: 5, InputBytes: 32 << 10} }

// day is the window of the daily caps. [NEW] the 24 hours before now: it needs no time zone.
const day = 24 * time.Hour

func (p Pass) caps() Caps {
	if p.Caps == (Caps{}) {
		return DefaultCaps()
	}
	return p.Caps
}

// Day is the use of the daily caps: the extractor calls and new proposals of the last day.
type Day struct {
	Calls, Proposals int
	Caps             Caps
}

// Day returns the use of the daily caps now.
func (p Pass) Day(ctx context.Context, tx *sql.Tx) (Day, error) {
	since := p.Now().Add(-day)
	calls, err := store.CallsSince(ctx, tx, since)
	if err != nil {
		return Day{}, err
	}
	proposals, err := store.ExtractorProposalsSince(ctx, tx, since)
	if err != nil {
		return Day{}, err
	}
	return Day{Calls: calls, Proposals: proposals, Caps: p.caps()}, nil
}

// Full returns why the caps stop a new call now, or "". A call is useless when no new
// proposal may come of it, so the proposal cap stops calls too.
func (d Day) Full() string {
	switch {
	case d.Calls >= d.Caps.Calls:
		return fmt.Sprintf("the daily cap of %d extractor calls is reached", d.Caps.Calls)
	case d.Proposals >= d.Caps.Proposals:
		return fmt.Sprintf("the daily cap of %d new proposals is reached", d.Caps.Proposals)
	}
	return ""
}

// order puts jobs in round robin order across workspace keys (§7.3 queue): the first job of
// each key, then the second of each key, and so on. The jobs of a key come oldest first. The
// key whose last call (from last, which covers the whole store) is oldest comes first, a key
// with no call before all, so under the call cap each key gets its turn over the passes, also
// against the keys of another herdr session that shares the store.
func order(jobs []job, last map[string]time.Time) []job {
	byKey := map[string][]job{}
	var keys []string
	for _, j := range jobs {
		k := j.batch.WorkspaceKey
		if _, ok := byKey[k]; !ok {
			keys = append(keys, k)
		}
		byKey[k] = append(byKey[k], j)
	}
	oldest := func(a, b job) int { return cmp.Compare(a.refs[0].obs, b.refs[0].obs) }
	for _, k := range keys {
		slices.SortFunc(byKey[k], oldest)
	}
	slices.SortFunc(keys, func(a, b string) int {
		return cmp.Or(last[a].Compare(last[b]), oldest(byKey[a][0], byKey[b][0]))
	})
	out := make([]job, 0, len(jobs))
	for i := 0; len(out) < len(jobs); i++ {
		for _, k := range keys {
			if i < len(byKey[k]) {
				out = append(out, byKey[k][i])
			}
		}
	}
	return out
}

// fit cuts run so that the excerpts of its refs rs fit in the input cap (§7.3): the cut part
// goes into a later batch, after the watermark moves. The cut never splits a signal of sigs
// that starts in run: all its observations stay on one side, so the later batch still sees
// the whole signal. A run that no such cut makes fit stays whole, so one long signal does not
// stop its session.
func (p Pass) fit(run []store.SignalInput, rs []ref, sigs []signal.Signal) ([]store.SignalInput, []ref) {
	size := 0
	for _, r := range rs {
		size += len(r.excerpt)
		if size <= p.caps().InputBytes {
			continue
		}
		cut := split(r.obs, run[0].ID, sigs)
		i := slices.IndexFunc(run, func(o store.SignalInput) bool { return o.ID >= cut })
		if i <= 0 {
			return run, rs
		}
		return run[:i], slices.DeleteFunc(slices.Clone(rs), func(r ref) bool { return r.obs >= cut })
	}
	return run, rs
}

// split moves the cut before observation cut back until no signal of sigs that starts at or
// after first has observations on both sides of it.
func split(cut, first int64, sigs []signal.Signal) int64 {
	for moved := true; moved; {
		moved = false
		for _, s := range sigs {
			lo, hi := slices.Min(s.Observations), slices.Max(s.Observations)
			if lo >= first && lo < cut && cut <= hi {
				cut, moved = lo, true
			}
		}
	}
	return cut
}

// Waiting is the extraction work of one workspace key that waits in the queue (§7.3).
type Waiting struct {
	WorkspaceKey string
	Batches      int
	Oldest       time.Time // the time of the oldest signal that waits
	Why          string    // why the next pass makes no call for it; "" when it may
	NoCLI        bool      // no extractor CLI is chosen
	Failed       string    // the stored reason of a failed or invalid last call; "" for none
}

// Waiting returns the work that waits for extraction, by workspace key: the batches with a
// signal of the finished sessions of the pass. It only reads.
func (p Pass) Waiting(ctx context.Context, tx *sql.Tx) ([]Waiting, error) {
	runs, sigs, byID, err := p.finished(ctx, tx)
	if err != nil {
		return nil, err
	}
	d, err := p.Day(ctx, tx)
	if err != nil {
		return nil, err
	}
	var ws []Waiting
	for _, r := range runs {
		j := p.job(r, sigs, byID)
		if len(j.refs) == 0 {
			continue
		}
		why, b, err := p.why(ctx, tx, j, d)
		if err != nil {
			return nil, err
		}
		key := j.batch.WorkspaceKey
		i := slices.IndexFunc(ws, func(w Waiting) bool { return w.WorkspaceKey == key })
		if i < 0 {
			ws = append(ws, Waiting{WorkspaceKey: key})
			i = len(ws) - 1
		}
		w := &ws[i]
		w.Batches++
		w.NoCLI = why == noCLI
		if w.Failed == "" && (b.Status == store.BatchFailed || b.Status == store.BatchInvalid) {
			w.Failed = b.Reason
		}
		if at := byID[j.refs[0].obs].At; w.Oldest.IsZero() || at.Before(w.Oldest) {
			w.Oldest = at
		}
		if why != "" && !strings.Contains(w.Why, why) {
			w.Why = strings.TrimPrefix(w.Why+"; "+why, "; ")
		}
	}
	slices.SortFunc(ws, func(a, b Waiting) int { return cmp.Compare(a.WorkspaceKey, b.WorkspaceKey) })
	return ws, nil
}

// noCLI is why work waits in a store with no chosen extractor CLI, such as an M1 store.
const noCLI = "no extractor CLI is chosen; run brain setup"

// why returns why the next pass makes no call for batch j, or "", and the stored batch of j:
// the switches and the CLI of allowed, a failure of its last call, a call that runs, or the
// caps (S15, §7.3). A failure says the reason that the batch stored; the official CLI reports
// its own sign-in failures.
func (p Pass) why(ctx context.Context, tx *sql.Tx, j job, d Day) (string, store.Batch, error) {
	s, err := store.Extractor(ctx, tx)
	if err != nil {
		return "", store.Batch{}, err
	}
	if s.CLI == "" {
		return noCLI, store.Batch{}, nil
	}
	if on, err := store.ExtractionOn(ctx, tx, j.batch.WorkspaceKey); err != nil || !on {
		return "extraction is off", store.Batch{}, err
	}
	if on, err := store.LearningOn(ctx, tx, p.DefaultOn, j.session.Key.HerdrSession, j.batch.WorkspaceKey,
		j.session.PaneID); err != nil || !on {
		return "learning is off", store.Batch{}, err
	}
	b, err := store.FindBatch(ctx, tx, j.batch)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", store.Batch{}, err
	case b.Status == store.BatchFailed || b.Status == store.BatchInvalid:
		return fmt.Sprintf("the last %s call %s: %s", s.CLI, b.Status, b.Reason), b, nil
	case b.Status == store.BatchActive:
		return "a call runs, or a crash stopped it and the next pass tries again", b, nil
	}
	return d.Full(), b, nil
}
