package brained

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// mainSubs returns the subscriptions of the main stream. herdr 0.9.1 takes no pane for
// pane.exited and pane.closed, so brained gets them for all panes here.
func mainSubs() []herdr.Subscription {
	return []herdr.Subscription{
		{Type: herdr.PaneCreated}, {Type: herdr.PaneAgentDetected}, {Type: herdr.PaneExited}, {Type: herdr.PaneClosed},
	}
}

// daemon is one brained process.
type daemon struct {
	env          env
	sock         herdr.Socket
	bindingsFile string
	handler      Handler
	log          io.Writer
	tick         <-chan time.Time // each tick checks the plugin, the binding and the learning switches
	tries        int
	pause        time.Duration
	queue        int
	environ      []string                                                    // the brained environment; a launch process gets it with the pane's herdr variables
	launch       func(ctx context.Context, environ []string, l Launch) error // starts the launch process of a claimed run (StartLaunch)
	now          func() time.Time
	onBehind     func()                                  // tests only: called after a reader drops an event
	extract      func(ctx context.Context, g Gate) error // one extraction pass of the store of g; nil for none
	stale        func(ctx context.Context, g Gate) error // the stale check of every workspace of the store of g; nil for none
	merge        func(ctx context.Context, g Gate) error // the merge pass of the store of g; nil for none
	// scan claims and runs the history scan of the store of g; nil for none. done says
	// that this brained has nothing more to scan.
	scan  func(ctx context.Context, g Gate) (done bool, err error)
	moved func() bool // reports a new brained binary at its path; nil for none
}

// item is one event, or the end of a stream, from a reader.
type item struct {
	ev  herdr.Event
	src *herdr.Stream
	err error
}

// pane is a live pane that brained follows.
type pane struct {
	workspaceID string
	agent       bool          // herdr detected an agent in the pane
	learning    Learning      // the state of the pane's last step
	auto        bool          // the workspace of the pane has an enabled automation
	joined      string        // the last agent status that joined agent.get; "" for none
	stream      *herdr.Stream // the status stream; nil while learning is off and auto is false
}

// loop is the state of one run. Only the run goroutine uses it; the readers use only items,
// behind and wg.
type loop struct {
	d      *daemon
	items  chan item
	behind chan struct{}
	wg     sync.WaitGroup
	main   *herdr.Stream
	panes  map[string]*pane
	gate   *Gate // nil while the session is not bound
	redo   *Gap  // the gap of a reconcile that failed; the next tick tries it again

	extracting  bool       // an extraction pass runs
	lastExtract time.Time  // the start of the last extraction pass
	extracted   chan error // the end of an extraction pass

	staling   bool       // a stale check runs
	lastStale time.Time  // the start of the last stale check of every workspace
	staled    chan error // the end of a stale check

	merging   bool       // a merge pass runs
	lastMerge time.Time  // the start of the last merge pass
	merged    chan error // the end of a merge pass

	scanning  bool             // the history scan runs
	scanFor   bindings.Binding // the binding of the last scan run; a new one starts again
	scanDone  bool             // the history scan has nothing more for scanFor
	scanAfter time.Time        // the next try after a scan that failed
	scanned   chan scanResult  // the end of a history scan
}

// scanResult is the end of one history scan run of binding b.
type scanResult struct {
	b    bindings.Binding
	done bool
	err  error
}

