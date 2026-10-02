package cli

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const doctorUsage = `usage: brain doctor`

// noReceipt is the gap word of a live agent whose agent session has no receipt (brain status).
const noReceipt = "unknown: no receipt for its agent session"

// Doctor runs `brain doctor` (E9-S3, §13 "Clear failures"): every check, and one line with
// the fix for each failed check. A clean setup prints "all checks passed"; any failed check
// returns an error, so the command exits 1. It opens the store as every store command does, so
// it runs the repair and the deferred session-id check (§7.5 step 6). The one fix that writes,
// a new hook entry, asks first and goes through install with its guard (§8.1). In a session
// with no binding it explains, prints the setup command, and reads and writes nothing (S84).
func Doctor(ctx context.Context, e Env, args []string) (err error) {
	if len(args) != 0 {
		return fmt.Errorf("unexpected argument %q\n%s", args[0], doctorUsage)
	}
	w := &lineWriter{w: e.Stdout}
	c, err := e.Open(ctx)
	if explained, err := explainNoStore(w, e, err, "doctor"); explained || err != nil {
		return errors.Join(err, w.err, errors.New("1 check failed"))
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	d := &doctorCall{caller: newCaller(e, c), w: w, in: bufio.NewReader(orEmpty(e.Stdin))}
	d.run(ctx)
	switch {
	case w.err != nil:
		return w.err
	case d.failed == 1:
		return errors.New("1 check failed")
	case d.failed > 1:
		return fmt.Errorf("%d checks failed", d.failed)
	}
	w.printf("all checks passed\n")
	return w.err
}

// orEmpty is r, or an empty reader when there is none: no answer means "no".
func orEmpty(r io.Reader) io.Reader {
	if r == nil {
		return strings.NewReader("")
	}
	return r
}

// doctorCall is one brain doctor call.
type doctorCall struct {
	caller
	w      *lineWriter
	in     *bufio.Reader
	failed int
	herdrState
}

// fail prints the line of one failed check.
func (d *doctorCall) fail(format string, args ...any) {
	d.failed++
	d.w.printf(format+"\n", args...)
}

func (d *doctorCall) run(ctx context.Context) {
	d.herdrState = readHerdr(ctx, d.Env, d.Binding)
	if d.versionErr != nil {
		d.fail("herdr: %v", d.versionErr)
	}
	if d.panesErr != nil && !errors.Is(d.panesErr, d.versionErr) {
		d.fail("herdr pane list: %v; fix: check that the herdr server of session %q runs, then run brain doctor again",
			d.panesErr, d.Binding.Session)
	}
	if err := d.extraction(ctx); err != nil {
		d.fail("extraction: %v", err)
	}
	ps, err := adapter.LoadAll(d.Profiles)
	if err != nil {
		d.fail("profiles: %v", err)
		return
	}
	if err := d.hooks(ctx, ps); err != nil {
		d.fail("hooks: %v", err)
	}
	d.conflicts()
}

// hooks checks the hook entry, the retest state, the Codex trust (hook_trust "codex") and the
// receipts of each profile.
func (d *doctorCall) hooks(ctx context.Context, ps []*adapter.Profile) error {
	inst, err := d.installer()
	if err != nil {
		return err
	}
	sts, err := harness.Check(ctx, fixedPanes(d.panes), d.Adapters, ps)
	if err != nil {
		return err
	}
	for i, p := range ps {
		st := sts[i]
		if !st.Configured {
			want, _, err := inst.WouldInstall(p)
			if err != nil {
				return err
			}
			if _, err := exec.LookPath(p.Kind); err == nil || st.Detected {
				d.fail("%s: no Brain hook in %s, so %s sessions get no Brain items; fix: brain setup",
					p.Kind, want.Target, p.Kind)
			}
			continue
		}
		if err := d.entry(inst, p); err != nil {
			return err
		}
		if st.Retest {
			version := st.CLIVersion
			if st.VersionErr != nil {
				version = "(version unknown)"
			}
			d.fail("%s %s: needs a retest: tested on %s, or the hook input changed its shape (S14); "+
				"fix: run the conformance test in the brain repo: go run ./cmd/brain-eval --probes evals/probes --stage delivery --harness %s", p.Kind, version, p.TestedVersion, p.Kind)
		}
		if p.HookTrust == adapter.TrustCodex {
			if err := d.trust(inst.Record, p); err != nil {
				return err
			}
		}
		if err := d.receipts(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// installer is the installer of this user, with this brain binary.
func (d *doctorCall) installer() (*install.Installer, error) {
	home, err := paths.Home()
	if err != nil {
		return nil, err
	}
	bin, err := paths.BinaryPath()
	if err != nil {
		return nil, err
	}
	return &install.Installer{Home: home, Record: d.Adapters.Install, Binary: bin}, nil
}

// entry checks that the recorded hook entry of p runs this brain binary. An entry of a
// missing binary (a moved or removed plugin, E1-S2) or of another binary gets the offer of a
// new entry. The offer shows each recorded entry of p: the start entry and the hook record
// entries (E11-S5). On yes it replaces each entry on its own, with the install guard, so an
// entry that you edited stays and the others still get the new binary; on no it changes no file.
func (d *doctorCall) entry(inst *install.Installer, p *adapter.Profile) error {
	rec, err := install.Read(inst.Record)
	if err != nil {
		return err
	}
	old := rec.Entries[p.Kind]
	want, _, err := inst.WouldInstall(p)
	if err != nil {
		return err
	}
	if old.Command == want.Command && old.Target == want.Target {
		return nil
	}
	bin := entryBinary(old.Command, p.Kind)
	if old.Target != want.Target {
		// The home changed since the install: the CLI reads another hook file.
		d.w.printf("%s: the Brain hook is in %s, but %s reads %s\n", p.Kind, old.Target, p.Kind, want.Target)
	} else {
		why := "another brain binary"
		if _, err := os.Stat(bin); err != nil {
			why = "a missing binary"
		}
		d.w.printf("%s: the Brain hook in %s runs %s (%s), not %s\n", p.Kind, old.Target, bin, why, inst.Binary)
	}
	hooks := []adapter.Hook{p.Hooks()[0]}
	q := "Replace it with this entry?\n  " + want.Text + "\n"
	for _, h := range p.Hooks()[1:] {
		if !rec.Has(p.Kind, h) {
			continue
		}
		e, _, err := inst.WouldInstallHook(p, h)
		if err != nil {
			return err
		}
		if len(hooks) == 1 {
			q = "Replace it and the hook record entries with these entries?\n  " + want.Text + "\n"
		}
		hooks = append(hooks, h)
		q += fmt.Sprintf("  %s: %s\n", h.CLIEvent, e.Text)
	}
	ok, err := d.ask(q + "[y/N] ")
	if err != nil {
		return err
	}
	if !ok {
		d.fail("%s: the Brain hook in %s runs %s; fix: run brain doctor again and answer y, or brain setup",
			p.Kind, old.Target, bin)
		return nil
	}
	return d.replace(inst, p, hooks, old.Target)
}

// replace takes out the recorded entry of each of hooks in target and adds the new one, one
// entry at a time. An entry that you edited stays, and the other entries still get replaced.
func (d *doctorCall) replace(inst *install.Installer, p *adapter.Profile, hooks []adapter.Hook, target string) error {
	for _, h := range hooks {
		st, err := inst.UninstallHook(p, h)
		if err != nil {
			return err
		}
		if st != install.Removed {
			d.fail("%s: %s holds the Brain %s entry that you edited (%s); Brain keeps it; fix: edit it by hand to run %s",
				p.Kind, target, h.CLIEvent, st, inst.Binary)
			continue
		}
		if _, err := inst.InstallHook(p, h); err != nil {
			return err
		}
		d.w.printf("Replaced the Brain %s entry for %s.\n", h.CLIEvent, p.Kind)
	}
	return nil
}

// entryBinary is the binary of a hook command that install built for kind: its first shell
// word, unquoted.
func entryBinary(command, kind string) string {
	bin := strings.TrimSuffix(command, " hook --profile "+kind+" --event start")
	if s, ok := strings.CutPrefix(bin, "'"); ok {
		bin = strings.ReplaceAll(strings.TrimSuffix(s, "'"), `'\''`, "'")
	}
	return bin
}

// trust checks that Codex trusts the recorded Brain entry (E8-S3). It reads only the
// hooks.state table of the Codex config.toml, and never writes it.
func (d *doctorCall) trust(record string, p *adapter.Profile) error {
	rec, err := install.Read(record)
	if err != nil {
		return err
	}
	home, err := paths.Home()
	if err != nil {
		return err
	}
	dir, err := harness.CodexHome(home, lookupEnv(d.Environ, "CODEX_HOME"))
	if err != nil {
		return err
	}
	t, err := harness.CodexTrust(dir, rec.Entries[p.Kind], p.Events.Start)
	if err != nil {
		return err
	}
	// Codex needs trust again for each new or changed hook entry, a hook record entry too
	// (E11-S5).
	fix := "fix: start codex, type /hooks, and press t on the Brain SessionStart hook " +
		"and on each new Brain hook entry (brain setup shows the steps)"
	switch t {
	case harness.Trusted:
	case harness.NoEntry:
		d.fail("codex: Codex reads %s, which holds no Brain hook entry; fix: brain setup with the same CODEX_HOME",
			filepath.Join(dir, "hooks.json"))
	case harness.Modified:
		d.fail("codex: Codex hook not trusted: the Brain hook changed since you trusted it; %s", fix)
	case harness.Disabled:
		d.fail("codex: Codex hook not trusted: the Brain hook is disabled in Codex; fix: enable it in /hooks in Codex")
	default:
		d.fail("codex: Codex hook not trusted; %s", fix)
	}
	return nil
}

// receipts checks that each live agent of the configured profile p has a receipt (§17 item
// 30). No receipt means that the hook did not run or could not reach the herdr socket.
func (d *doctorCall) receipts(ctx context.Context, p *adapter.Profile) error {
	kind := p.Kind
	if d.panesErr != nil {
		return nil //nolint:nilerr // run reports the failed pane list as its own check
	}
	var missing []string
	err := d.Store.Write(ctx, func(tx *sql.Tx) error {
		for _, pane := range d.panes {
			if pane.Agent == nil || *pane.Agent != kind {
				continue
			}
			g, err := gap(ctx, tx, d.Binding.Session, kind, pane)
			if err != nil {
				return err
			}
			if g == noReceipt {
				missing = append(missing, pane.ID)
			}
		}
		return nil
	})
	if err != nil || len(missing) == 0 {
		return err
	}
	// The herdr pane list shows no prompt. Claude runs the hook at start, so no receipt is a
	// failure. Codex runs it at the first prompt (M0 item 1), so no receipt is only a note:
	// the trust check above already fails an untrusted Codex hook.
	if p.HookTrust == adapter.TrustCodex {
		d.w.printf("note: codex: %d live agent(s) with no Brain receipt (%s); Codex runs the hook at the first prompt. "+
			"If an agent had a prompt, the hook is not trusted or not firing: trust it (/hooks in Codex); "+
			"if it is trusted, the CLI sandbox may block the herdr socket\n", len(missing), strings.Join(missing, ", "))
		return nil
	}
	d.fail("%s: hook not trusted or not firing: %d live agent(s) with no Brain receipt (%s); fix: start a new session; "+
		"if it gets no receipt either, the CLI sandbox may block the herdr socket", kind, len(missing), strings.Join(missing, ", "))
	return nil
}

// conflicts checks the output generations against their manifests (S66).
func (d *doctorCall) conflicts() {
	cs, err := output.Conflicts(filepath.Join(d.Binding.Store, outDir))
	switch {
	case err != nil:
		d.fail("output conflicts: %v", err)
	case len(cs) > 0:
		d.fail("output conflict: %d output generation(s) changed outside Brain, and the hook withholds them; "+
			"fix: brain repair --confirm deletes them", len(cs))
	}
}

// ask prints question and reads one answer line. Only "y" or "yes" is yes; EOF means "no".
func (d *doctorCall) ask(question string) (bool, error) {
	d.w.printf("%s", question)
	answer, err := d.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read answer: %w", err)
	}
	d.w.printf("\n")
	a := strings.ToLower(strings.TrimSpace(answer))
	return a == "y" || a == "yes", nil
}

// extraction checks the extraction queue of this herdr session (S15): signals wait while no
// extractor CLI is chosen, or after a failed call. Brain never inspects credentials; the stored
// reason is what the official CLI reported.
func (d *doctorCall) extraction(ctx context.Context) error {
	p := extract.Pass{HerdrSession: d.Binding.Session, DefaultOn: learningDefault(d.Binding), Now: d.Now}
	var ws []extract.Waiting
	var cli store.ExtractorCLI
	err := d.Store.Write(ctx, func(tx *sql.Tx) (err error) {
		if ws, err = p.Waiting(ctx, tx); err != nil {
			return err
		}
		s, err := store.Extractor(ctx, tx)
		cli = s.CLI
		return err
	})
	if err != nil {
		return err
	}
	if slices.ContainsFunc(ws, func(w extract.Waiting) bool { return w.NoCLI }) {
		d.fail("extraction: signals wait, and no extractor CLI is chosen; fix: brain setup")
	}
	for _, w := range ws {
		if w.Failed != "" {
			d.fail("extraction: the last extractor call for workspace %s failed (%s), so its signals wait; "+
				"fix: run %s in a terminal and check that it works and is signed in; Brain tries again later",
				w.WorkspaceKey, w.Failed, cli)
		}
	}
	return nil
}
