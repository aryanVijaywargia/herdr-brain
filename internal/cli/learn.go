package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/workspace"
)

const learnUsage = `usage: brain learn on|off [--workspace <workspace id> | --pane <pane id>]`

// Learn runs `brain learn` with args after "learn" (E9-S4, §6.7 "Exclusion", §12). It records
// the learning switch of the whole store, of one workspace (by its Brain key) or of one pane
// of this herdr session. Delivery and brain remember never read it, and in M1 there is no
// capture, so the switch only records: it changes no item, output or receipt. E11 reads it.
func Learn(ctx context.Context, e Env, args []string) (err error) {
	on, ws, pane, err := parseLearn(args)
	if err != nil {
		return err
	}
	// The switch touches no output, so the call does not repair it either.
	c, err := e.open(ctx, false)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	t, err := resolveTarget(ctx, e, c, ws, pane)
	if err != nil {
		return err
	}
	set := store.LearningSetting{Scope: t.scope, On: on, Changed: e.Now()}
	switch t.scope {
	case store.LearningWorkspace:
		set.Target = t.key
	case store.LearningPane:
		set.HerdrSession, set.Target = c.Binding.Session, pane
	}
	var now bool
	err = c.Store.Write(ctx, func(tx *sql.Tx) error {
		if err := store.SetLearning(ctx, tx, set); err != nil {
			return err
		}
		now, err = store.LearningOn(ctx, tx, learningDefault(c.Binding), c.Binding.Session, t.key, pane)
		return err
	})
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("learning is %s for %s. Delivery and brain remember stay on.\n", onOffWord(on), t.where)
	if on && !now {
		msg += "Learning stays off there: a wider scope is off (brain status shows it).\n"
	}
	_, err = io.WriteString(e.Stdout, msg)
	return err
}

// learnTarget is the scope of one brain learn call.
type learnTarget struct {
	scope store.LearningScope
	key   string // the workspace key of the target; "" for the store or an unmapped pane
	where string // the target, for the output
}

// resolveTarget resolves the scope flags of call c: workspace id ws or pane id pane of this
// herdr session, or the whole store when both are "".
func resolveTarget(ctx context.Context, e Env, c *Call, ws, pane string) (learnTarget, error) {
	switch {
	case ws != "":
		m, err := workspaceKey(c, ws)
		if err != nil {
			return learnTarget{}, err
		}
		return learnTarget{scope: store.LearningWorkspace, key: m.Key,
			where: fmt.Sprintf("workspace %s (%s, key %s)", ws, m.Label, m.Key)}, nil
	case pane != "":
		p, err := livePane(ctx, e, c.Binding, pane)
		if err != nil {
			return learnTarget{}, err
		}
		t := learnTarget{scope: store.LearningPane, where: fmt.Sprintf("pane %s of herdr session %q", pane, c.Binding.Session)}
		// The key of the pane's workspace only tells the output whether a workspace switch
		// keeps learning off there.
		if m, err := c.Workspace.Lookup(p.WorkspaceID); err == nil && m.State == workspace.Mapped {
			t.key = m.Entry.Key
		}
		return t, nil
	}
	return learnTarget{scope: store.LearningStore, where: "the whole store"}, nil
}

// parseLearn parses the arguments of brain learn: on or off, and at most one scope flag.
func parseLearn(args []string) (on bool, ws, pane string, err error) {
	if len(args) == 0 || args[0] != "on" && args[0] != "off" {
		return false, "", "", fmt.Errorf("want on or off\n%s", learnUsage)
	}
	fs := flag.NewFlagSet("learn", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&ws, "workspace", "", "")
	fs.StringVar(&pane, "pane", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return false, "", "", fmt.Errorf("%w\n%s", err, learnUsage)
	}
	// An empty flag value, such as --pane "$p" with no $p, must not switch the whole store.
	fs.Visit(func(f *flag.Flag) {
		if f.Value.String() == "" && err == nil {
			err = fmt.Errorf("--%s is empty\n%s", f.Name, learnUsage)
		}
	})
	switch {
	case err != nil:
		return false, "", "", err
	case fs.NArg() != 0:
		return false, "", "", fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), learnUsage)
	case ws != "" && pane != "":
		return false, "", "", fmt.Errorf("--workspace and --pane exclude each other\n%s", learnUsage)
	}
	return args[0] == "on", ws, pane, nil
}

// workspaceKey returns the map entry of workspace id w of this herdr session.
func workspaceKey(c *Call, w string) (workspace.Entry, error) {
	m, err := c.Workspace.Lookup(w)
	if err != nil {
		return workspace.Entry{}, err
	}
	switch m.State {
	case workspace.Mapped:
		return m.Entry, nil
	case workspace.Unmapped:
		return workspace.Entry{}, fmt.Errorf("workspace %s is not mapped in herdr session %q; give a workspace id such as w1; nothing was written",
			w, c.Binding.Session)
	default:
		return workspace.Entry{}, fmt.Errorf("workspace %s: %w; nothing was written", w, m.Err)
	}
}

// livePane returns pane id of the session of binding b from herdr, so a typo does not record
// a switch that never applies. herdr can answer an old pane id with the pane's new id: that
// id would never match, so it is refused too.
func livePane(ctx context.Context, e Env, b bindings.Binding, id string) (herdr.Pane, error) {
	ctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	defer cancel()
	p, err := e.Pane(ctx, herdr.Target{Session: b.Session, Socket: b.Socket}, id)
	switch {
	case err != nil:
		return herdr.Pane{}, fmt.Errorf("pane %s: %w; nothing was written", id, err)
	case p.ID != id:
		return herdr.Pane{}, fmt.Errorf("pane %s is now pane %s; use --pane %s; nothing was written", id, p.ID, p.ID)
	}
	return p, nil
}

// learningDefault is the learning setting of a store with no store row: off for a test store,
// so the M2 listener never captures an eval session (§12.1).
func learningDefault(b bindings.Binding) bool { return b.Kind != bindings.KindTest }

func onOffWord(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// learningLine is the learning line of brain status: on, off, or the scopes that are off.
func learningLine(ctx context.Context, tx *sql.Tx, b bindings.Binding) (string, error) {
	on, err := store.LearningOnWholeStore(ctx, tx, learningDefault(b))
	if err != nil {
		return "", err
	}
	if !on {
		return "learning: off for the whole store; delivery and brain remember stay on", nil
	}
	ss, err := store.LearningSettings(ctx, tx)
	if err != nil {
		return "", err
	}
	var offs []string
	for _, s := range ss {
		switch {
		case s.On:
		case s.Scope == store.LearningWorkspace:
			offs = append(offs, "workspace "+s.Target)
		case s.Scope == store.LearningPane:
			offs = append(offs, fmt.Sprintf("pane %s of herdr session %q", s.Target, s.HerdrSession))
		}
	}
	if len(offs) == 0 {
		return "learning: on", nil
	}
	return "learning: on; off for " + strings.Join(offs, ", "), nil
}
