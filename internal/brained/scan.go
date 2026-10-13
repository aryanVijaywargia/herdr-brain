package brained

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/secret"
	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/transcript"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

// The history scan at setup (§6.8, D16): brained reads your past Claude and Codex sessions
// once, imports their records as hook records would give them, and finds the repeats with the
// signal rules. It calls no model: the scan batches of the findings go through the extractor
// pass later.

const (
	// scanFiles and scanWindow pick the files: the newest 300 that changed in the 30 days before
	// the claim time, Claude and Codex together. [NEW] 300 and 30 days (§6.8).
	scanFiles  = 300
	scanWindow = 30 * 24 * time.Hour
	// argvLimit is the longest stored command in bytes (§6.7, §11: ≤ 4 KB), as for a hook record.
	argvLimit = 4096
	// scanTitle is the title of the notification at the end of the read. [NEW]
	scanTitle = "Brain history scan"
)

// ScanText is the text of the notification at the end of the read, from the report r and
// whether an extractor CLI is chosen (cli.ScanNotification). cmd/brain gives it, because
// internal/cli imports this package.
type ScanText func(r store.ScanReport, extractor bool) string

// historyScan runs the history scan of the store of a main binding. The fields are the outside
// world, so tests can change them.
type historyScan struct {
	environ []string // the brained environment: the CLI homes and herdr
	config  string   // the config file: the gates (§7.2)
	text    ScanText
	list    func(claudeHome, codexHome string) (transcript.Listing, error)
	read    func(f transcript.File, limit int64) (transcript.Session, error)
	sync    func(ctx context.Context, g Gate, st *store.Store) error // refreshes the workspace map of g
	notify  func(ctx context.Context, g Gate, body string) error
	now     func() time.Time
}

// newHistoryScan returns the history scan of brained with environment environ and the config
// file config.
func newHistoryScan(environ []string, config string, text ScanText) *historyScan {
	return &historyScan{environ: environ, config: config, text: text, list: transcript.List, read: readFile,
		now: time.Now,
		sync: func(ctx context.Context, g Gate, st *store.Store) error {
			return workspace.For(g.Binding).Sync(ctx, environ, st, time.Now())
		},
		notify: func(ctx context.Context, g Gate, body string) error {
			c, err := herdr.New(environ, herdr.Target{Session: g.Binding.Session, Socket: g.Binding.Socket})
			if err != nil {
				return err
			}
			return c.ShowNotification(ctx, scanTitle, body)
		}}
}

// readFile reads session file f up to limit bytes.
func readFile(f transcript.File, limit int64) (transcript.Session, error) {
	if f.Harness == transcript.Codex {
		return transcript.ReadCodex(f.Path, limit)
	}
	return transcript.ReadClaude(f.Path, limit)
}

// run claims the scan of the store of g and reads it, at a tick (§6.8 "Where it runs"). Only a
// main binding scans: a test binding never does (S87). With learning off for the store, the row
// waits. done says that this brained has nothing more to scan: the read ended, or the row is
// not one that it can claim. err alone leaves done false, so a later tick reads again from the
// start; a known capture identity adds nothing then (§6.4).
func (s *historyScan) run(ctx context.Context, g Gate) (done bool, err error) {
	if g.Binding.Kind != bindings.KindMain {
		return true, nil
	}
	st, err := store.Open(ctx, g.Binding.Store, busyTimeout)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	var sc store.Scan
	var claimed bool
	err = st.Write(ctx, func(tx *sql.Tx) error {
		on, err := store.LearningOnWholeStore(ctx, tx, true)
		if err != nil || !on {
			return err
		}
		if sc, claimed, err = store.ClaimScan(ctx, tx, g.Binding.Session, s.now()); err != nil || claimed {
			return err
		}
		row, ok, err := store.HistoryScan(ctx, tx)
		done = ok && row.Status != store.ScanPending
		return err
	})
	if err != nil || !claimed {
		return done, err
	}
	return s.scan(ctx, g, st, sc)
}

// scanRun is what one read of the scan keeps in memory: the counts of each CLI.
type scanRun struct {
	counts map[string]store.ScanCounts
}

// errPaused says that learning went off for the whole store during the read: the scan waits,
// and the row stays reading for a read from the start when learning is on again (§6.8).
var errPaused = errors.New("learning is off for the store")

