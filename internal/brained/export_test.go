package brained

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// Pipeline is a brained of herdr session s1 with the fake herdr, for the M4 exit pipeline test
// of package brained_test (E17-S12): that test also runs the cli commands, and package brained
// cannot import internal/cli. Workspace w1 (ws_1) has an approved tool lint that writes the
// file "ran" in its folder, and no automation; pane w1:p1 has a claude agent at rest in that
// folder.
type Pipeline struct {
	h   *autoHarness
	st  *fakeSub
	seq uint64
}

func NewPipeline(t *testing.T) *Pipeline {
	t.Helper()
	h := newAutoHarnessWith(t, bindings.KindMain, `argv = ["/usr/bin/touch", "ran"]`, false)
	if err := os.MkdirAll(h.folder["ws_1"], 0o700); err != nil {
		t.Fatal(err)
	}
	return &Pipeline{h: h, seq: 1}
}

func (p *Pipeline) Store() string    { return p.h.store }
func (p *Pipeline) Bindings() string { return p.h.d.bindingsFile }
func (p *Pipeline) Socket() string   { return p.h.f.path }
func (p *Pipeline) Folder() string   { return p.h.folder["ws_1"] }

// Start starts brained, which follows pane w1:p1 for learning, and waits for its baseline.
func (p *Pipeline) Start(t *testing.T) {
	t.Helper()
	p.st = p.h.startPane(t, "idle")
}

// Check sends one tick: brained reads the binding, the learning switches and the automations
// again, and waits until it took the tick.
func (p *Pipeline) Check(t *testing.T) {
	t.Helper()
	p.h.check(t)
	p.h.check(t) // the second tick waits for the end of the first
}

// Stop drives one stop of the agent in pane w1:p1: a joined working, then a joined idle.
func (p *Pipeline) Stop(t *testing.T) {
	t.Helper()
	p.seq++
	p.h.set("w1:p1", "working", p.seq, "")
	p.h.event(t, p.st, "w1:p1", "working")
	p.seq++
	p.h.set("w1:p1", "idle", p.seq, "")
	p.h.event(t, p.st, "w1:p1", "idle")
}

// Duplicate sends the last status event of pane w1:p1 again.
func (p *Pipeline) Duplicate(t *testing.T) {
	t.Helper()
	p.h.event(t, p.st, "w1:p1", "idle")
}

// Launch returns the arguments and the environment of the next launch process that brained
// started, or fails.
func (p *Pipeline) Launch(t *testing.T) (args, environ []string) {
	t.Helper()
	l := p.h.launch(t)
	return l.l.Args(), l.environ
}

// NoLaunch fails when brained started a launch process.
func (p *Pipeline) NoLaunch(t *testing.T) {
	t.Helper()
	p.h.noLaunch(t)
}

// Runs returns the automation runs of the store.
func (p *Pipeline) Runs(t *testing.T) []store.AutomationRun {
	t.Helper()
	return p.h.runs(t)
}

// MergePass runs the merge pass of the store of session s1 at now, with the fake extractor CLI r
// and no config file.
func (p *Pipeline) MergePass(r extract.Runner, now time.Time) error {
	g := NewGate(bindings.Binding{Session: "s1", Socket: p.h.f.path, Store: p.h.store, Kind: bindings.KindMain})
	return mergePass(context.Background(), g, filepath.Join(p.h.store, "config.toml"), io.Discard, r,
		func() time.Time { return now })
}
