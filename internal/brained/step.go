package brained

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

// Handler gets the steps of the panes that brained follows, one at a time and in order, so
// the reads of a pane are one at a time (HLD §6.2 step 3). E11-S3 reads the pane in it.
//
// A handler must check the learning state in the transaction that writes: it writes only
// when Gate.Learning gives the same state as the step of its last baseline. Otherwise it
// drops the read, and that read is its new baseline with a gap. So text of a time with
// learning off never reaches the store, also when the switch comes between two brained
// checks or during a read.
type Handler interface {
	Handle(ctx context.Context, s Step) error
}

// StepKind is what a step asks of the handler.
type StepKind string

// The step kinds.
const (
	// Baseline: read the pane and keep the read as the base for the next change. Write none of
	// its text. Gap says why earlier text is missing.
	Baseline StepKind = "baseline"
	// Change: the agent status changed; the new part of the pane is new text.
	Change StepKind = "change"
	// End: stop the pane's capture and drop its last read. Reason says why.
	End StepKind = "end"
)

// Gap says why text before a baseline is missing.
type Gap string

// The gaps of a baseline.
const (
	GapNone     Gap = ""            // a new pane or a new agent: nothing is missing
	GapStart    Gap = "start"       // brained started; it missed what came before (S13, S32)
	GapBehind   Gap = "behind"      // brained dropped events (S39)
	GapConnect  Gap = "reconnect"   // the event socket failed; brained connected again
	GapLearning Gap = "learning on" // learning was off, so the text of that time is not read
)

// Reason says why a pane's capture ends.
type Reason string

// The reasons of an end.
const (
	ReasonGone        Reason = "gone"         // the pane exited or closed, or is not live at a reconcile
	ReasonLearningOff Reason = "learning off" // learning turned off for the pane
	ReasonAgentLeft   Reason = "agent left"   // herdr released the pane's agent
)

// Step is one step of one pane.
type Step struct {
	Kind        StepKind
	PaneID      string
	WorkspaceID string
	Gap         Gap          // Baseline only
	Reason      Reason       // End only
	Event       *herdr.Event // the herdr event of the step; nil for a reconcile or a learning change
	Learning    Learning     // the learning state that brained saw for the pane
	Gate        Gate
}

// Learning is the learning state of one pane.
type Learning struct {
	On      bool
	Changed time.Time // the latest change of a setting that applies; zero when none applies
}

// Gate is the one learning check of a bound herdr session (E9-S4, HLD §8.3). brained and the
// handler call it with the same arguments.
type Gate struct {
	Binding bindings.Binding
	ws      workspace.Session
}

// NewGate returns the gate of binding b.
func NewGate(b bindings.Binding) Gate {
	return Gate{Binding: b, ws: workspace.For(b)}
}

// Learning returns the learning state of pane paneID in workspace workspaceID, read in tx. A
// test store has learning off by default (§12.1), as in brain learn. A withheld workspace
// keeps its key, so a switch of that workspace still applies.
func (g Gate) Learning(ctx context.Context, tx *sql.Tx, paneID, workspaceID string) (Learning, error) {
	m, err := g.ws.Lookup(workspaceID)
	if err != nil {
		return Learning{}, err
	}
	var key string
	if m.State != workspace.Unmapped {
		key = m.Entry.Key
	}
	on, err := store.LearningOn(ctx, tx, g.Binding.Kind != bindings.KindTest, g.Binding.Session, key, paneID)
	if err != nil {
		return Learning{}, err
	}
	changed, err := store.LearningChanged(ctx, tx, g.Binding.Session, key, paneID)
	if err != nil {
		return Learning{}, err
	}
	return Learning{On: on, Changed: changed}, nil
}

// LogHandler writes one line for each step to W. It stands in until E11-S3 reads panes.
type LogHandler struct {
	W io.Writer
}

// Handle writes the line of s.
func (h LogHandler) Handle(_ context.Context, s Step) error {
	var extra string
	switch s.Kind {
	case Baseline:
		extra = " gap=" + string(s.Gap)
	case Change:
		extra = " status=" + s.Event.Status
	case End:
		extra = " reason=" + string(s.Reason)
	}
	_, err := fmt.Fprintf(h.W, "brained: %s pane %s workspace %s%s\n", s.Kind, s.PaneID, s.WorkspaceID, extra)
	return err
}