// skip counts one session of harness that the scan did not read, with reason.
func (r *scanRun) skip(harness string, reason store.SkipReason) {
	c := r.counts[harness]
	if c.Skipped == nil {
		c.Skipped = map[store.SkipReason]int{}
	}
	c.Skipped[reason]++
	r.counts[harness] = c
}

// scan reads the files of claimed scan sc, imports each session in its own transaction, then
// stores the findings and ends the read with one herdr notification. It first refreshes the
// workspace map: a failed refresh stops it, so no session goes to a scope from an old map. done
// is false while the row still needs a read: a later run reads again from the start.
func (s *historyScan) scan(ctx context.Context, g Gate, st *store.Store, sc store.Scan) (done bool, err error) {
	gates, _, err := extract.ReadConfig(s.config)
	if err != nil {
		return false, fmt.Errorf("history scan: %w", err)
	}
	if err := s.sync(ctx, g, st); err != nil {
		return false, fmt.Errorf("history scan: workspace map: %w", err)
	}
	files, compressed, err := s.files(sc.Claimed)
	if err != nil {
		return false, err
	}
	r := &scanRun{counts: map[string]store.ScanCounts{}}
	for _, f := range files {
		c := r.counts[f.Harness]
		c.Found++
		r.counts[f.Harness] = c
	}
	for range compressed {
		r.skip(transcript.Codex, store.SkipCompressed)
	}
	err = st.Write(ctx, func(tx *sql.Tx) error { return store.SetScanCounts(ctx, tx, sc.ID, r.counts) })
	if err != nil {
		return false, err
	}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return false, err // brained stops: the next brained reads again
		}
		err := s.file(ctx, g, st, sc, f, r)
		if errors.Is(err, errPaused) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	body, err := s.end(ctx, st, sc, gates)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := s.notify(ctx, g, body); err != nil {
		return true, fmt.Errorf("history scan: notification: %w", err)
	}
	return true, nil
}

// files returns the files of the scan with claim time claimed, newest first, and the count of
// compressed Codex files (§6.8 "Which sessions"). A restart picks again with the same claim
// time.
func (s *historyScan) files(claimed time.Time) ([]transcript.File, int, error) {
	claude, codex, err := cliHomes(s.environ)
	if err != nil {
		return nil, 0, err
	}
	l, err := s.list(claude, codex)
	if err != nil {
		return nil, 0, err
	}
	from := claimed.Add(-scanWindow)
	files := slices.DeleteFunc(l.Files, func(f transcript.File) bool {
		return f.Changed.Before(from) || f.Changed.After(claimed)
	})
	return files[:min(len(files), scanFiles)], l.Compressed, nil
}

// cliHomes returns the Claude home (CLAUDE_CONFIG_DIR, else ~/.claude) and the Codex home
// (CODEX_HOME, else ~/.codex) of environ (§6.8).
func cliHomes(environ []string) (claude, codex string, err error) {
	claude, codex = lookup(environ, "CLAUDE_CONFIG_DIR"), lookup(environ, "CODEX_HOME")
	if claude != "" && codex != "" {
		return claude, codex, nil
	}
	home := lookup(environ, "HOME")
	if home == "" {
		return "", "", errors.New("history scan: HOME is not set")
	}
	return cmp.Or(claude, filepath.Join(home, ".claude")), cmp.Or(codex, filepath.Join(home, ".codex")), nil
}

// file reads session file f up to its size now and imports its session in one transaction
// with the new counts. A file with an unknown shape is skipped and counted: the reader fails
// closed (§6.6).
func (s *historyScan) file(ctx context.Context, g Gate, st *store.Store, sc store.Scan, f transcript.File, r *scanRun) error {
	info, err := os.Stat(f.Path)
	if err != nil {
		return fmt.Errorf("history scan: %w", err)
	}
	ses, err := s.read(f, info.Size())
	unknown := errors.Is(err, transcript.ErrUnknownFormat)
	if err != nil && !unknown {
		return fmt.Errorf("history scan: %w", err)
	}
	return st.Write(ctx, func(tx *sql.Tx) error {
		if on, err := store.LearningOnWholeStore(ctx, tx, true); err != nil || !on {
			return cmp.Or(err, errPaused)
		}
		reason := store.SkipUnknownFormat
		if !unknown {
			if reason, err = s.add(ctx, tx, g, sc, f.Harness, ses); err != nil {
				return err
			}
		}
		if reason != "" {
			r.skip(f.Harness, reason)
		} else {
			c := r.counts[f.Harness]
			c.Read++
			r.counts[f.Harness] = c
		}
		return store.SetScanCounts(ctx, tx, sc.ID, r.counts)
	})
}