// run connects, reconciles and then handles events until an ErrStop or ctx ends.
func (d *daemon) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	l := newLoop(d)
	defer func() {
		cancel()
		l.stop()
	}()
	if err := l.start(ctx); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case it := <-l.items:
			if err := l.item(ctx, it); err != nil {
				return err
			}
		case <-l.behind:
			if err := l.drain(ctx); err != nil {
				return err
			}
			l.retry(ctx, GapBehind)
		case <-d.tick:
			// [NEW] The binary check runs at each tick (checkEvery), so a new binary runs within
			// about 2 s of its install.
			if d.moved != nil && d.moved() {
				return errRestart
			}
			if err := l.check(ctx); err != nil {
				return err
			}
			if l.redo != nil {
				l.retry(ctx, *l.redo)
			} else {
				l.report(l.sync(ctx, GapLearning, false))
			}
			l.extract(ctx)
			l.checkStale(ctx)
			l.mergePass(ctx)
			l.historyScan(ctx)
		case err := <-l.extracted:
			l.extracting = false
			l.report(err)
		case err := <-l.staled:
			l.staling = false
			l.report(err)
		case err := <-l.merged:
			l.merging = false
			l.report(err)
		case r := <-l.scanned:
			l.scanning = false
			l.scanDone = r.done && r.b == l.scanFor
			if r.err != nil && !r.done {
				l.scanAfter = time.Now().Add(extractEvery)
			}
			l.report(r.err)
		}
	}
}

func newLoop(d *daemon) *loop {
	return &loop{d: d, items: make(chan item, d.queue), behind: make(chan struct{}, 1), panes: map[string]*pane{},
		extracted: make(chan error, 1), staled: make(chan error, 1), merged: make(chan error, 1),
		scanned: make(chan scanResult, 1)}
}

// historyScan starts the history scan of the bound store at a tick,
// unless it runs, has nothing more for this brained, or failed less than extractEvery ago. It
// runs beside the loop: reading the files takes time, and the events do not wait for it. The
// end of ctx stops it.
func (l *loop) historyScan(ctx context.Context) {
	if l.d.scan == nil || l.gate == nil || l.scanning {
		return
	}
	if l.gate.Binding != l.scanFor {
		// Another binding, store or kind: what the last one found does not hold for it.
		l.scanFor, l.scanDone, l.scanAfter = l.gate.Binding, false, time.Time{}
	}
	if l.scanDone || time.Now().Before(l.scanAfter) {
		return
	}
	l.scanning = true
	g := *l.gate
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		done, err := l.d.scan(ctx, g)
		l.scanned <- scanResult{g.Binding, done, err}
	}()
}

// checkStale starts the stale check of every workspace of the bound store (once a day),
// unless one runs or the last one started less than staleEvery ago. The first tick with a
// binding starts one. It runs beside the loop, as the extraction pass.
func (l *loop) checkStale(ctx context.Context) {
	if l.d.stale == nil || l.gate == nil || l.staling || !l.lastStale.IsZero() && time.Since(l.lastStale) < staleEvery {
		return
	}
	l.staling, l.lastStale = true, time.Now()
	g := *l.gate
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.staled <- l.d.stale(ctx, g)
	}()
}

// mergePass starts the merge pass of the bound store (once a day), unless one runs or
// the last one started less than mergeEvery ago. The first tick with a binding starts one. It
// runs beside the loop, as the stale check; the store keeps the hash of each part at its last
// pass, so a restart does not pass an unchanged part again.
func (l *loop) mergePass(ctx context.Context) {
	if l.d.merge == nil || l.gate == nil || l.merging || !l.lastMerge.IsZero() && time.Since(l.lastMerge) < mergeEvery {
		return
	}
	l.merging, l.lastMerge = true, time.Now()
	g := *l.gate
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.merged <- l.d.merge(ctx, g)
	}()
}

// extract starts an extraction pass of the bound store, unless one runs or the last
// one started less than extractEvery ago. The pass runs beside the loop: an extractor call
// takes seconds or minutes, and the events do not wait for it. The end of ctx stops it.
func (l *loop) extract(ctx context.Context) {
	if l.d.extract == nil || l.gate == nil || l.extracting || time.Since(l.lastExtract) < extractEvery {
		return
	}
	l.extracting, l.lastExtract = true, time.Now()
	g := *l.gate
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.extracted <- l.d.extract(ctx, g)
	}()
}

// start connects, checks and reconciles with GapStart.
func (l *loop) start(ctx context.Context) error {
	main, err := l.d.connect(ctx)
	if err != nil {
		return err
	}
	l.main = main
	l.read(ctx, main)
	if err := l.check(ctx); err != nil {
		return err
	}
	l.retry(ctx, GapStart)
	return nil
}

