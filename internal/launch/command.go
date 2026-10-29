package launch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/bindings"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/resolver"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// callDeadline is the deadline of the herdr call of the resolve, and of `<cli> --version`.
const callDeadline = 5 * time.Second

// waitDelay is how long `<cli> --version` may hold its output pipe open after it ends or after
// its deadline.
const waitDelay = 500 * time.Millisecond

// Deps are the parts of a launch command that reach outside the process; tests replace them.
type Deps struct {
	Environ  []string
	PID      int    // the process ID (os.Getpid)
	Self     string // the running brain binary: a launch never runs it as the CLI
	Bindings string // the bindings file
	Pane     resolver.PaneFunc
	Now      func() time.Time
	Stderr   io.Writer
	// Exec replaces the process with bin, with the args argv and the env environ
	// (syscall.Exec): the signals, the terminal and the exit code are then the CLI's.
	Exec func(bin string, argv, environ []string) error
	// Dir is the working folder of the call, and CodexSystem the system config folder of
	// Codex (/etc/codex): a Codex launch reads the config files of both.
	Dir, CodexSystem string
}

// Main is `brain <kind> [args]`: it starts the CLI of kind in place of Brain. In a live pane of
// a bound herdr session, a form that gets Brain flags starts with the Brain flags of a new
// launch, then args with no change. Every other call starts the plain CLI with args, and
// prints one line on stderr with the reason. A refuse conflict starts nothing. Main writes
// nothing to a CLI config file. It returns only on an error.
func Main(ctx context.Context, d Deps, kind string, args []string) (err error) {
	data, err := load(kind)
	if err != nil {
		return err
	}
	c := readChain(d.Environ, d.PID)
	if c.depth >= maxDepth {
		return fmt.Errorf("brain started brain again %d times: a file named %s on PATH runs brain; "+
			"remove it from PATH, or run %s without brain", c.depth, kind, kind)
	}
	bin, err := findCLI(d.Environ, kind, d.Self, c.paths)
	if err != nil {
		return err
	}
	if args, err = again(ctx, d, c, args); err != nil {
		return err
	}
	argv, notes, plain, l, err := brainArgv(ctx, d, data, bin, args)
	if err != nil {
		return err
	}
	dir := ""
	if l != nil {
		dir = l.dir
	}
	if l != nil {
		// A real exec returns only when the CLI did not start: its launch must not stay running.
		defer func() {
			if err != nil {
				err = errors.Join(err, l.abandon(ctx, d.Now()))
			}
		}()
	}
	if plain != "" {
		argv = args
		// One line: a reason can hold the line breaks of a herdr error.
		notes = []string{fmt.Sprintf("starting the plain %s with no Brain flags: %s", kind, strings.Join(strings.Fields(plain), " "))}
	}
	for _, n := range notes {
		if _, err := fmt.Fprintf(d.Stderr, "brain %s: %s\n", kind, n); err != nil {
			return err
		}
	}
	return d.Exec(bin, append([]string{kind}, argv...), c.env(d.Environ, bin, dir))
}

// argvFile is the file of a launch folder with the argv that its launch gave the CLI and the
// user's args of the launch.
const argvFile = "argv.json"

// launchArgv is the content of argvFile.
type launchArgv struct {
	Argv []string `json:"argv"`
	Args []string `json:"args"`
}

// again returns the args of this Brain. A CLI file that an earlier Brain of this process ran,
// such as a shell shim with `exec brain claude "$@"`, can run brain again with the argv of that
// launch, and maybe more args after it. Then args start with the argv of the launch folder of
// the chain: again ends that launch, whose CLI did not start, and returns its user's args and
// the more args. The caller checks them all as a new launch, so a shim that adds a flag gets the
// same checks as the user. Any other args stay as they are; Brain flags in them are refused.
// The env only names the folder: the argv comes from the 0600 file of a bound store.
func again(ctx context.Context, d Deps, c chain, args []string) ([]string, error) {
	if len(c.paths) == 0 || c.dir == "" {
		return args, nil
	}
	l, ok, err := readArgv(d.Bindings, c.dir)
	if err != nil || !ok || len(args) < len(l.Argv) || !slices.Equal(args[:len(l.Argv)], l.Argv) {
		return args, err
	}
	if err := endFolder(ctx, c.dir, d.Now()); err != nil {
		return nil, err
	}
	return append(slices.Clone(l.Args), args[len(l.Argv):]...), nil
}

