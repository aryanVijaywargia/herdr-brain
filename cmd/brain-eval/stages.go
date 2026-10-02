package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// agentStep is the prefix of a setup step that prompts a fresh CLI in place of the shell:
// "agent claude: <prompt>" or "agent codex: <prompt>". A transfer case saves a fact with one
// CLI this way and asks it with the other (§12.1 transfer).
const agentStep = "agent "

// parseAgentStep returns the harness and prompt of an agent step; ok is false for a shell
// step. kinds are the profile kinds.
func parseAgentStep(step string, kinds []string) (harness, prompt string, ok bool) {
	rest, found := strings.CutPrefix(step, agentStep)
	if !found {
		return "", "", false
	}
	harness, prompt, found = strings.Cut(rest, ":")
	if !found || !slices.Contains(kinds, harness) {
		return "", "", false
	}
	return harness, strings.TrimSpace(prompt), true
}

// startArgs are the CLI arguments of a fresh CLI of profile p in the scratch folder dir: its
// eval.args, and for a setup agent also its eval.setup_args. A setup agent of Claude may run
// `brain remember` without an ask: the runner never answers a permission dialog. [NEW] the
// allowed tool.
func startArgs(p *adapter.Profile, dir string, setup bool) ([]string, error) {
	args, err := agentArgs(p, dir)
	if err != nil {
		return nil, err
	}
	if setup {
		args = append(args, p.Eval.SetupArgs...)
	}
	return args, nil
}

// runAgentStep runs one agent setup step: a fresh CLI of harness in a pane beside the shell
// pane gets prompt and works until it is done. skip is the reason when the CLI did not get
// ready, for example at a dialog that the runner does not answer: the case cannot run then.
func (r *Runner) runAgentStep(ctx context.Context, h Herdr, shell herdr.Pane, dir, name, harness, prompt string, opened *[]string) (skip string, err error) {
	pane, err := call(ctx, r.Timeouts.Call, func(ctx context.Context) (herdr.Pane, error) {
		return h.SplitPane(ctx, shell.ID, dir)
	})
	if err != nil {
		return "", err
	}
	*opened = append(*opened, pane.ID)
	p, err := adapter.LoadFS(r.Profiles, harness)
	if err != nil {
		return "", err
	}
	args, err := startArgs(p, dir, true)
	if err != nil {
		return "", err
	}
	if _, err := call(ctx, r.Timeouts.Start, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, h.StartAgent(ctx, name, harness, pane.ID, args)
	}); err != nil {
		if hasCode(err, "agent_not_ready") {
			return fmt.Sprintf("the setup CLI %s did not get ready, for example at a dialog that the runner does not answer: %v", harness, err), nil
		}
		return "", err
	}
	_, err = call(ctx, r.Timeouts.Prompt+r.Timeouts.Call, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, h.PromptAgent(ctx, name, prompt, r.Timeouts.Prompt)
	})
	return "", err
}

// hookProbe calls `brain hook` of harness p.Harness in the shell pane with test start input
// and the herdr state of mode c.Hook, and returns what it printed as the reply (§12.1
// exclusion). The hook reads its herdr variables from the pane; the step changes only the
// one that the mode needs.
func (r *Runner) hookProbe(ctx context.Context, h Herdr, shell herdr.Pane, dir string, c Case, p Probe, b bindings.Binding, marker string, i int, opened *[]string) (Reply, error) {
	pane, set, err := r.hookState(ctx, h, shell, dir, c, opened)
	if err != nil {
		return Reply{}, err
	}
	// The same start input for both profiles: neither has a start condition. Input of an
	// unknown shape would set the adapter to retest, so it must stay valid.
	in, err := json.Marshal(map[string]string{"session_id": "brain-eval-" + marker, "cwd": dir, "hook_event_name": "SessionStart", "source": "startup"})
	if err != nil {
		return Reply{}, err
	}
	out := filepath.Join(dir, fmt.Sprintf(".brain-eval-hook-%d", i+1))
	quoted := make([]string, len(set))
	for j, s := range set {
		quoted[j] = shellQuote(s)
	}
	command := fmt.Sprintf("printf '%%s' %s | env %s brain hook --profile %s --event start > %s",
		shellQuote(string(in)), strings.Join(quoted, " "), p.Harness, shellQuote(out))
	if err := r.step(ctx, h, shell.ID, command, marker, 1000+i); err != nil { // [NEW] step numbers after the setup steps
		return Reply{}, fmt.Errorf("hook call: %w", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return Reply{}, err
	}
	rep := Reply{Harness: p.Harness, Pane: pane, Screen: string(data), Text: strings.TrimSpace(string(data)), Status: Delivered}
	if rep.Text == "" {
		rep.Status, rep.Why = NotDelivered, "the hook printed nothing"
	}
	d, ok, err := r.receipt(ctx, b, pane, p.Harness)
	if err != nil {
		return Reply{}, err
	}
	if ok {
		rep.Receipt = &d
	}
	return rep, nil
}

// hookState makes the herdr state of the hook mode of case c. It returns the pane that the
// hook call stands for and the env arguments that set it.
func (r *Runner) hookState(ctx context.Context, h Herdr, shell herdr.Pane, dir string, c Case, opened *[]string) (pane string, set []string, err error) {
	switch c.Hook {
	case "no-herdr-env":
		return shell.ID, []string{"-u", "HERDR_ENV"}, nil
	case "unbound-session":
		return shell.ID, []string{"HERDR_SOCKET_PATH=" + filepath.Join(dir, "unbound.sock")}, nil
	case "no-herdr-answer":
		// A herdr that answers after the hook's lookup deadline of 150 ms. [NEW] 2 s.
		slow := filepath.Join(dir, ".brain-eval-slow-herdr")
		if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 2\n"), 0o700); err != nil {
			return "", nil, err
		}
		return shell.ID, []string{"HERDR_BIN_PATH=" + slow}, nil
	case "closed-pane":
		closed, err := call(ctx, r.Timeouts.Call, func(ctx context.Context) (herdr.Pane, error) {
			return h.SplitPane(ctx, shell.ID, dir)
		})
		if err != nil {
			return "", nil, err
		}
		*opened = append(*opened, closed.ID)
		if _, err := call(ctx, r.Timeouts.Call, func(ctx context.Context) (struct{}, error) {
			return struct{}{}, h.ClosePane(ctx, closed.ID)
		}); err != nil {
			return "", nil, err
		}
		return closed.ID, []string{"HERDR_PANE_ID=" + closed.ID}, nil
	case "other-workspace":
		// A workspace made after the setup steps has no map entry: the hook gives it only the
		// global part.
		other, err := call(ctx, r.Timeouts.Call, func(ctx context.Context) (herdr.Pane, error) {
			return h.CreateWorkspace(ctx, dir, workspaceLabel(c)+"-other")
		})
		if err != nil {
			return "", nil, err
		}
		*opened = append(*opened, other.ID)
		return other.ID, []string{"HERDR_PANE_ID=" + other.ID, "HERDR_WORKSPACE_ID=" + other.WorkspaceID}, nil
	}
	return "", nil, fmt.Errorf("hook mode %q", c.Hook)
}