// add imports session ses of harness in tx, or returns why the scan skips it (§6.8): not
// interactive, a fork, known from capture or hook records, a shared checkout, or learning off
// for its workspace. It reads the workspaces in tx, so a change during a slow read applies.
func (s *historyScan) add(ctx context.Context, tx *sql.Tx, g Gate, sc store.Scan, harness string,
	ses transcript.Session) (store.SkipReason, error) {
	switch {
	case !ses.Interactive || ses.SideChain:
		return store.SkipNotInteractive, nil
	case ses.Fork:
		return store.SkipFork, nil
	}
	known, err := store.KnownSession(ctx, tx, harness, ses.ID)
	if err != nil || known {
		return store.SkipKnown, err
	}
	wss, err := store.ScanWorkspaces(ctx, tx)
	if err != nil {
		return "", err
	}
	key, binding, ok := scope(ses.CWD, wss, g.Binding.Session)
	if !ok {
		return store.SkipSharedCheckout, nil
	}
	on, err := store.LearningOn(ctx, tx, true, binding, key, "")
	if err != nil || !on {
		return store.SkipLearningOff, err
	}
	first, last := cmp.Or(ses.First, sc.Claimed), cmp.Or(ses.Last, sc.Claimed)
	session, segment, err := store.ScanSession(ctx, tx, store.SessionKey{HerdrSession: binding, Harness: harness,
		Kind: store.KeyAgent, ID: ses.ID}, key, first, last)
	if err != nil {
		return "", err
	}
	imp := importer{tx: tx, harness: harness, segment: segment, cwd: ses.CWD, at: last}
	turns, err := imp.turns(ctx, ses.Turns)
	if err != nil {
		return "", err
	}
	if err := store.AddScanTurns(ctx, tx, sc.ID, turns); err != nil {
		return "", err
	}
	if imp.last == 0 {
		return "", nil
	}
	// The watermark at the end of the session: the normal pass makes no batch of it (§6.8).
	return "", store.MoveWatermark(ctx, tx, store.Batch{SessionID: session, WorkspaceKey: key,
		LastObservation: imp.last, LastCapture: imp.lastIdentity})
}

// scope returns the workspace key and the binding of a session with cwd (§6.8 "Scope of a
// session"): the one live workspace whose project folder holds cwd, with the binding of its
// herdr session; the global pool ("") with binding runner when none does. ok is false for a
// cwd in two or more workspaces, a shared checkout.
func scope(cwd string, wss []store.Workspace, runner string) (key, binding string, ok bool) {
	if canon, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = canon
	}
	var in []store.Workspace
	for _, w := range wss {
		if cwd != "" && within(*w.RootFolder, cwd) {
			in = append(in, w)
		}
	}
	switch len(in) {
	case 0:
		return "", runner, true
	case 1:
		return in[0].Key, in[0].HerdrSession, true
	}
	return "", "", false
}

