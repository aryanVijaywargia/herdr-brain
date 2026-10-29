package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
)

// client is the herdr client of a real run. herdr.Client starts an agent with no CLI
// arguments, so client runs `herdr agent start ... -- <args>` itself when a start has
// arguments (E10-S3).
type client struct {
	*herdr.Client
	bin string
	env []string
}

func newClient(environ []string, t herdr.Target) (*client, error) {
	c, err := herdr.New(environ, t)
	if err != nil {
		return nil, err
	}
	bin, err := herdrBinary(environ)
	if err != nil {
		return nil, err
	}
	env := slices.DeleteFunc(slices.Clone(environ), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return k == "HERDR_SESSION" || k == "HERDR_SOCKET_PATH"
	})
	env = append(env, "HERDR_SESSION="+t.Session, "HERDR_SOCKET_PATH="+t.Socket)
	return &client{Client: c, bin: bin, env: env}, nil
}

// StartAgent starts agent name of kind in pane id with the CLI arguments args.
func (c *client) StartAgent(ctx context.Context, name, kind, id string, args []string) error {
	if len(args) == 0 {
		return c.Client.StartAgent(ctx, name, kind, id)
	}
	return c.run(ctx, "agent start", slices.Concat([]string{"agent", "start", name, "--kind", kind, "--pane", id, "--"}, args)...)
}

// SendKeys presses keys in pane id, from `herdr pane send-keys`.
func (c *client) SendKeys(ctx context.Context, id string, keys ...string) error {
	return c.run(ctx, "pane send-keys", slices.Concat([]string{"pane", "send-keys", id}, keys)...)
}

