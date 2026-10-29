package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
)

const newPaneUsage = "usage: brain new-pane claude|codex"

// newPaneDeadline is the deadline of the herdr calls of brain new-pane. 5 s, as
// startHerdrDeadline.
const newPaneDeadline = 5 * time.Second

// newPane is `brain new-pane claude|codex`, the plugin actions "Brain: new Claude pane" and
// "Brain: new Codex pane" (E24-S7). It splits the focused pane of the action context, in the
// cwd of that pane, then types `brain claude` or `brain codex` into the shell of the new pane,
// so the pane keeps its shell after the CLI exits. The split always gets --cwd: without it,
// herdr uses its [terminal] new_cwd setting, which can be the home folder. bin is this brain
// binary, which may not be on PATH.
func newPane(ctx context.Context, args, environ []string, bin string) error {
	if len(args) != 1 || (args[0] != "claude" && args[0] != "codex") {
		return errors.New(newPaneUsage)
	}
	raw := lookupEnv(environ, "HERDR_PLUGIN_CONTEXT_JSON")
	if raw == "" {
		return errors.New("no HERDR_PLUGIN_CONTEXT_JSON: run it from its herdr action (brain.claude, brain.codex)")
	}
	var pc struct {
		PaneID string `json:"focused_pane_id"`
		Cwd    string `json:"focused_pane_cwd"`
	}
	if err := json.Unmarshal([]byte(raw), &pc); err != nil {
		return fmt.Errorf("read HERDR_PLUGIN_CONTEXT_JSON: %w", err)
	}
	if pc.PaneID == "" || pc.Cwd == "" {
		return errors.New("the herdr action context has no focused pane or no cwd: focus a pane, then run the action again")
	}
	// The default session has no HERDR_SESSION.
	t := herdr.Target{Session: cmp.Or(lookupEnv(environ, "HERDR_SESSION"), "default"), Socket: lookupEnv(environ, "HERDR_SOCKET_PATH")}
	c, err := herdr.New(environ, t)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, newPaneDeadline)
	defer cancel()
	p, err := c.SplitPane(ctx, pc.PaneID, pc.Cwd, true)
	if err != nil {
		return err
	}
	return c.RunInPane(ctx, p.ID, shellQuote(bin, lookupEnv(environ, "SHELL"))+" "+args[0])
}

// shellQuote quotes s as one word for shell, the shell of the new pane. sh, bash and zsh keep
// all text in single quotes as it is, so each ' ends the quotes, adds \', and opens them again.
// fish also reads \\ and \' in single quotes.
func shellQuote(s, shell string) string {
	if filepath.Base(shell) == "fish" {
		return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