// within reports whether path is the folder dir or lies beneath it.
func within(dir, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// importer stores the records of one scanned session in tx.
type importer struct {
	tx           *sql.Tx
	harness      string
	segment      int64
	cwd          string    // the session cwd: the files give no cwd for each command
	at           time.Time // the time of a record that has none
	last         int64     // the newest observation id of the session
	lastIdentity string
}

// turns stores the records of each turn of ts and returns the scan turns, each id once.
func (imp *importer) turns(ctx context.Context, ts []transcript.Turn) ([]store.ScanTurn, error) {
	var turns []store.ScanTurn
	at := map[int64]int{} // a turn id → its place in turns
	for _, t := range ts {
		id, err := imp.turn(ctx, t)
		if err != nil {
			return nil, err
		}
		if id == 0 {
			continue // nothing of the turn is stored: forget removed it, or it held no text
		}
		// A record that the file repeats gives the same turn id again; the reader counts the
		// usage of each assistant message once, so the tokens of both parts add up.
		if i, ok := at[id]; ok {
			turns[i].Tokens += t.Tokens
			turns[i].Known = turns[i].Known || t.TokensKnown
			continue
		}
		at[id] = len(turns)
		turns = append(turns, store.ScanTurn{ID: id, Tokens: t.Tokens, Known: t.TokensKnown})
	}
	return turns, nil
}

// turn stores the records of turn t (§6.8 "What Brain takes"): the prompt, each shell command
// with its command record and the failure excerpt of a failed one, and the last assistant
// text. It returns the turn id: the id of its first stored observation, 0 when none is stored.
// A session stores its records in file order, so each later observation of the turn has a
// higher id (turnsOf).
func (imp *importer) turn(ctx context.Context, t transcript.Turn) (int64, error) {
	var ids []int64
	add := func(id int64, err error) error {
		if id != 0 {
			ids = append(ids, id)
		}
		return err
	}
	if err := add(imp.text(ctx, t.Prompt, adapter.EventPrompt, store.SenderUnconfirmed, true)); err != nil {
		return 0, err
	}
	for _, c := range t.Commands {
		if err := add(imp.command(ctx, c)); err != nil {
			return 0, err
		}
		if c.Failure != nil {
			// The exit status of the command record holds the failure when the file has one: the
			// output is detail of the same occurrence, not a second one (§6.4).
			if err := add(imp.text(ctx, *c.Failure, adapter.EventTool, store.SenderTool, c.Exit == nil)); err != nil {
				return 0, err
			}
		}
	}
	if t.Reply.ID != "" {
		if err := add(imp.text(ctx, t.Reply, adapter.EventStop, store.SenderAssistant, true)); err != nil {
			return 0, err
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return ids[0], nil
}

// observation stores o, or finds it when a first read stored it. It returns its id, and
// whether this call stored it; the id is 0 when forget removed it (§6.7): its identity is in
// the forget tombstones, so it stays out.
func (imp *importer) observation(ctx context.Context, o store.Observation) (int64, bool, error) {
	if o.At.IsZero() {
		o.At = imp.at
	}
	id, err := store.AddObservation(ctx, imp.tx, o)
	if err != nil {
		return 0, false, err
	}
	added := id != 0
	if !added {
		if id, err = store.ObservationID(ctx, imp.tx, o.CaptureIdentity); err != nil {
			return 0, false, err
		}
	}
	if id > imp.last {
		imp.last, imp.lastIdentity = id, o.CaptureIdentity
	}
	return id, added, nil
}

// base is the observation of record rec of the session: its capture identity is (harness,
// record id) (§6.8).
func (imp *importer) base(rec transcript.Text, event string, sender store.Sender) store.Observation {
	return store.Observation{CaptureIdentity: imp.harness + ":" + rec.ID, Source: store.SourceTranscript,
		SegmentID: imp.segment, Event: event, Status: store.ObservationComplete, Sender: sender, Counts: true, At: rec.At}
}

// text stores the text of record rec after the capture filters (HookText) and returns its id.
// A prompt gets its prompt index row (E14-S1). counts is false for text that adds detail to an
// occurrence that another record holds.
func (imp *importer) text(ctx context.Context, rec transcript.Text, event string, sender store.Sender, counts bool) (int64, error) {
	o := imp.base(rec, event, sender)
	o.Counts = counts
	o.Excerpt, o.Truncated, o.BrainOrigin = HookText(imp.harness, rec.Text)
	id, added, err := imp.observation(ctx, o)
	if err != nil || !added || event != adapter.EventPrompt {
		return id, err
	}
	o.ID = id
	return id, IndexPrompt(ctx, imp.tx, o, HookPrompt(imp.harness, rec.Text))
}

// command stores shell command c as a tool observation with its command record (§6.7): a
// command in which the secret filter finds a secret is not stored, and the record says only
// that it had one.
func (imp *importer) command(ctx context.Context, c transcript.Command) (int64, error) {
	o := imp.base(transcript.Text{ID: c.ID, At: c.At}, adapter.EventTool, store.SenderTool)
	rec := store.CommandRecord{CWD: imp.cwd, ExitStatus: c.Exit}
	if len(secret.Find(c.Command)) > 0 {
		rec.SecretFound = true
	} else {
		o.Excerpt, o.Truncated, o.BrainOrigin = HookText(imp.harness, c.Command)
		rec.Argv = c.Command
		if len(rec.Argv) > argvLimit {
			rec.Argv, rec.Truncated = cutBytes(rec.Argv, argvLimit), true
		}
	}
	id, added, err := imp.observation(ctx, o)
	if err != nil || !added {
		return id, err
	}
	rec.ObservationID = id
	return id, store.AddCommandRecord(ctx, imp.tx, rec)
}

// cutBytes returns the longest prefix of s of at most n bytes that ends at a character
// boundary.
func cutBytes(s string, n int) string {
	return strings.ToValidUTF8(s[:n], "")
}

// end stores the findings of the read and moves scan sc to extracting, in one transaction, and
// returns the text of the notification (§6.8): the scan proposals wait for the extractor pass,
// and no review pane opens.
func (s *historyScan) end(ctx context.Context, st *store.Store, sc store.Scan, gates store.Gates) (string, error) {
	var body string
	err := st.Write(ctx, func(tx *sql.Tx) error {
		ins, err := store.SignalInputs(ctx, tx)
		if err != nil {
			return err
		}
		turns, err := store.ScanTurns(ctx, tx, sc.ID)
		if err != nil {
			return err
		}
		// The findings come from the records of the scan only: a session can resume in a pane,
		// and its live evidence takes the live path.
		ins = slices.DeleteFunc(ins, func(in store.SignalInput) bool { return in.Source != store.SourceTranscript })
		if err := store.SetScanFindings(ctx, tx, sc.ID, findings(ins, turns, store.ScanGates(gates))); err != nil {
			return err
		}
		if err := store.SetScanStatus(ctx, tx, sc.ID, store.ScanExtracting, s.now()); err != nil {
			return err
		}
		rep, err := store.HistoryScanReport(ctx, tx, sc.ID, store.ScanGates(gates))
		if err != nil {
			return err
		}
		x, err := store.Extractor(ctx, tx)
		body = s.text(rep, x.CLI != "")
		return err
	})
	return body, err
}

// turnsOf returns the turn of each observation of ins: the newest turn of its session at or
// before it. A turn id is the id of the first stored observation of the turn (importer.turn).
func turnsOf(ins []store.SignalInput, turns map[int64]*int64) map[int64]int64 {
	bySession := map[int64][]int64{}
	for _, in := range ins {
		bySession[in.SessionID] = append(bySession[in.SessionID], in.ID)
	}
	of := map[int64]int64{}
	for _, ids := range bySession {
		slices.Sort(ids)
		var turn int64
		for _, id := range ids {
			if _, ok := turns[id]; ok {
				turn = id
			}
			of[id] = turn
		}
	}
	return of
}

// findings returns the findings of the scanned observations ins (§6.8 "Repeats with no model
// call"): the signal rules run over the observations of each scope apart, and a finding stays
// only when it reaches the scan gate of its kind. The list is most tokens first, and the id of
// a finding is its place in it.
func findings(ins []store.SignalInput, turns map[int64]*int64, gates store.Gates) []store.Finding {
	turnOf := turnsOf(ins, turns)
	byScope := map[string][]store.SignalInput{}
	for _, in := range ins {
		byScope[in.WorkspaceKey] = append(byScope[in.WorkspaceKey], in)
	}
	type ranked struct {
		f      store.Finding
		tokens int64
	}
	var fs []ranked
	for _, scope := range slices.Sorted(maps.Keys(byScope)) {
		sessionOf := map[int64]int64{}
		identity := map[int64]string{}
		for _, in := range byScope[scope] {
			sessionOf[in.ID], identity[in.ID] = in.SessionID, in.CaptureIdentity
		}
		for _, g := range groups(byScope[scope]) {
			sessions := map[int64]bool{}
			f := store.Finding{Kind: g.kind, Scope: scope}
			var t store.TurnTokens
			seen := map[int64]bool{}
			for _, id := range g.obs {
				sessions[sessionOf[id]] = true
				turn := turnOf[id]
				f.Obs = append(f.Obs, store.FindingObs{Observation: id, Identity: identity[id], Turn: turn})
				if !seen[turn] {
					seen[turn] = true
					if n := turns[turn]; n != nil {
						t.Tokens += *n
					}
				}
			}
			if gates.Reached(store.Candidate{Kind: g.kind, Count: len(g.obs), Sessions: len(sessions)}) {
				fs = append(fs, ranked{f, t.Tokens})
			}
		}
	}
	slices.SortStableFunc(fs, func(a, b ranked) int { return cmp.Compare(b.tokens, a.tokens) })
	out := make([]store.Finding, len(fs))
	for i, rf := range fs {
		out[i] = rf.f
		out[i].ID = i + 1
	}
	return out
}

// group is the observations of one repeat of one scope, by id, and its finding kind.
type group struct {
	kind store.CandidateKind
	obs  []int64
}

// groups runs the signal rules over ins, the observations of one scope in time order, and
// joins the signals of one repeat (§7.1, §6.8):
//   - a lesson in plain words is a repeat of its own (gate 1);
//   - a failure and a command chain join by their key;
//   - text that repeats earlier text joins its earlier text. The correction rule and the
//     repeated-prompt rule find the same repeats there, so a repeat is a correction when one of
//     its texts holds a correction marker, a lesson when one gives a lesson in plain words, else
//     a repeated prompt [NEW]. A correction marker with no repeat stays one occurrence, below its
//     gate;
//   - a procedure joins the earlier procedures that it is like. A procedure whose texts are all
//     in one text repeat is that repeat, and a lesson in a text repeat is part of it.
func groups(ins []store.SignalInput) []group {
	byID := map[int64]store.SignalInput{}
	for _, in := range ins {
		byID[in.ID] = in
	}
	text, proc := union{}, union{}
	var lessons []int64
	var keys []string
	keyed := map[string]*group{}
	for _, s := range signal.Find(ins) {
		switch s.Kind {
		case signal.Correction, signal.RepeatedPrompt:
			text.join(s.Observations)
		case signal.Procedure:
			proc.join(s.Observations)
		case signal.Lesson:
			lessons = append(lessons, s.Observations...)
		case signal.Failure, signal.CommandChain:
			k := string(s.Kind) + "/" + s.Key
			if keyed[k] == nil {
				keys = append(keys, k)
				keyed[k] = &group{kind: store.CandidateKind(s.Kind)}
			}
			keyed[k].obs = append(keyed[k].obs, s.Observations...)
		}
	}
	gs, repeatOf := textRepeats(text.sets(), byID, lessons)
	for _, set := range proc.sets() {
		r, ok := repeatOf[set[0]]
		if !ok || slices.ContainsFunc(set, func(id int64) bool { i, ok := repeatOf[id]; return !ok || i != r }) {
			gs = append(gs, group{kind: store.CandidateProcedure, obs: set})
		}
	}
	for _, id := range lessons {
		if _, ok := repeatOf[id]; !ok {
			gs = append(gs, group{kind: store.CandidateLesson, obs: []int64{id}})
		}
	}
	for _, k := range keys {
		g := keyed[k]
		slices.Sort(g.obs)
		gs = append(gs, group{kind: g.kind, obs: slices.Compact(g.obs)})
	}
	return gs
}

// textRepeats returns the finding of each set of texts that repeat each other (groups), and
// the index of its repeat for each text in a set of two or more.
func textRepeats(sets [][]int64, byID map[int64]store.SignalInput, lessons []int64) ([]group, map[int64]int) {
	var gs []group
	repeatOf := map[int64]int{}
	for _, set := range sets {
		kind := store.FindingPrompt
		switch {
		case slices.ContainsFunc(set, func(id int64) bool { return hasMarker(byID[id]) }):
			kind = store.CandidateCorrection
		case slices.ContainsFunc(set, func(id int64) bool { return slices.Contains(lessons, id) }):
			kind = store.CandidateLesson
		}
		if len(set) > 1 {
			for _, id := range set {
				repeatOf[id] = len(gs)
			}
		}
		gs = append(gs, group{kind: kind, obs: set})
	}
	return gs, repeatOf
}

// hasMarker reports whether text in holds a correction marker: the correction rule finds a
// correction in it alone, with no earlier text to repeat.
func hasMarker(in store.SignalInput) bool {
	return slices.ContainsFunc(signal.Find([]store.SignalInput{in}), func(s signal.Signal) bool {
		return s.Kind == signal.Correction
	})
}

// union joins sets of observation ids.
type union map[int64]int64

// root returns the root of the set of id.
func (u union) root(id int64) int64 {
	for {
		p, ok := u[id]
		if !ok || p == id {
			return id
		}
		id = p
	}
}

// join puts ids in one set.
func (u union) join(ids []int64) {
	for _, id := range ids {
		if _, ok := u[id]; !ok {
			u[id] = id
		}
		u[u.root(id)] = u.root(ids[0])
	}
}

// sets returns the sets, each by id, in the order of their first id.
func (u union) sets() [][]int64 {
	by := map[int64][]int64{}
	for _, id := range slices.Sorted(maps.Keys(u)) {
		r := u.root(id)
		by[r] = append(by[r], id)
	}
	sets := slices.Collect(maps.Values(by))
	slices.SortFunc(sets, func(a, b []int64) int { return cmp.Compare(a[0], b[0]) })
	return sets
}