// readArgv reads the argvFile of the launch folder dir. ok is false when dir is no launch
// folder of a bound store, or it has no such file.
func readArgv(bindingsFile, dir string) (l launchArgv, ok bool, err error) {
	if _, err := folderID(dir); err != nil {
		return launchArgv{}, false, nil //nolint:nilerr // a folder that the env names is only a hint
	}
	bs, _, err := bindings.Read(bindingsFile)
	if err != nil {
		return launchArgv{}, false, err
	}
	bound, err := boundStore(bs, filepath.Dir(filepath.Dir(dir)))
	if err != nil || !bound {
		return launchArgv{}, false, err
	}
	b, err := os.ReadFile(filepath.Join(dir, argvFile))
	if errors.Is(err, fs.ErrNotExist) {
		return launchArgv{}, false, nil // a launch that ended: End deleted its folder
	}
	if err != nil {
		return launchArgv{}, false, err
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return launchArgv{}, false, fmt.Errorf("read the launch args: %w", err)
	}
	return l, true, nil
}

// endFolder ends the running launch of the launch folder dir at now, and deletes its folder.
func endFolder(ctx context.Context, dir string, now time.Time) error {
	id, err := folderID(dir)
	if err != nil {
		return err
	}
	storeDir := filepath.Dir(filepath.Dir(dir))
	st, err := store.Open(ctx, storeDir, busyTimeout)
	if err != nil {
		return err
	}
	var l store.Launch
	var ok bool
	err = st.Write(ctx, func(tx *sql.Tx) error {
		l, ok, err = store.LaunchByID(ctx, tx, id)
		return err
	})
	if err = errors.Join(err, st.Close()); err != nil || !ok {
		return err
	}
	gone := func(r store.Launch) bool { return r.ID == id }
	if err := End(ctx, storeDir, l.Socket, gone, store.EndAgentExit, now); err != nil {
		return fmt.Errorf("end the launch that ran brain again: %w", err)
	}
	return nil
}

// brainArgv returns the args of a Brain launch of the CLI bin of d with the user's args, the
// notes to print, and the new launch l. plain is the reason when the call gets no Brain flags.
func brainArgv(ctx context.Context, d Deps, data *cliData, bin string, args []string) (argv, notes []string, plain string, l *launched, err error) {
	a := parse(data, args)
	if r := a.plain(); r != "" {
		return nil, nil, r, nil, nil
	}
	r, plain := resolve(ctx, d)
	if plain != "" {
		return nil, nil, plain, nil, nil
	}
	if err := a.refused(); err != nil {
		return nil, nil, "", nil, err
	}
	if err := data.servers(a, d); err != nil {
		return nil, nil, "", nil, err
	}
	if plain := supported(ctx, data, bin, d.Environ); plain != "" {
		return nil, nil, plain, nil, nil
	}
	home, err := codexHome(data.kind, d.Environ)
	if err != nil {
		return nil, nil, "", nil, err
	}
	dir, err := Create(ctx, r.Binding, r.WorkspaceID, r.PaneID, data.kind, home, d.Now())
	if err != nil {
		return nil, nil, "", nil, err
	}
	l = &launched{b: r.Binding, dir: dir}
	hooks, err := captureHooks(data.kind, d.Self, dir)
	if err == nil {
		argv, notes, err = a.argv(dir, Plan{Hooks: hooks, MCP: mcpServer(d.Self, dir)})
	}
	if err == nil {
		_, err = writeJSON(dir, argvFile, launchArgv{Argv: argv, Args: args})
	}
	if err != nil {
		return nil, nil, "", nil, errors.Join(err, l.abandon(ctx, d.Now()))
	}
	return argv, notes, "", l, nil
}

