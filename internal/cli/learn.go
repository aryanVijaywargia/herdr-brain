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

const learnUsage = `usage: brain learn on|off [--workspace <w> | --pane <p>]`

// Learn runs `brain learn` with args after "learn" (E9-S4, §6.7 "Exclusion", §12). It records
// the learning switch of the whole store, of one workspace (by its Brain key) or of one pane
// of this herdr session. Delivery and brain remember never read it, and in M1 there is no
// capture, so the switch only records: it changes no item, output or receipt. E11 reads it.
func Learn(ctx context.Context, e Env, args []string) (err error) {
	if len(args) == 0 || args[0] != "on" && args[0] != "off" {
		return fmt.Errorf("want on or off\n%s", learnUsage)
	}
	on := args[0] == "on"
	fs := flag.NewFlagSet("learn", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	ws := fs.String("workspace", "", "")
	pane := fs.String("pane", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("%w\n%s", err, learnUsage)
	}
	switch {
	case fs.NArg() != 0:
		return fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), learnUsage)
	case *ws != "" && *pane != "":
		return fmt.Errorf("--workspace and --pane exclude each other\n%s", learnUsage)
	}
	// The switch touches no output, so the call does not repair it either.
	c, err := e.open(ctx, false)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	set := store.LearningSetting{Scope: store.LearningStore, On: on, Changed: e.Now()}
	where := "the whole store"
	switch {
	case *ws != "":
		key, label, err := workspaceKey(c, *ws)
		if err != nil {
			return err
		}
		set.Scope, set.Target = store.LearningWorkspace, key
		where = fmt.Sprintf("workspace %s (%s, key %s)", *ws, label, key)
	case *pane != "":
		if err := livePane(ctx, e, c.Binding, *pane); err != nil {
			return err
		}
		set.Scope, set.HerdrSession, set.Target = store.LearningPane, c.Binding.Session, *pane
		where = fmt.Sprintf("pane %s of herdr session %q", *pane, c.Binding.Session)
	}
	var storeOn bool
	err = c.Store.Write(ctx, func(tx *sql.Tx) error {
		if err := store.SetLearning(ctx, tx, set); err != nil {
			return err
		}
		storeOn, err = store.StoreLearningOn(ctx, tx, learningDefault(c.Binding))
		return err
	})
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("learning is %s for %s. Delivery and brain remember stay on.\n", onOffWord(on), where)
	if on && !storeOn {
		msg += "Learning stays off there: it is off for the whole store (brain learn on).\n"
	}
	_, err = io.WriteString(e.Stdout, msg)
	return err
}

// workspaceKey returns the Brain key and the label of workspace w of this herdr session: its
// herdr id, or the label of the calling pane's workspace.
func workspaceKey(c *Call, w string) (key, label string, err error) {
	if key, label, err := c.WorkspaceKey(); err == nil && w == label {
		return key, label, nil
	}
	m, err := c.Workspace.Lookup(w)
	if err != nil {
		return "", "", err
	}
	switch m.State {
	case workspace.Mapped:
		return m.Entry.Key, m.Entry.Label, nil
	case workspace.Unmapped:
		return "", "", fmt.Errorf("workspace %s is not mapped in herdr session %q; give a workspace id such as w1; nothing was written",
			w, c.Binding.Session)
	default:
		return "", "", fmt.Errorf("workspace %s: %w; nothing was written", w, m.Err)
	}
}

// livePane checks that herdr has pane id in the session of binding b, so a typo does not
// record a switch that never applies.
func livePane(ctx context.Context, e Env, b bindings.Binding, id string) error {
	ctx, cancel := context.WithTimeout(ctx, herdrDeadline)
	defer cancel()
	if _, err := e.Pane(ctx, herdr.Target{Session: b.Session, Socket: b.Socket}, id); err != nil {
		return fmt.Errorf("pane %s: %w; nothing was written", id, err)
	}
	return nil
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
	on, err := store.StoreLearningOn(ctx, tx, learningDefault(b))
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
