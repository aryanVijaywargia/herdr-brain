package extract

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// scanHelp tells the extractor what a scan batch is (§6.8 "Proposals"): repeats of past
// sessions, from which Brain proposes only items, skills and tools (D12 stays open, S95).
const scanHelp = "These signals come from the user's past sessions, and each one repeats there. Return only facts, " +
	"preferences, lessons, procedures, skills and tools. A repeat that fits a hook best, such as \"run the formatter " +
	"after each edit\", becomes a lesson or a tool: Brain proposes no hook, allow rule or automation from it.\n"

// globalHelp tells the extractor that the sessions of the global pool belong to no project, so
// Brain keeps only a claim that it marks global (§6.8 "Global pool", S90).
const globalHelp = "These sessions belong to no project: return only a claim that holds in every project, with " +
	"scope_guess global, and no skill or tool.\n"

// scanJobs returns every scan batch that waits (§6.8 "Proposals"): for each scope of the history
// scan in its extracting state, the findings after the progress of the scope, in finding order,
// packed into batches up to the input byte cap. A finding is one that still reaches the gate of
// its kind with the observations that still exist (store.HistoryScanReport). An exact command
// chain goes to the exact tool path instead (exactChain). The batches of a
// scope follow each other: the start of each is the last finding of the one before, so a later
// batch waits until the earlier one commits. It returns the scan too; ok is false when there is
// no scan in its extracting state.
func (p Pass) scanJobs(ctx context.Context, tx *sql.Tx) (scan store.Scan, jobs []job, ok bool, err error) {
	s, ok, err := store.HistoryScan(ctx, tx)
	if err != nil || !ok || s.Status != store.ScanExtracting {
		return store.Scan{}, nil, false, err
	}
	r, err := store.HistoryScanReport(ctx, tx, s.ID, store.ScanGates(p.gates()))
	if err != nil {
		return store.Scan{}, nil, false, err
	}
	ins, err := store.SignalInputs(ctx, tx)
	if err != nil {
		return store.Scan{}, nil, false, err
	}
	byID := map[int64]store.SignalInput{}
	for _, in := range ins {
		byID[in.ID] = in
	}
	sessions, err := store.Sessions(ctx, tx)
	if err != nil {
		return store.Scan{}, nil, false, err
	}
	byScope := map[string][]store.Finding{}
	for _, rep := range r.Repeats {
		byScope[rep.Finding.Scope] = append(byScope[rep.Finding.Scope], rep.Finding)
	}
	for _, scope := range slices.Sorted(maps.Keys(byScope)) {
		start, err := store.ScanProgress(ctx, tx, s.ID, scope)
		if err != nil {
			return store.Scan{}, nil, false, err
		}
		fs := slices.SortedFunc(slices.Values(byScope[scope]), func(a, b store.Finding) int { return a.ID - b.ID })
		fs = slices.DeleteFunc(fs, func(f store.Finding) bool { return f.ID <= start || exactChain(f, byID) })
		for len(fs) > 0 {
			n := p.fitFindings(fs, byID)
			jobs = append(jobs, scanJob(s.ID, scope, start, fs[:n], byID, sessions))
			start, fs = fs[n-1].ID, fs[n:]
		}
	}
	return s, jobs, true, nil
}

// exactChain reports whether finding f is a command chain of a workspace that the exact tool path
// drafts with no model call (§6.8, exactTools): in each session the same commands, with no
// failure, that give one exact draft. Such a finding goes into no scan batch.
func exactChain(f store.Finding, byID map[int64]store.SignalInput) bool {
	if f.Kind != store.CandidateCommandChain || f.Scope == "" {
		return false
	}
	bySession := map[int64][]store.CommandRecord{}
	var order []int64
	for _, o := range f.Obs {
		in := byID[o.Observation]
		if in.Command == nil || in.Command.ExitStatus != nil && *in.Command.ExitStatus != 0 {
			return false
		}
		if _, ok := bySession[in.SessionID]; !ok {
			order = append(order, in.SessionID)
		}
		bySession[in.SessionID] = append(bySession[in.SessionID], *in.Command)
	}
	var first string
	for i, sid := range order {
		text, err := exactDraft(bySession[sid])
		if err != nil || i > 0 && text != first {
			return false
		}
		first = text
	}
	return true
}

// scanPlan returns the scan batches that this pass extracts now, after the live batches: those
// of the scopes of its herdr session binding that may call now (batch), in round robin order of
// the scopes, each scope in finding order (§7.3 queue). A scope belongs to the binding of the
// session of its first finding: the scan gives a workspace session the binding of its workspace,
// and a global pool session the binding of the brained that ran the scan (§6.8). An active scan
// batch of the binding was left by a crash, and is discarded first, also when the binding has no
// scan batch left. When no scan batch is left in any scope, every one is committed: the scan is
// done.
func (p Pass) scanPlan(ctx context.Context, tx *sql.Tx) ([]job, error) {
	s, all, ok, err := p.scanJobs(ctx, tx)
	if err != nil || !ok {
		return nil, err
	}
	if err := store.DiscardScanBatches(ctx, tx, s.ID, p.HerdrSession, p.Now()); err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, store.SetScanStatus(ctx, tx, s.ID, store.ScanDone, p.Now())
	}
	var jobs []job
	for _, j := range all {
		if j.session.Key.HerdrSession != p.HerdrSession {
			continue
		}
		ok, err := p.batch(ctx, tx, j)
		if err != nil {
			return nil, err
		}
		if ok {
			jobs = append(jobs, j)
		}
	}
	last, err := store.LastCalls(ctx, tx)
	return roundRobin(jobs, func(j job) string { return j.batch.WorkspaceKey }, last), err
}