// stop closes every stream and waits for the readers. ctx of start must be done.
func (l *loop) stop() {
	l.closeAll()
	l.wg.Wait()
}

// connect opens the main stream. It tries again while herdr's socket is not ready.
func (d *daemon) connect(ctx context.Context) (*herdr.Stream, error) {
	var err error
	for i := range d.tries {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(d.pause):
			}
		}
		var st *herdr.Stream
		st, err = d.subscribe(ctx, mainSubs())
		if err == nil {
			return st, nil
		}
	}
	return nil, fmt.Errorf("connect to herdr (%d tries): %w", d.tries, err)
}

func (d *daemon) subscribe(ctx context.Context, subs []herdr.Subscription) (*herdr.Stream, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return d.sock.Subscribe(ctx, subs)
}

// read starts the reader of st. A reader never waits for the handler: when the queue is
// full it drops the event and signals behind.
func (l *loop) read(ctx context.Context, st *herdr.Stream) {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		for {
			ev, err := st.Next()
			var shape *herdr.ShapeError
			if err != nil && !errors.As(err, &shape) {
				select {
				case l.items <- item{src: st, err: err}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case l.items <- item{ev: ev, src: st, err: err}:
			default:
				select {
				case l.behind <- struct{}{}:
				default: // a reconcile is due already
				}
				if l.d.onBehind != nil {
					l.d.onBehind()
				}
			}
		}
	}()
}

// report writes err to the log. brained keeps running: the next check or reconcile tries
// again, and the handler checks learning before it writes.
func (l *loop) report(err error) {
	if err != nil {
		_, _ = fmt.Fprintf(l.d.log, "brained: %v\n", err) // a failed log write has no other place to go
	}
}

// item handles one item of a reader.
func (l *loop) item(ctx context.Context, it item) error {
	var shape *herdr.ShapeError
	switch {
	case errors.As(it.err, &shape):
		// An event that brained cannot read is a missed event.
		l.report(fmt.Errorf("event: %w", it.err))
		l.retry(ctx, GapBehind)
		return nil
	case it.err != nil && it.src == l.main:
		if errors.Is(it.err, io.EOF) {
			return fmt.Errorf("%w: herdr closed the event socket", ErrStop)
		}
		return l.reconnect(ctx, it.err)
	case it.err != nil:
		// A status stream ended. When brained closed it, no pane holds it. Else herdr
		// ended it: the pane may be gone, or its events are lost.
		for _, p := range l.panes {
			if p.stream == it.src {
				l.closeStream(p)
				l.retry(ctx, GapBehind)
				break
			}
		}
		return nil
	}
	ev := it.ev
	p := l.panes[ev.PaneID]
	switch ev.Kind {
	case herdr.PaneCreated:
		if p == nil {
			l.panes[ev.PaneID] = &pane{workspaceID: ev.WorkspaceID}
			l.report(l.syncPanes(ctx, []string{ev.PaneID}, GapNone, true, &ev))
		}
	case herdr.PaneAgentDetected:
		if p == nil {
			return nil
		}
		p.agent = !ev.Released
		if p.learning.On && ev.Released {
			l.step(ctx, Step{Kind: End, Reason: ReasonAgentLeft}, ev.PaneID, p, &ev)
		} else if p.learning.On {
			l.step(ctx, Step{Kind: Baseline, Gap: GapNone}, ev.PaneID, p, &ev)
		}
	case herdr.PaneStatusChanged:
		if p == nil || p.stream != it.src {
			return nil
		}
		// agent.get comes first: the join needs the state of the event, and a read takes time.
		l.automate(ctx, ev.PaneID, p, ev)
		if p.learning.On {
			l.step(ctx, Step{Kind: Change}, ev.PaneID, p, &ev)
		}
	case herdr.PaneExited, herdr.PaneClosed:
		if p != nil {
			l.drop(ctx, ev.PaneID, p, &ev)
		}
	}
	return nil
}

