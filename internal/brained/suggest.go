package brained

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/fingerprint"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// Dispatch is what the detached `brain suggest` process of one reserved suggestion gets: ids,
// never proposal text (E14-S3). Before it shows anything, the process resolves its pane again
// and requires each id to match.
type Dispatch struct {
	Suggestion int64  // the suggestions row; 0 for none
	Binding    string // the herdr session of the binding
	Store      string // the store folder of the binding
	Workspace  string // the Brain workspace key; "" for none
	Session    int64  // the capture session
	Occurrence int64  // the prompt occurrence: the observation of the reservation
	Generation int64  // the capture generation of the session at the reservation
	Proposal   int64
}

const suggestUsage = `usage: brain suggest --suggestion <id> --binding <herdr session> --store <folder> --workspace <key>
                     --session <id> --occurrence <id> --generation <n> --proposal <id>`

// Args returns the arguments of `brain suggest` for d.
func (d Dispatch) Args() []string {
	n := func(v int64) string { return strconv.FormatInt(v, 10) }
	return []string{"suggest", "--suggestion", n(d.Suggestion), "--binding", d.Binding, "--store", d.Store,
		"--workspace", d.Workspace, "--session", n(d.Session), "--occurrence", n(d.Occurrence),
		"--generation", n(d.Generation), "--proposal", n(d.Proposal)}
}

// ParseDispatch reads the arguments of `brain suggest` after "suggest", as Args writes them.
func ParseDispatch(args []string) (Dispatch, error) {
	var d Dispatch
	fs := flag.NewFlagSet("suggest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Int64Var(&d.Suggestion, "suggestion", 0, "")
	fs.StringVar(&d.Binding, "binding", "", "")
	fs.StringVar(&d.Store, "store", "", "")
	fs.StringVar(&d.Workspace, "workspace", "", "")
	fs.Int64Var(&d.Session, "session", 0, "")
	fs.Int64Var(&d.Occurrence, "occurrence", 0, "")
	fs.Int64Var(&d.Generation, "generation", 0, "")
	fs.Int64Var(&d.Proposal, "proposal", 0, "")
	if err := fs.Parse(args); err != nil {
		return Dispatch{}, fmt.Errorf("%w\n%s", err, suggestUsage)
	}
	if fs.NArg() != 0 || d.Suggestion <= 0 || d.Binding == "" || d.Store == "" || d.Session <= 0 ||
		d.Occurrence <= 0 || d.Generation < 0 || d.Proposal <= 0 {
		return Dispatch{}, fmt.Errorf("bad arguments %q\n%s", args, suggestUsage)
	}
	return d, nil
}

// Reserve reserves the suggestion of stored observation o when it is a new prompt whose full
// text full matches a ready proposal (store.ReserveSuggestion). A replay that does not
// count is no new prompt. It returns the Dispatch of the reservation, without its Store, which
// the caller sets; its Suggestion is 0 when there is none. The caller runs it in the
// transaction that stored o, after Match, and starts the dispatch after the commit.
func Reserve(ctx context.Context, tx *sql.Tx, o store.Observation, full string, now time.Time) (Dispatch, error) {
	if !isPrompt(o) || !o.Counts {
		return Dispatch{}, nil
	}
	s, err := store.ReserveSuggestion(ctx, tx, o.ID, fingerprint.Prompt(full), now)
	if err != nil || s.ID == 0 {
		return Dispatch{}, err
	}
	return Dispatch{Suggestion: s.ID, Binding: s.HerdrSession, Workspace: s.WorkspaceKey, Session: s.SessionID,
		Occurrence: s.ObservationID, Generation: s.Generation, Proposal: s.ProposalID}, nil
}

// Start starts `brain suggest` for d with environ, detached, and does not wait for it.
// A shell starts it in the background and exits at once, so the caller waits only for the
// shell: the hook keeps its cap, and brained keeps no child to reap. The new process has its
// own session, so the end of the agent's terminal does not stop it.
func Start(ctx context.Context, environ []string, d Dispatch) error {
	if err := detach(ctx, environ, d.Args()); err != nil {
		return fmt.Errorf("start brain suggest: %w", err)
	}
	return nil
}

// detach starts the brain binary with args and environ in the background, and does not wait
// for it.
func detach(ctx context.Context, environ, args []string) error {
	exe, err := paths.BinaryPath()
	if err != nil {
		return err
	}
	// "$0" and "$@" pass the binary and the arguments as words: the shell parses none of them.
	cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{"-c", `"$0" "$@" </dev/null >/dev/null 2>&1 &`, exe}, args...)...)
	cmd.Env = environ
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Run()
}

// PaneEnv returns environ with the herdr variables of pane paneID in workspace workspaceID of
// binding b: the environment of a `brain suggest` process that brained starts for that pane,
// so it resolves the pane as a call from the pane would (E2-S3).
func PaneEnv(environ []string, b bindings.Binding, paneID, workspaceID string) []string {
	env := slices.DeleteFunc(slices.Clone(environ), func(kv string) bool {
		// The herdr variables that name the pane of a call.
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "HERDR_ENV", "HERDR_SESSION", "HERDR_SOCKET_PATH", "HERDR_PANE_ID", "HERDR_WORKSPACE_ID":
			return true
		}
		return false
	})
	return append(env, "HERDR_ENV=1", "HERDR_SESSION="+b.Session, "HERDR_SOCKET_PATH="+b.Socket,
		"HERDR_PANE_ID="+paneID, "HERDR_WORKSPACE_ID="+workspaceID)
}