// scanRoom reports whether a scan batch may call now, with d the use of the daily caps: live
// batches go first, so the caps keep room for one call and one proposal of each live batch of
// every binding that may call now (S93). The caps cover the whole store.
func (p Pass) scanRoom(ctx context.Context, tx *sql.Tx, d Day) (bool, error) {
	runs, sigs, byID, err := p.finished(ctx, tx, "")
	if err != nil {
		return false, err
	}
	live := 0
	for _, r := range runs {
		j := p.job(r, sigs, byID)
		if len(j.refs) == 0 {
			continue
		}
		why, _, err := p.why(ctx, tx, j, d)
		if err != nil {
			return false, err
		}
		if why == "" {
			live++
		}
	}
	return d.Calls+live < d.Caps.Calls && d.Proposals+live < d.Caps.Proposals, nil
}

// fitFindings returns how many of findings fs, from the first, fit in the input cap with their
// excerpts (§7.3). A finding stays whole, and the first one goes in also when it alone is
// larger, as one long signal of a live batch does.
func (p Pass) fitFindings(fs []store.Finding, byID map[int64]store.SignalInput) int {
	size := 0
	seen := map[int64]bool{}
	for i, f := range fs {
		for _, o := range f.Obs {
			if !seen[o.Observation] {
				seen[o.Observation] = true
				size += len(inputText(byID[o.Observation]))
			}
		}
		if size > p.caps().InputBytes && i > 0 {
			return i
		}
	}
	return len(fs)
}

// scanJob is the scan batch of findings fs of scope in scan id, whose scope progress was start
// before it. Its session is the session of the first observation of its first finding.
func scanJob(id int64, scope string, start int, fs []store.Finding, byID map[int64]store.SignalInput, sessions []store.Session) job {
	rs := scanRefs(fs, byID)
	first, last := fs[0], fs[len(fs)-1]
	sid := byID[first.Obs[0].Observation].SessionID
	i := slices.IndexFunc(sessions, func(s store.Session) bool { return s.ID == sid })
	var newest time.Time
	for _, r := range rs {
		if at := byID[r.obs].At; at.After(newest) {
			newest = at
		}
	}
	return job{session: sessions[i], refs: rs, newest: newest, batch: store.Batch{ScanID: id, SessionID: sid,
		WorkspaceKey: scope, FirstCapture: fmt.Sprintf("scan:%d", first.ID), LastCapture: fmt.Sprintf("scan:%d", last.ID),
		StartWatermark: int64(start), FirstFinding: first.ID, LastFinding: last.ID}}
}

// scanRefs returns the input of the findings fs of a scan batch, by observation id, as refs does
// for the signals of a live run: the kind of its first finding, the lesson mark, the command
// chain, and the other tellings of a procedure or a repeated prompt as its earlier occurrences
// (#314), so the tellings that the extractor does not cite still count.
func scanRefs(fs []store.Finding, byID map[int64]store.SignalInput) []ref {
	at := map[int64]int{}
	var rs []ref
	for _, f := range fs {
		kind := findingKind(f.Kind)
		obs := make([]int64, len(f.Obs))
		for i, o := range f.Obs {
			obs[i] = o.Observation
		}
		for _, id := range obs {
			if i, ok := at[id]; ok {
				rs[i].lesson = rs[i].lesson || kind == signal.Lesson
				continue
			}
			o := byID[id]
			r := ref{id: fmt.Sprintf("e%d", id), obs: id, identity: o.CaptureIdentity, workspace: o.WorkspaceKey,
				sender: o.Sender, excerpt: inputText(o), kind: kind, lesson: kind == signal.Lesson, cmd: o.Command}
			switch kind {
			case signal.CommandChain:
				r.chain = obs
			case signal.Procedure, signal.RepeatedPrompt:
				r.earlier = occurrences(slices.DeleteFunc(slices.Clone(obs), func(x int64) bool { return x == id }),
					o.WorkspaceKey, byID)
			}
			at[id] = len(rs)
			rs = append(rs, r)
		}
	}
	slices.SortFunc(rs, func(a, b ref) int { return int(a.obs - b.obs) })
	return rs
}

// findingKind is the signal kind of a finding of kind k: a finding has the kind of a candidate,
// and a repeated prompt its own (store.FindingPrompt).
func findingKind(k store.CandidateKind) signal.Kind {
	if k == store.FindingPrompt {
		return signal.RepeatedPrompt
	}
	return signal.Kind(k)
}

// scanDrop returns why scan batch j drops checked claim c, or "" (§6.8 "Global pool", S90): the
// global pool keeps only an item claim that the extractor marks global. A skill and a tool
// always have a workspace, and a claim about one project has no workspace to go to.
func scanDrop(c Claim, j job) string {
	if !j.batch.Scan() || j.batch.WorkspaceKey != "" {
		return ""
	}
	switch {
	case c.Skill != nil || c.Tool != "":
		return "the global pool keeps no skill or tool"
	case c.Scope != store.ScopeGlobal:
		return "the global pool keeps no claim about one project"
	}
	return ""
}