// launched is a launch that Create made: its binding and its launch folder.
type launched struct {
	b   bindings.Binding
	dir string
}

// abandon ends the launch l, whose CLI did not start, at now and deletes its folder. It ends
// no other launch of the pane. No agent ran, so the record ends as at an agent exit.
func (l *launched) abandon(ctx context.Context, now time.Time) error {
	gone := func(r store.Launch) bool { return paths.Launch(l.b.Store, r.ID) == l.dir }
	if err := End(ctx, l.b.Store, l.b.Socket, gone, store.EndAgentExit, now); err != nil {
		return fmt.Errorf("end the launch that did not start: %w", err)
	}
	return nil
}

// chainEnv is the env name of the launch chain: "<pid>:<depth>:<CLI file>...". syscall.Exec
// keeps the process ID, so a Brain that starts with the pid of the chain was run again by a
// CLI file that an earlier Brain of this process chose, such as a shell script that runs
// `exec brain claude`. It skips those files. A nested `brain claude` in the session of the
// real CLI is another process, so it finds the real CLI again. depth counts the Brain launches
// above this one, also across processes, so a script that runs brain with no exec stops at
// maxDepth.
const chainEnv = "BRAIN_LAUNCH_CHAIN"

// maxDepth is the most Brain launches in one chain: far above any nesting of sessions.
const maxDepth = 8

// chainDirEnv is the env name of the launch folder of the last Brain of the chain, when it
// made a launch. Only a Brain of the same process uses it (again).
const chainDirEnv = "BRAIN_LAUNCH_DIR"

// chain is the launch chain of this process.
type chain struct {
	pid, depth int
	paths      []string // the CLI files that earlier Brain launches of this process ran
	dir        string   // the launch folder of the last of them; "" for none
}