// shellQuote quotes s for sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// hookLabel labels what the hook printed in an exclusion case: PASS when it holds each
// expected answer and no other marker of the case, as the case withholds all the rest.
func hookLabel(c Case, markers map[string]string, text string) (Label, string) {
	expect := fillAll(c.Check.Expect, markers)
	names := slices.Sorted(func(yield func(string) bool) {
		for n := range markers {
			if !yield(n) {
				return
			}
		}
	})
	for _, n := range names {
		if v := markers[n]; !slices.Contains(expect, v) && strings.Contains(text, v) {
			return Invented, fmt.Sprintf("the hook printed marker %s, which this case withholds", v)
		}
	}
	for _, e := range expect {
		if !strings.Contains(text, e) {
			return Miss, fmt.Sprintf("the hook did not print %s", e)
		}
	}
	if len(expect) == 0 {
		return Pass, "the hook printed no marker of the case"
	}
	return Pass, "the hook printed the expected part only"
}

// record is what a usefulness probe changed in the scratch folder dir: `git status
// --porcelain`, for your review of the case (§12.1 usefulness).
func (r *Runner) record(ctx context.Context, dir string) string {
	ctx, cancel := context.WithTimeout(ctx, r.Timeouts.Step)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain")
	cmd.Env = r.Environ
	out, err := cmd.Output()
	if err != nil {
		return fmt.Sprintf("git status: %v", err)
	}
	if lines := strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", "; ")); len(lines) > 0 {
		return "git status: " + strings.Join(lines, " ")
	}
	return "git status: no change"
}

// leak reports where the test store of b still holds one of forgotten, the answers that a
// forget case removed: a file of the out folder, or a text value of a table. The store keeps
// no raw page check: SQLite can keep a removed value in a free page until a vacuum. It
// returns "" when the text is nowhere (§12.1 forget, S19).
func (r *Runner) leak(ctx context.Context, b bindings.Binding, forgotten []string) (string, error) {
	var where []string
	err := filepath.WalkDir(filepath.Join(b.Store, outDir), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, f := range forgotten {
			if strings.Contains(string(data), f) {
				rel, _ := filepath.Rel(b.Store, path)
				where = append(where, fmt.Sprintf("%s in %s", f, rel))
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(b.Store, store.FileName)); err == nil {
		s, err := store.Open(ctx, b.Store, r.Timeouts.Call)
		if err != nil {
			return "", err
		}
		defer s.Close() //nolint:errcheck // read-only use: the read error is the one to report
		err = s.Write(ctx, func(tx *sql.Tx) error {
			found, err := tableLeaks(ctx, tx, forgotten)
			where = append(where, found...)
			return err
		})
		if err != nil {
			return "", err
		}
	}
	if len(where) == 0 {
		return "", nil
	}
	sort.Strings(where)
	return "the store still holds forgotten " + strings.Join(where, ", "), nil
}

// columns returns the column names of table t.
func columns(ctx context.Context, tx *sql.Tx, t string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, t)
	if err != nil {
		return nil, err
	}
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			_ = rows.Close()
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, errors.Join(rows.Err(), rows.Close())
}

// tableLeaks lists each table of the store whose text values hold one of forgotten.
func tableLeaks(ctx context.Context, tx *sql.Tx, forgotten []string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			_ = rows.Close()
			return nil, err
		}
		tables = append(tables, t)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var where []string
	for _, t := range tables {
		cols, err := columns(ctx, tx, t)
		if err != nil {
			return nil, err
		}
		if len(cols) == 0 {
			continue
		}
		conds := make([]string, len(cols))
		for i, c := range cols {
			conds[i] = fmt.Sprintf("instr(CAST(%q AS TEXT), ?1) > 0", c)
		}
		// A match in any column of any row, each value as text.
		q := fmt.Sprintf(`SELECT count(*) FROM %q WHERE %s`, t, strings.Join(conds, " OR "))
		for _, f := range forgotten {
			var n int
			if err := tx.QueryRowContext(ctx, q, f).Scan(&n); err != nil {
				return nil, fmt.Errorf("scan table %s: %w", t, err)
			}
			if n > 0 {
				where = append(where, fmt.Sprintf("%s in table %s", f, t))
			}
		}
	}
	return where, nil
}