// reconnect opens a new main stream after the old one failed with err, and reconciles: the
// events of the time between are lost.
func (l *loop) reconnect(ctx context.Context, err error) error {
	l.report(fmt.Errorf("event socket: %w", err))
	_ = l.main.Close() // it failed already; a close error loses nothing
	main, err := l.d.connect(ctx)
	if err != nil {
		return err
	}
	l.main = main
	l.read(ctx, main)
	l.retry(ctx, GapConnect)
	return nil
}

// drain drops the waiting events: the reconcile after it replaces them. It keeps the end of
// the main stream.
func (l *loop) drain(ctx context.Context) error {
	for {
		select {
		case it := <-l.items:
			if it.err == nil {
				continue
			}
			if err := l.item(ctx, it); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// check stops brained when Brain is unlinked or disabled, and reads the binding of the
// session. An unbound session gets no gate, so learning is off for all its panes.
func (l *loop) check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	ps, err := l.d.sock.Plugins(ctx)
	if err != nil {
		l.report(err)
	}
	if err == nil {
		if err := pluginState(ps, l.d.env.pluginID); err != nil {
			return err
		}
	}
	l.gate = nil
	bs, _, err := bindings.Read(l.d.bindingsFile)
	if err != nil {
		l.report(err)
		return nil
	}
	b, ok, err := bindings.BySocket(bs, l.d.env.socket)
	switch {
	case err != nil:
		l.report(err)
	case ok && b.Session == l.d.env.session:
		g := NewGate(b)
		l.gate = &g
	}
	return nil
}

// pluginState returns ErrStop when plugin id is not linked or is disabled.
func pluginState(ps []herdr.Plugin, id string) error {
	for _, p := range ps {
		if p.ID != id {
			continue
		}
		if !p.Enabled {
			return fmt.Errorf("%w: plugin %s is disabled", ErrStop, id)
		}
		return nil
	}
	return fmt.Errorf("%w: plugin %s is unlinked", ErrStop, id)
}

// retry reconciles with gap. When the reconcile fails, the next tick tries it again.
func (l *loop) retry(ctx context.Context, gap Gap) {
	l.redo = nil
	if err := l.reconcile(ctx, gap); err != nil {
		l.report(err)
		l.redo = &gap
	}
}

// reconcile reads the live panes. It ends the panes that are gone and gives each live
// agent with learning on a baseline with gap. brained does not invent the
// missed text.
func (l *loop) reconcile(ctx context.Context, gap Gap) error {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	live, err := l.d.sock.Panes(cctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(live))
	for _, lp := range live {
		seen[lp.ID] = true
		ids = append(ids, lp.ID)
		p := l.panes[lp.ID]
		if p == nil {
			p = &pane{}
			l.panes[lp.ID] = p
		}
		p.workspaceID, p.agent = lp.WorkspaceID, lp.Agent != nil
	}
	var open []string
	for id, p := range l.panes {
		if !seen[id] {
			l.drop(ctx, id, p, nil)
		} else if p.stream != nil {
			open = append(open, id)
		}
	}
	// Events may be lost before a reconcile, so the agent state of each pane that keeps its
	// stream is a new baseline; a new stream gets its baseline in follow.
	for _, id := range open {
		l.baselineAgent(ctx, id, l.panes[id])
	}
	return l.syncPanes(ctx, ids, gap, true, nil)
}

// sync applies the learning state of every pane that brained follows.
func (l *loop) sync(ctx context.Context, gap Gap, force bool) error {
	ids := make([]string, 0, len(l.panes))
	for id := range l.panes {
		ids = append(ids, id)
	}
	return l.syncPanes(ctx, ids, gap, force, nil)
}

// syncPanes reads the learning state of panes ids and applies it. A pane whose learning
// turned off ends, and loses its status stream unless its workspace has an enabled automation. A pane whose learning turned on, or was
// switched since its last step, gets its stream and a baseline with GapLearning. With force,
// every pane with learning on gets a baseline with gap. ev is the event of the call, if any.
func (l *loop) syncPanes(ctx context.Context, ids []string, gap Gap, force bool, ev *herdr.Event) error {
	states, autos, err := l.learning(ctx, ids)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		p := l.panes[id]
		was, wasAuto, had := p.learning, p.auto, p.stream != nil
		p.learning, p.auto = states[id], autos[id]
		switch {
		case !p.learning.On:
			if !p.auto {
				l.closeStream(p)
			} else if err := l.follow(ctx, id, p); err != nil {
				errs = append(errs, err)
			}
			if was.On && p.agent {
				l.step(ctx, Step{Kind: End, Reason: ReasonLearningOff}, id, p, ev)
			}
		case force || !was.On || !p.learning.Changed.Equal(was.Changed):
			g := gap
			if !force {
				g = GapLearning
			}
			if err := l.follow(ctx, id, p); err != nil {
				errs = append(errs, err)
				p.learning = Learning{} // not followed: the next sync tries again
				continue
			}
			if p.agent {
				l.step(ctx, Step{Kind: Baseline, Gap: g}, id, p, ev)
			}
		}
		if had && p.auto != wasAuto {
			// No stop counts across a time with no automation (no backfill). A new stream got
			// its baseline in follow.
			l.baselineAgent(ctx, id, p)
		}
	}
	return errors.Join(errs...)
}

// learning reads the learning state of panes ids in one store transaction, and whether the
// workspace of each has an enabled automation. Without a binding, learning is off and there
// is no automation for every pane.
func (l *loop) learning(ctx context.Context, ids []string) (_ map[string]Learning, _ map[string]bool, err error) {
	states := make(map[string]Learning, len(ids))
	autos := make(map[string]bool, len(ids))
	if l.gate == nil || len(ids) == 0 {
		return states, autos, nil
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	st, err := store.Open(ctx, l.gate.Binding.Store, busyTimeout)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	err = st.Write(ctx, func(tx *sql.Tx) error {
		keys, err := automated(ctx, tx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			ws := l.panes[id].workspaceID
			s, err := l.gate.Learning(ctx, tx, id, ws)
			if err != nil {
				return err
			}
			states[id] = s
			if len(keys) > 0 {
				key, err := l.gate.workspaceKey(ws)
				if err != nil {
					return err
				}
				autos[id] = key != "" && keys[key]
			}
		}
		return nil
	})
	return states, autos, err
}

// follow opens the status stream of pane id, unless it is open.
func (l *loop) follow(ctx context.Context, id string, p *pane) error {
	if p.stream != nil {
		return nil
	}
	st, err := l.d.subscribe(ctx, []herdr.Subscription{{Type: herdr.PaneStatusChanged, PaneID: id}})
	if err != nil {
		return fmt.Errorf("pane %s: %w", id, err)
	}
	p.stream = st
	l.read(ctx, st)
	l.baselineAgent(ctx, id, p)
	return nil
}

// drop ends pane id and stops following it.
func (l *loop) drop(ctx context.Context, id string, p *pane, ev *herdr.Event) {
	l.closeStream(p)
	if p.learning.On && p.agent {
		l.step(ctx, Step{Kind: End, Reason: ReasonGone}, id, p, ev)
	}
	delete(l.panes, id)
}

func (l *loop) closeStream(p *pane) {
	if p.stream != nil {
		_ = p.stream.Close() // brained stops reading it; a close error loses nothing
		p.stream = nil
	}
}

// closeAll closes every stream, so that each reader returns.
func (l *loop) closeAll() {
	if l.main != nil {
		_ = l.main.Close() // brained stops; a close error loses nothing
	}
	for _, p := range l.panes {
		l.closeStream(p)
	}
}

// step fills in the pane part of s and gives it to the handler.
func (l *loop) step(ctx context.Context, s Step, id string, p *pane, ev *herdr.Event) {
	s.PaneID, s.WorkspaceID, s.Event, s.Learning = id, p.workspaceID, ev, p.learning
	if l.gate != nil {
		s.Gate = *l.gate
	}
	if err := l.d.handler.Handle(ctx, s); err != nil {
		l.report(fmt.Errorf("%s pane %s: %w", s.Kind, id, err))
	}
}
