package cli

import (
	"bufio"
	"cmp"
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
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/output"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const doctorUsage = `usage: brain doctor [--fix]`

// noReceipt is the gap word of a live agent whose agent session has no receipt (brain status).
const noReceipt = "unknown: no receipt for its agent session"

// Doctor runs `brain doctor` (E9-S3, §13 "Clear failures"): every check, and one line with
// the fix for each failed check. A clean setup prints "all checks passed"; any failed check
// returns an error, so the command exits 1. It opens the store as every store command does, so
// it runs the repair and the deferred session-id check (§7.5 step 6). The one fix that writes,
// a new hook entry, asks first and goes through install with its guard (§8.1). With --fix it
// applies that fix with no question (E19-S4: brain update runs it); an entry that you edited
// still stays. In a session with no binding it explains, prints the setup command, and reads
// and writes nothing (S84).
func Doctor(ctx context.Context, e Env, args []string) (err error) {
	fix := len(args) == 1 && args[0] == "--fix"
	if len(args) != 0 && !fix {
		return fmt.Errorf("unexpected argument %q\n%s", args[0], doctorUsage)
	}
	w := &lineWriter{w: e.Stdout}
	c, err := e.Open(ctx)
	if explained, err := explainNoStore(w, e, err, "doctor"); explained || err != nil {
		return errors.Join(err, w.err, errors.New("1 check failed"))
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	d := &doctorCall{caller: newCaller(e, c), w: w, in: bufio.NewReader(orEmpty(e.Stdin)), fix: fix}
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
	fix    bool // --fix: every question gets "y" with no read
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
	if err := d.historyScan(ctx); err != nil {
		d.fail("history scan: %v", err)
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

// historyScan reports each CLI whose session files the history scan could not parse (§6.8
// "Fail closed", S94). It is a note, not a failed check: the scan runs once, and the other CLI
// and herdr capture still work.
func (d *doctorCall) historyScan(ctx context.Context) error {
	var sc store.Scan
	var ok bool
	err := d.Store.Write(ctx, func(tx *sql.Tx) (err error) {
		sc, ok, err = store.HistoryScan(ctx, tx)
		return err
	})
	if err != nil || !ok {
		return err
	}
	for _, line := range ScanUnparsed(sc) {
		d.w.printf("note: %s; Brain may need an update for this format: brain update\n", line)
	}
	return nil
}

// hooks checks the hook entry, the hook record entries of an installed CLI (#315), the CLI
// version, a hook input that the profile could not parse (S14), the Codex trust of each Brain
// entry (hook_trust "codex") and the receipts of each profile.
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
		if st.Installed {
			if err := d.records(inst, p); err != nil {
				return err
			}
		}
		// A CLI update is only a warning (D13): the receipt check below finds a hook that stopped.
		switch n := st.VersionNote(); {
		case st.Installed && st.VersionErr != nil:
			d.w.printf("note: %s: version unknown (%v); Brain keeps its hook records and learning on\n", p.Kind, st.VersionErr)
		case n != "":
			d.w.printf("note: %s %s: %s; Brain keeps its hook records and learning on\n", p.Kind, st.CLIVersion, n)
		}
		if st.Retest {
			d.fail("%s %s: the Brain profile could not parse a %s hook input, last at %s, so Brain dropped it (S14); "+
				"herdr capture continues; fix: brain update, which may bring a profile for this input shape",
				p.Kind, cmp.Or(st.CLIVersion, "(version unknown)"), p.Kind, st.RetestAt.Local().Format(time.DateTime))
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
func (cl caller) installer() (*install.Installer, error) {
	home, err := paths.Home()
	if err != nil {
		return nil, err
	}
	bin, err := paths.BinaryPath()
	if err != nil {
		return nil, err
	}
	return &install.Installer{Home: home, Record: cl.Adapters.Install, Binary: bin}, nil
}

// records checks that each hook record entry that profile p turns on is in place (#315):
// with one missing, Brain stores no hook record of that CLI event. brain update adds them, with
// one question, as setup does.
func (d *doctorCall) records(inst *install.Installer, p *adapter.Profile) error {
	missing, target, err := missingRecords(inst, p)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		d.fail("hook records off for %s: %s has no Brain entry for %s; fix: brain update",
			p.Kind, target, strings.Join(missing, ", "))
	}
	return nil
}

// missingRecords returns the CLI events of the hook record entries of profile p that install
// would add, and their hook file (#315). An entry that you edited is not missing: Brain keeps
// it.
func missingRecords(inst *install.Installer, p *adapter.Profile) (events []string, target string, err error) {
	for _, h := range p.Hooks()[1:] {
		e, st, err := inst.WouldInstallHook(p, h)
		if err != nil {
			return nil, "", err
		}
		if st == install.Added {
			events, target = append(events, h.CLIEvent), e.Target
		}
	}
	return events, target, nil
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
		st, err = inst.InstallHook(p, h)
		if err != nil {
			return err
		}
		// Only a new start entry gets a new install time (#249).
		if st == install.Added && h == p.Hooks()[0] {
			if err := harness.MarkInstalled(d.Adapters.Installed, p.Kind, d.Now()); err != nil {
				return err
			}
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

// trust checks that Codex trusts the recorded Brain start entry (E8-S3) and each recorded hook
// record entry (#315). It reads only the hooks.state table of the Codex config.toml, and never
// writes it.
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
		// brain setup writes only ~/.codex/hooks.json and never reads CODEX_HOME, so the fix
		// is that folder; SETUP.md gives the same fix (#333).
		d.fail("codex: Codex reads %s, which holds no Brain hook entry; fix: run Codex with CODEX_HOME unset, "+
			"or set to ~/.codex, so that it reads ~/.codex/hooks.json", filepath.Join(dir, "hooks.json"))
	case harness.Modified:
		d.fail("codex: Codex hook not trusted: the Brain hook changed since you trusted it; %s", fix)
	case harness.Disabled:
		d.fail("codex: Codex hook not trusted: the Brain hook is disabled in Codex; fix: enable it in /hooks in Codex")
	default:
		d.fail("codex: Codex hook not trusted; %s", fix)
	}
	return d.recordTrust(dir, rec, p)
}

// recordTrust checks that Codex in the Codex home dir trusts each hook record entry of p that
// the install record rec holds (#315): Codex skips an untrusted entry, and codex exec gives no
// warning. A recorded entry that is not in the hook file is missing; the records check reports
// it.
func (d *doctorCall) recordTrust(dir string, rec install.Record, p *adapter.Profile) error {
	var bad []string
	for _, h := range p.Hooks()[1:] {
		e, ok := rec.Entry(p.Kind, h)
		if !ok {
			continue
		}
		t, err := harness.CodexTrust(dir, e, h.CLIEvent)
		if err != nil {
			return err
		}
		if t != harness.Trusted && t != harness.NoEntry {
			bad = append(bad, fmt.Sprintf("%s (%s)", h.CLIEvent, t))
		}
	}
	if len(bad) > 0 {
		d.fail("codex: Codex hook record entries not trusted: %s, so Codex skips them; fix: start codex, type /hooks, "+
			"and press t on each Brain hook entry, or enable a disabled one", strings.Join(bad, ", "))
	}
	return nil
}

// receipts checks that each live agent of the configured profile p has a receipt (§17 item
// 30). No receipt means that the hook did not run or could not reach the herdr socket. An
// agent that started before Brain installed its hook never ran it: that is only a note (#249).
func (d *doctorCall) receipts(ctx context.Context, p *adapter.Profile) error {
	kind := p.Kind
	if d.panesErr != nil {
		return nil //nolint:nilerr // run reports the failed pane list as its own check
	}
	var missing, before []string
	err := d.Store.Write(ctx, func(tx *sql.Tx) error {
		for _, pane := range d.panes {
			if pane.Agent == nil || *pane.Agent != kind {
				continue
			}
			g, err := d.gap(ctx, tx, d.Binding.Session, kind, pane)
			if err != nil {
				return err
			}
			switch {
			case g == beforeBrain:
				before = append(before, pane.ID)
			case strings.HasPrefix(g, noReceipt):
				missing = append(missing, pane.ID)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(before) > 0 {
		d.w.printf("note: %s: %d live agent(s) started before Brain installed its hook (%s); "+
			"their next sessions get Brain items\n", kind, len(before), strings.Join(before, ", "))
	}
	if len(missing) == 0 {
		return nil
	}
	// Claude runs the hook at start. Codex runs it at the first prompt (M0 item 1), and herdr
	// gets the Codex session id from its own hook at that prompt too (#257): a Codex agent with
	// no id is not counted here, and one with an id ran its hooks.
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
// With --fix it prints the answer "y" and reads nothing.
func (d *doctorCall) ask(question string) (bool, error) {
	d.w.printf("%s", question)
	if d.fix {
		d.w.printf("y (--fix)\n")
		return true, nil
	}
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
	gates, caps, err := extract.ReadConfig(d.Config)
	if err != nil {
		d.fail("extraction: %v, so Brain makes no extractor call; fix: correct or remove the [gates] and [caps] keys", err)
		return nil
	}
	p := extract.Pass{HerdrSession: d.Binding.Session, DefaultOn: learningDefault(d.Binding), Now: d.Now,
		Gates: gates, Caps: caps}
	var ws []extract.Waiting
	var cli store.ExtractorCLI
	err = d.Store.Write(ctx, func(tx *sql.Tx) (err error) {
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
