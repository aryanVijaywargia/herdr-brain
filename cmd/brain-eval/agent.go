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
	cmd := exec.CommandContext(ctx, c.bin, slices.Concat([]string{"agent", "start", name, "--kind", kind, "--pane", id, "--"}, args)...)
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
			return fmt.Errorf("herdr agent start: %s (%s)", reply.Error.Message, reply.Error.Code)
		}
		return fmt.Errorf("herdr agent start: %w: %s", err, strings.TrimSpace(stderr.String()))
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

// agentArgs are the CLI arguments of a fresh CLI of harness in the scratch folder dir, which
// the runner made itself. Codex asks at start whether to trust a new folder, and the runner
// never answers that dialog; so Codex gets the trust of dir on its command line only, never in
// ~/.codex/config.toml. The key is a quoted TOML key, because Codex splits a dotted -c key at
// each dot (E10-S3). /tmp is a link on macOS, so the key is the real path of dir.
func agentArgs(harness, dir string) ([]string, error) {
	if harness != "codex" {
		return nil, nil
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	return []string{
		"-c", "projects={" + strconv.Quote(realDir) + "={trust_level=\"trusted\"}}",
		"-c", "check_for_update_on_startup=false",
	}, nil
}