// run runs herdr with args. The error of a refusal holds its code in parentheses.
func (c *client) run(ctx context.Context, what string, args ...string) error {
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Env = c.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// herdr prints {"error":{"code":...,"message":...}} when the server refuses a request.
		var reply struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if json.Unmarshal(stderr.Bytes(), &reply) == nil && reply.Error.Code != "" {
			return fmt.Errorf("herdr %s: %s (%s)", what, reply.Error.Message, reply.Error.Code)
		}
		return fmt.Errorf("herdr %s: %w: %s", what, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// herdrBinary is $HERDR_BIN_PATH, else the first herdr on the PATH of environ, as for
// herdr.New.
func herdrBinary(environ []string) (string, error) {
	if bin := lookup(environ, "HERDR_BIN_PATH"); bin != "" {
		return bin, nil
	}
	for _, dir := range filepath.SplitList(lookup(environ, "PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, "herdr")
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", errors.New("herdr not found: HERDR_BIN_PATH is not set and herdr is not on PATH")
}

// lookup returns the value of key in environ; the last entry wins, as in os/exec.
func lookup(environ []string, key string) string {
	var v string
	for _, kv := range environ {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// dirSlot is the placeholder of the scratch folder in a profile's eval.args.
const dirSlot = "{{dir}}"

// agentArgs are the eval.args of profile p for a fresh CLI in the scratch folder dir, which the
// runner made itself. Codex gets the trust of dir this way, because the runner never answers
// its folder-trust dialog (E10-S3). /tmp is a link on macOS, so dirSlot is the real path of
// dir, quoted.
func agentArgs(p *adapter.Profile, dir string) ([]string, error) {
	if len(p.Eval.Args) == 0 {
		return nil, nil
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	args := make([]string, len(p.Eval.Args))
	for i, a := range p.Eval.Args {
		args[i] = strings.ReplaceAll(a, dirSlot, strconv.Quote(realDir))
	}
	return args, nil
}

// trustDialog is the folder-trust dialog of a CLI that the runner answers: answer is the line
// that trusts the folder, key moves the selection to it, and selected is that line once it is
// selected.
type trustDialog struct {
	answer, key, selected string
}

// trustDialogs are the folder-trust dialogs that the runner answers, by profile kind (#276).
// Claude Code 2.1.292 asks in each new git folder, also below a trusted folder, and selects
// "No, exit" first. The Claude dialog lines and key, read on a test Mac.
var trustDialogs = map[string]trustDialog{
	"claude": {answer: "Yes, I trust this folder", key: "Down", selected: "❯ Yes, I trust this folder"},
}

// startCLI opens a pane beside the shell pane and starts a fresh CLI of kind in it, as agent
// name with args, in the scratch folder dir of the case. It returns the pane, and the herdr
// error of a CLI that did not get ready. A CLI at its folder-trust dialog for dir gets the trust
// answer once, as the runner made dir, and then a fresh start in a new pane (#276). The runner
// answers no other dialog.
func (r *Runner) startCLI(ctx context.Context, h Herdr, shell herdr.Pane, dir, name, kind string, args []string, opened *[]string) (herdr.Pane, error) {
	pane, err := r.splitStart(ctx, h, shell, dir, name, kind, args, opened)
	if !hasCode(err, "agent_not_ready") {
		return pane, err
	}
	answered, aerr := r.answerTrust(ctx, h, pane.ID, name, kind, dir)
	if aerr != nil {
		return herdr.Pane{}, aerr
	}
	if !answered {
		return pane, err
	}
	// The answered CLI goes on with its start: close it and wait for its exit, so that only the
	// fresh CLI runs in the case. When its start hook ran first, the brief of the fresh CLI
	// names its session, and the delivery check expects that line (#296).
	if err := r.closeAll(ctx, h, []string{pane.ID}); err != nil {
		return herdr.Pane{}, err
	}
	return r.splitStart(ctx, h, shell, dir, name, kind, args, opened)
}

// splitStart opens a pane beside the shell pane in dir and starts agent name of kind in it.
func (r *Runner) splitStart(ctx context.Context, h Herdr, shell herdr.Pane, dir, name, kind string, args []string, opened *[]string) (herdr.Pane, error) {
	pane, err := call(ctx, r.Timeouts.Call, func(ctx context.Context) (herdr.Pane, error) {
		return h.SplitPane(ctx, shell.ID, dir, false)
	})
	if err != nil {
		return herdr.Pane{}, err
	}
	*opened = append(*opened, pane.ID)
	_, err = call(ctx, r.Timeouts.Start, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, h.StartAgent(ctx, name, kind, pane.ID, args)
	})
	return pane, err
}

// answerTrust answers the folder-trust dialog of agent name in pane, a CLI of kind that did not
// get ready in folder dir. It answers only a dialog of trustDialogs that names dir, and returns
// false when the screen shows none.
func (r *Runner) answerTrust(ctx context.Context, h Herdr, pane, name, kind, dir string) (bool, error) {
	d, ok := trustDialogs[kind]
	if !ok {
		return false, nil
	}
	realDir, err := filepath.EvalSymlinks(dir) // the dialog names the real folder
	if err != nil {
		return false, err
	}
	screen := func() (string, error) {
		return call(ctx, r.Timeouts.Call, func(ctx context.Context) (string, error) { return h.ReadAgentVisible(ctx, name) })
	}
	s, err := screen()
	if err != nil || !strings.Contains(s, realDir) || !strings.Contains(s, d.answer) {
		return false, err
	}
	// Each key waits for its change of the screen: the selection on the answer, then the
	// dialog gone. The CLI saves the trust before it leaves the dialog.
	for _, k := range []struct {
		key, line string
		shown     bool
	}{{d.key, d.selected, true}, {"Enter", d.answer, false}} {
		if _, err := call(ctx, r.Timeouts.Call, func(ctx context.Context) (struct{}, error) {
			return struct{}{}, h.SendKeys(ctx, pane, k.key)
		}); err != nil {
			return false, err
		}
		err := r.poll(ctx, r.Timeouts.Start, func() (bool, error) {
			s, err := screen()
			return strings.Contains(s, k.line) == k.shown, err
		})
		if errors.Is(err, errWaitTimeout) {
			return false, fmt.Errorf("the folder-trust dialog of agent %s did not take key %s in %s", name, k.key, r.Timeouts.Start)
		}
		if err != nil {
			return false, err
		}
	}
	return true, nil
}