// readChain reads the launch chain of environ for the process pid. A value that Brain did not
// write is no chain: the env is only a hint, and the depth then starts again at 0.
func readChain(environ []string, pid int) chain {
	c := chain{pid: pid}
	parts := strings.Split(lookup(environ, chainEnv), ":")
	if len(parts) < 3 {
		return c
	}
	p, err := strconv.Atoi(parts[0])
	if err != nil {
		return c
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil || n < 0 {
		return c
	}
	c.depth = n
	if p == pid {
		c.paths, c.dir = parts[2:], lookup(environ, chainDirEnv)
	}
	return c
}

// env returns environ with the chain of a launch that runs the CLI file bin, with the launch
// folder dir; "" for none.
func (c chain) env(environ []string, bin, dir string) []string {
	v := fmt.Sprintf("%d:%d:%s", c.pid, c.depth+1, strings.Join(append(slices.Clone(c.paths), bin), ":"))
	out := slices.DeleteFunc(slices.Clone(environ), func(kv string) bool {
		return strings.HasPrefix(kv, chainEnv+"=") || strings.HasPrefix(kv, chainDirEnv+"=")
	})
	out = append(out, chainEnv+"="+v)
	if dir != "" {
		out = append(out, chainDirEnv+"="+dir)
	}
	return out
}

// resolve resolves the pane of the call with the resolver. plain is why the call gets no
// Brain flags: it is not from a live pane of a bound herdr session, or Brain cannot read the
// bindings file.
func resolve(ctx context.Context, d Deps) (r *resolver.Resolved, plain string) {
	ctx, cancel := context.WithTimeout(ctx, callDeadline)
	defer cancel()
	res, err := resolver.New(d.Bindings, openNoStore, d.Pane).Resolve(ctx, d.Environ, "")
	if err != nil {
		return nil, err.Error()
	}
	switch v := res.(type) {
	case *resolver.NoStore:
		if v.Err != nil {
			return nil, fmt.Sprintf("%s (%v)", v.Reason, v.Err)
		}
		return nil, string(v.Reason)
	case *resolver.Denied:
		return nil, fmt.Sprintf("herdr does not confirm pane %s in workspace %s (%s)", v.PaneID, v.WorkspaceID, v.Reason)
	case *resolver.Resolved:
		_ = v.Store.Close() // openNoStore opened nothing
		return v, ""
	}
	panic(fmt.Sprintf("launch: unknown resolver result %T", res))
}

// supported returns why the CLI bin of d gets no Brain flags: its version is older than the
// min_version of d, or Brain cannot read its version. "" is a supported CLI.
func supported(ctx context.Context, d *cliData, bin string, environ []string) string {
	v, err := cliVersion(ctx, bin, environ)
	if err != nil {
		return err.Error()
	}
	if d.older(v) {
		return fmt.Sprintf("%s %s is older than %s", d.kind, v, d.MinVersion)
	}
	return ""
}

// noStore is the store of the resolve of a launch. The resolve only checks the pane: the
// launch writes its own record with Create, writes no receipt, and leaves the agent checks of
// the pane to the next Brain call.
type noStore struct{}

func openNoStore(context.Context, string) (resolver.Store, error) { return noStore{}, nil }

func (noStore) WriteWithheld(context.Context, resolver.Withheld) error {
	return errors.New("a launch writes no receipts")
}

func (noStore) RecordAgentSession(context.Context, string, string, string) error {
	return errors.New("a launch writes no receipts")
}

func (noStore) UncheckedAgentSessions(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (noStore) SetAgentCheck(context.Context, string, string, string, resolver.AgentCheck) error {
	return nil
}

func (noStore) Close() error { return nil }

// cliVersion runs `bin --version` and returns the first version word of its output.
func cliVersion(ctx context.Context, bin string, environ []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, callDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = environ
	// A child of the CLI can hold the output pipe open past the deadline: stop the wait soon
	// after, so the whole check stays bounded.
	cmd.WaitDelay = waitDelay
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", bin, err)
	}
	for _, w := range strings.Fields(string(out)) {
		if versionRe.MatchString(w) {
			return w, nil
		}
	}
	return "", fmt.Errorf("%s --version: no version in %q", bin, out)
}

// findCLI returns the first executable file named kind in an absolute folder of the PATH of
// environ that is not the brain binary self: not self, not a link to it and not a copy of it.
// So `brain claude` never runs itself, also with a brain link named claude earlier on PATH.
// It also skips the files of skip: the files of the launch chain that ran brain again.
func findCLI(environ []string, kind, self string, skip []string) (string, error) {
	me, err := os.Stat(self)
	if err != nil {
		return "", fmt.Errorf("brain binary: %w", err)
	}
	for _, dir := range filepath.SplitList(lookup(environ, "PATH")) {
		if !filepath.IsAbs(dir) {
			continue // as exec.LookPath: the working folder must not supply the CLI
		}
		p := filepath.Join(dir, kind)
		fi, err := os.Stat(p)
		// As exec.LookPath, a PATH entry that is not an executable file is skipped.
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 || slices.Contains(skip, p) {
			continue
		}
		if os.SameFile(me, fi) {
			continue
		}
		if fi.Size() == me.Size() {
			same, err := sameContent(self, p)
			if err != nil {
				return "", err
			}
			if same {
				continue
			}
		}
		return p, nil
	}
	return "", fmt.Errorf("%s not found on PATH (Brain skips its own binary and its copies)", kind)
}

// sameContent reports whether the files a and b hold the same bytes.
func sameContent(a, b string) (bool, error) {
	ha, err := fileHash(a)
	if err != nil {
		return false, err
	}
	hb, err := fileHash(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ha, hb), nil
}

// fileHash returns the SHA-256 of the file at path.
func fileHash(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only: Close cannot lose data
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
