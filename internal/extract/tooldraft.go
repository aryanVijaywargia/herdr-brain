package extract

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/aryanVijaywargia/herdr-brain/internal/signal"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/tool"
)

// kindTool is the claim kind of a tool draft (§10.1 detection, E17-S6).
const kindTool = "tool"

// The fields of a tool draft that Brain writes itself, for an exact repeat and for "make this a
// tool" (§10.1). [NEW] the values: the timeout of the §10.1 example; no read-only guess, so the
// restriction is yours to set; an effects line that asks for your words.
const (
	draftTimeout = 600
	draftEffects = "runs the recorded commands as the agent ran them; edit this line to say what they change"
	// draftRule is the rule of the claim key of a tool draft [NEW].
	draftRule = "make it a brain tool"
	// ExactBenefit is the benefit of a tool draft that Brain writes itself: the exact tool path
	// and "make this a tool". No model wrote it, so review names the exact repeat rule as the
	// source of such a draft (#345).
	ExactBenefit = "runs these commands with one reviewed contract"
)

// shellChars are the characters of shell syntax [NEW]: a command with one of them needs a
// shell, so it does not fit one argv list (§10.1 "argv, not shell").
const shellChars = "|&;<>()$`\\\"'*?[]{}~#\n"

// toolWords returns the argv list of command text argv, its words. It returns false when the
// command does not fit an argv list: it needs a shell (shellChars, or a first word that sets a
// variable), or it runs Brain itself, as brain run does: a tool of a tool is no new tool. [NEW]
// both rules.
func toolWords(argv string) ([]string, bool) {
	ws := strings.Fields(argv)
	if len(ws) == 0 || strings.ContainsAny(argv, shellChars) || strings.Contains(ws[0], "=") || path.Base(ws[0]) == "brain" {
		return nil, false
	}
	return ws, true
}

// heldBack reports whether Brain never drafts a tool from command c: the secret filter found a
// secret in it, or it was cut (§6.7).
func heldBack(c store.CommandRecord) bool { return c.SecretFound || c.Truncated }

// toolKey is the claim key of a tool draft with steps in folder cwd [NEW]: Brain makes it from
// the folder and the commands, with each input as {name}, so the same commands in another
// session, from Brain or from the extractor, give the same claim. It keeps the case and quotes
// each word, so two other commands never share a key.
func toolKey(cwd string, steps [][]string) store.ClaimKey {
	runs := make([]string, len(steps))
	for i, s := range steps {
		words := make([]string, len(s))
		for j, w := range s {
			words[j] = strconv.Quote(w)
		}
		runs[i] = strings.Join(words, " ")
	}
	return store.ClaimKey{Situation: "the agent runs in " + strconv.Quote(cwd) + ": " + strings.Join(runs, " then "),
		Rule: draftRule}
}

// draftToml is the tool.toml of a draft, in the field order of §10.1.
type draftToml struct {
	Name        string                `toml:"name"`
	Description string                `toml:"description"`
	Cwd         string                `toml:"cwd"`
	TimeoutS    int64                 `toml:"timeout_s"`
	Effects     string                `toml:"effects"`
	ReadOnly    bool                  `toml:"read_only"`
	Argv        []string              `toml:"argv,omitempty"`
	Steps       [][]string            `toml:"steps,omitempty"`
	Inputs      map[string]tool.Input `toml:"inputs,omitempty"`
}

// text returns the tool text of d, after the E17-S3 contract check (tool.Parse): a draft that
// breaks a §10.1 rule is no draft. One command is argv; more are steps.
func (d draftToml) text(steps [][]string) (string, error) {
	if len(steps) == 1 {
		d.Argv = steps[0]
	} else {
		d.Steps = steps
	}
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(d); err != nil {
		return "", fmt.Errorf("write the tool draft: %w", err)
	}
	if _, err := tool.Parse(b.String()); err != nil {
		return "", err
	}
	return b.String(), nil
}

// nameChars are the runs of characters that a tool name cannot hold.
var nameChars = regexp.MustCompile(`[^a-z0-9]+`)

// maxDraftName is the longest name of a tool draft in characters [NEW], as for a skill.
const maxDraftName = 40

// draftName is the name of a draft that runs argv [NEW]: its words, with the folder of the
// program cut, in lower case with hyphens, cut to maxDraftName at a hyphen.
func draftName(argv []string) string {
	words := append([]string{path.Base(argv[0])}, argv[1:]...)
	n := strings.Trim(nameChars.ReplaceAllString(strings.ToLower(strings.Join(words, " ")), "-"), "-")
	for len(n) > maxDraftName {
		i := strings.LastIndex(n[:maxDraftName+1], "-")
		if i <= 0 {
			n = n[:maxDraftName]
			break
		}
		n = n[:i]
	}
	return strings.Trim(n, "-")
}

// exactDraft is the tool text of an exact repeat of cmds, one example in run order [NEW form]:
// the commands as they ran, in the folder where they ran. It refuses a command that held a
// secret, that was cut (§6.7), or that does not fit an argv list, and commands of more than
// one folder.
func exactDraft(cmds []store.CommandRecord) (string, error) {
	steps := make([][]string, 0, len(cmds))
	runs := make([]string, 0, len(cmds))
	for _, c := range cmds {
		switch {
		case c.SecretFound:
			return "", errors.New("a command of the example held a secret: Brain never drafts a tool from it")
		case c.Truncated:
			return "", errors.New("a command of the example was cut: Brain never drafts a tool from it")
		case c.CWD == "" || c.CWD != cmds[0].CWD:
			return "", errors.New("the commands of the example ran in no folder or in more than one folder")
		}
		ws, ok := toolWords(c.Argv)
		if !ok {
			return "", fmt.Errorf("command %q needs a shell or runs brain: a tool runs one argv list", c.Argv)
		}
		steps, runs = append(steps, ws), append(runs, strings.Join(ws, " "))
	}
	if len(steps) == 0 {
		return "", errors.New("the example has no command")
	}
	d := draftToml{Name: draftName(steps[0]), Description: "Run " + strings.Join(runs, ", then "), Cwd: cmds[0].CWD,
		TimeoutS: draftTimeout, Effects: draftEffects}
	return d.text(steps)
}

// gatedTool is a candidate of an exact repeat at its gate, with the tool text and the script
// text of its example.
type gatedTool struct {
	id     int64
	key    string // the workspace key
	text   string
	script string // "" for a chain that is too short for a script
}

// exactTools runs the exact repeats of command chains (§10.1 detection: exact repeats come
// first, model placeholders after). It makes no model call. Each command chain (E12-S1) that
// ran with no failure, whose commands Brain may draft a tool from, is evidence of the claim of
// its exact commands (toolKey), a command chain candidate in its workspace: the gate is 3
// sessions (§7.2). A failed chain and the chain of one session stay as evidence. At the gate
// the candidate gets one tool draft (exactDraft), after the queue checks of proposeBinding: the
// daily proposal cap, in round robin order of the workspaces (§7.3). A chain of minScript or more
// commands gets a script proposal too (proposeScript): the review offers both, and you pick one;
// they count as one proposal (#355). A chain that the extractor made a skill of needs judgment
// (§10.3): it gives no tool draft.
func (p Pass) exactTools(ctx context.Context, tx *sql.Tx) error {
	ins, err := store.SignalInputs(ctx, tx)
	if err != nil {
		return err
	}
	byID := map[int64]store.SignalInput{}
	for _, in := range ins {
		byID[in.ID] = in
	}
	sigs := signal.Find(ins)
	sessions, err := store.Sessions(ctx, tx)
	if err != nil {
		return err
	}
	byKey := map[int64]store.Session{}
	for _, s := range sessions {
		byKey[s.ID] = s
	}
	var gated []gatedTool
	seen := map[int64]bool{}
	for _, s := range sigs {
		if s.Kind != signal.CommandChain {
			continue
		}
		g, err := p.exactChain(ctx, tx, s, byID, byKey)
		if err != nil {
			return err
		}
		if g.id != 0 && !seen[g.id] {
			seen[g.id] = true
			gated = append(gated, g)
		}
	}
	return p.proposeGated(ctx, tx, gated)
}

// proposeGated makes the tool drafts and the scripts of gated, in round robin order of the
// workspaces (§7.3). A new tool draft needs room for one proposal under the daily cap. Its script
// needs none, and nor does the script of a pending draft, as the extractor's: a tool draft and
// its script count as one (#355), so the review offers both at once and you pick one (E17-S8).
func (p Pass) proposeGated(ctx context.Context, tx *sql.Tx, gated []gatedTool) error {
	last, err := store.LastCalls(ctx, tx)
	if err != nil {
		return err
	}
	for _, g := range roundRobin(gated, func(g gatedTool) string { return g.key }, last) {
		if err := p.proposeExact(ctx, tx, g); err != nil {
			return err
		}
		if g.script == "" {
			continue
		}
		if err := proposeScript(ctx, tx, g.id, g.script, p.Now()); err != nil {
			return err
		}
	}
	return nil
}

// proposeExact makes the tool draft of gated chain g when its candidate has no pending one and
// the daily proposal cap has room for it.
func (p Pass) proposeExact(ctx context.Context, tx *sql.Tx, g gatedTool) error {
	if draft, err := toolDraftOf(ctx, tx, g.id); err != nil || draft != 0 {
		return err
	}
	if d, err := p.Day(ctx, tx); err != nil || d.Proposals >= d.Caps.Proposals {
		return err
	}
	_, err := proposeTool(ctx, tx, g.id, g.text, ExactBenefit, p.Now())
	if errors.Is(err, errNoDraft) {
		return nil
	}
	return err
}

// exactChain attaches chain s to the candidate of its exact commands, and returns the
// candidate with the tool text and the script text of s when it reached its gate. A chain of another herdr session,
// with no workspace key, with learning off, with a failed command, or with a command that Brain
// never drafts a tool from gives nothing.
func (p Pass) exactChain(ctx context.Context, tx *sql.Tx, s signal.Signal, byID map[int64]store.SignalInput,
	sessions map[int64]store.Session) (gatedTool, error) {
	first := byID[s.Observations[0]]
	sess := sessions[first.SessionID]
	if sess.Key.HerdrSession != p.HerdrSession || first.WorkspaceKey == "" {
		return gatedTool{}, nil
	}
	cmds := make([]store.CommandRecord, 0, len(s.Observations))
	steps := make([][]string, 0, len(s.Observations))
	for _, id := range s.Observations {
		in := byID[id]
		if in.WorkspaceKey != first.WorkspaceKey || in.Command.ExitStatus != nil && *in.Command.ExitStatus != 0 {
			return gatedTool{}, nil
		}
		ws, _ := toolWords(in.Command.Argv) // exactDraft refuses a command that does not fit
		cmds, steps = append(cmds, *in.Command), append(steps, ws)
	}
	text, err := exactDraft(cmds)
	if err != nil {
		return gatedTool{}, nil //nolint:nilerr // a chain with no draft stays evidence
	}
	on, err := store.LearningOn(ctx, tx, p.DefaultOn, p.HerdrSession, first.WorkspaceKey, sess.PaneID)
	if err != nil || !on {
		return gatedTool{}, err
	}
	if held, err := heldChain(ctx, tx, s, byID); err != nil || held {
		return gatedTool{}, err
	}
	claim, err := store.ClaimID(ctx, tx, toolKey(cmds[0].CWD, steps), p.Now())
	if err != nil {
		return gatedTool{}, err
	}
	if ts, err := store.TombstonesByClaim(ctx, tx, claim); err != nil || len(ts) > 0 {
		return gatedTool{}, err
	}
	id, err := p.attachChain(ctx, tx, claim, first.WorkspaceKey, s.Observations)
	if err != nil || id == 0 {
		return gatedTool{}, err
	}
	cand, err := store.CandidateByID(ctx, tx, id)
	if err != nil || !p.gates().Reached(cand) {
		return gatedTool{}, err
	}
	if known, err := knownTool(ctx, tx, claim, first.WorkspaceKey, cmds[0].CWD, steps); err != nil || known {
		return gatedTool{}, err
	}
	return gatedTool{id: id, key: first.WorkspaceKey, text: text, script: scriptText(cmds)}, nil
}

// knownTool reports whether a tool of another claim that a pane of workspace key sees, active or
// pending, runs steps in folder cwd, maybe with a step run again (runs): that chain, or a draft
// of it, makes no new tool proposal and uses no cap (#337). A tool of the same claim is the
// same tool: proposeTool, propose and proposeScript check it, and a pending draft of the claim
// still gets its script on a later day.
func knownTool(ctx context.Context, tx *sql.Tx, claim, key, cwd string, steps [][]string) (bool, error) {
	ts, err := store.ToolTexts(ctx, tx, key)
	if err != nil {
		return false, err
	}
	for _, t := range ts {
		if t.ClaimID == claim {
			continue
		}
		// A pending text that is no valid contract is no tool: approve refuses it.
		c, err := tool.Parse(t.Text)
		if err == nil && path.Clean(c.Cwd) == path.Clean(cwd) && runs(c, steps) {
			return true, nil
		}
	}
	return false, nil
}

// knownDraft reports whether tool draft text of candidate cand is a tool of another claim that
// its workspace has (knownTool): the extractor's draft of a known chain makes no proposal either.
func knownDraft(ctx context.Context, tx *sql.Tx, cand store.Candidate, text string) (bool, error) {
	d, err := tool.Parse(text)
	if err != nil {
		return false, err
	}
	return knownTool(ctx, tx, cand.ClaimID, cand.WorkspaceKey, d.Cwd, d.Commands())
}

// runs reports whether tool c runs chain when a step that runs again right after itself counts
// once, as in a chain (signal.chain) [NEW]: each command of chain fits the step of c at its
// place, and the words in place of its inputs are values that the input schema of c takes
// (tool.Contract.Fill). So a chain that adds a rerun to the steps of c is c, and so is a chain
// of a tool whose steps hold a rerun, as a draft before #337 could.
func runs(c tool.Contract, chain [][]string) bool {
	once := func(s [][]string) [][]string { return slices.CompactFunc(slices.Clone(s), slices.Equal[[]string]) }
	steps := once(c.Commands())
	if chain = once(chain); len(chain) != len(steps) {
		return false
	}
	values := map[string]string{}
	for i, ws := range chain {
		if !fits(steps[i], ws) {
			return false
		}
		for j, el := range steps[i] {
			if !wholeInput.MatchString(el) {
				continue
			}
			n := el[1 : len(el)-1]
			if v, ok := values[n]; ok && v != ws[j] {
				return false // one call gives an input one value
			}
			values[n] = ws[j]
		}
	}
	_, err := c.Fill(values)
	return err == nil
}

// freeName returns tool text with a name that no other tool that a pane of workspace key sees
// has, active or pending (#339): its own name, else that name with the first free number from
// 2, as "reset-db-sh-2" [NEW]. A tool of a name that an active tool has cannot be approved
// (§10.1), and brain run finds one tool by a name. item is the tool item of text, 0 for a new
// one: its own texts are no clash.
func freeName(ctx context.Context, tx *sql.Tx, text, key string, item int64) (string, error) {
	c, err := tool.Parse(text)
	if err != nil {
		return "", err
	}
	ts, err := store.ToolTexts(ctx, tx, key)
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, t := range ts {
		// A pending text that is no valid contract has no name to take: approve refuses it.
		if o, err := tool.Parse(t.Text); err == nil && t.ItemID != item {
			taken[o.Name] = true
		}
	}
	name := c.Name
	for n := 2; taken[name]; n++ {
		name = c.Name + "-" + strconv.Itoa(n)
	}
	// The text of a draft comes from draftToml.text: the encoder writes the name line first.
	return strings.Replace(text, "name = "+strconv.Quote(c.Name), "name = "+strconv.Quote(name), 1), nil
}

// heldChain reports whether chain s gives no exact draft: a forget erased the claim key, and the
// tombstone of a source stops the same evidence (§6.7); or the extractor made a skill of it, as
// it needs judgment (§10.3).
func heldChain(ctx context.Context, tx *sql.Tx, s signal.Signal, byID map[int64]store.SignalInput) (bool, error) {
	for _, id := range s.Observations {
		stopped, err := store.Tombstoned(ctx, tx, byID[id].CaptureIdentity)
		if err != nil || stopped {
			return stopped, err
		}
		skill, err := store.SkillEvidence(ctx, tx, id)
		if err != nil || skill {
			return skill, err
		}
	}
	return false, nil
}

// attachChain attaches the observations obs of a chain to the command candidate of claim in
// workspace key, and returns the candidate id, or 0 when none attached.
func (p Pass) attachChain(ctx context.Context, tx *sql.Tx, claim, key string, obs []int64) (int64, error) {
	var id int64
	for _, o := range obs {
		cid, err := store.Attach(ctx, tx, store.Candidate{ClaimID: claim, Scope: store.ScopeWorkspace,
			WorkspaceKey: key, Kind: store.CandidateCommandChain}, o, p.gates(), signal.Supports)
		switch {
		case errors.Is(err, store.ErrEvidenceRefused):
			continue
		case err != nil:
			return 0, err
		}
		id = cid
	}
	return id, nil
}

// errNoDraft says why a candidate gets no draft now; the message is for "make this a tool".
var errNoDraft = errors.New("no tool draft")

// proposeTool makes the tool draft with text of candidate id: a draft tool item in the scope of
// the candidate, its pending proposal through store.ProposeCandidate, and its first revision.
// It wraps errNoDraft when the candidate has a pending proposal, when its claim has an item or
// a rejection in its scope, or when its workspace has no row yet. It returns the proposal id.
func proposeTool(ctx context.Context, tx *sql.Tx, id int64, text, benefit string, now time.Time) (int64, error) {
	cand, err := store.CandidateByID(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	pending, err := store.PendingCandidateProposalIDs(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	has, err := store.ClaimHasItem(ctx, tx, cand.ClaimID, cand.WorkspaceKey)
	if err != nil {
		return 0, err
	}
	held, err := store.Rejected(ctx, tx, cand.ClaimID, cand.WorkspaceKey)
	switch {
	case err != nil:
		return 0, err
	case len(pending) > 0:
		return 0, fmt.Errorf("candidate %d has a pending proposal already (brain review %d): %w", id, pending[0], errNoDraft)
	case has:
		return 0, fmt.Errorf("the claim of candidate %d has an item already: %w", id, errNoDraft)
	case held:
		return 0, fmt.Errorf("the claim of candidate %d is rejected (brain review --rejected): %w", id, errNoDraft)
	}
	target, err := target(ctx, tx, cand)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("the workspace of candidate %d has no row yet: %w", id, errNoDraft)
	case err != nil:
		return 0, err
	}
	if text, err = freeName(ctx, tx, text, cand.WorkspaceKey, 0); err != nil {
		return 0, err
	}
	var key *string
	if cand.Scope == store.ScopeWorkspace {
		key = &cand.WorkspaceKey
	}
	item, err := store.InsertItem(ctx, tx, store.Item{ClaimID: cand.ClaimID, CurrentRevision: 1, Kind: store.KindTool,
		Scope: cand.Scope, WorkspaceKey: key, State: store.StateDraft, Created: now})
	if err != nil {
		return 0, err
	}
	base := int64(1)
	pid, err := store.ProposeCandidate(ctx, tx, id, store.Proposal{ItemID: &item, BaseRevision: &base, Text: text,
		Scope: cand.Scope, Target: target, Diff: store.SkillDiff("", text), ExpectedBenefit: benefit,
		PermissionWarning: store.TouchesPermission(text), Status: store.ProposalPending, Created: now})
	if err != nil {
		return 0, err
	}
	if err := store.InsertItemRevision(ctx, tx, store.ItemRevision{ItemID: item, Revision: 1, Text: text,
		Scope: cand.Scope, WorkspaceKey: key, State: store.StateDraft, Author: store.AuthorBrain,
		Origin: store.OriginExtractor, ProposalID: &pid, Created: now}); err != nil {
		return 0, err
	}
	return pid, store.MarkProposed(ctx, tx, id)
}

// MakeTool is the review action "make this a tool" (§10.1) on command candidate id [NEW form]:
// it makes a tool draft from one example, the commands of the newest session in the evidence
// of the candidate, as they ran (exactDraft). It needs no model call and adds no evidence: the
// batch of the example is committed, its watermark stays, and the support count does not
// change. For an input in place of a word, ask for a change of the draft (brain review <id>
// --change): the E13-S5 request route drafts it again with the extractor. The draft goes
// through the normal review; nothing runs before your approval. It returns the proposal id.
func MakeTool(ctx context.Context, tx *sql.Tx, id int64, now time.Time) (int64, error) {
	cand, err := store.CandidateByID(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	if cand.Kind != store.CandidateCommandChain {
		return 0, fmt.Errorf("candidate %d is a %s candidate: only a command candidate becomes a tool", id, cand.Kind)
	}
	ts, err := store.TombstonesByClaim(ctx, tx, cand.ClaimID)
	if err != nil {
		return 0, err
	}
	if len(ts) > 0 {
		return 0, fmt.Errorf("the claim of candidate %d is forgotten", id)
	}
	cmds, err := store.CandidateCommands(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	// The evidence of the learned automation case is stop observations, not commands (E17-S10).
	if len(cmds) == 0 {
		return 0, fmt.Errorf("candidate %d has no command in its evidence", id)
	}
	example, err := chainOf(ctx, tx, cmds[len(cmds)-1])
	if err != nil {
		return 0, err
	}
	text, err := exactDraft(example)
	if err != nil {
		return 0, fmt.Errorf("candidate %d: %w", id, err)
	}
	return proposeTool(ctx, tx, id, text, ExactBenefit, now)
}

// toolClaim returns claim c of kind tool, with its evidence, and the tool of r (checkTool). A
// tool draft is a workspace tool (its cwd is a folder of the workspace), and its claim key comes
// from its commands (toolKey), not from the model words.
func toolClaim(c Claim, r rawClaim) (Claim, error) {
	text, cwd, steps, err := checkTool(r, c.Refs)
	if err != nil {
		return Claim{}, err
	}
	key := toolKey(cwd, steps)
	c.Tool, c.Scope, c.Situation, c.Rule = text, store.ScopeWorkspace, key.Situation, key.Rule
	c.TouchesPermission = c.TouchesPermission || store.TouchesPermission(text)
	return c, nil
}

// chainOf returns the command chain of c (E12-S1): the commands of its turn, in run order.
// A command with no chain (a held-back command of the turn stops the chain) is its own example,
// so exactDraft says why it gives no draft.
func chainOf(ctx context.Context, tx *sql.Tx, c store.CandidateCommand) ([]store.CommandRecord, error) {
	ins, err := store.SignalInputs(ctx, tx)
	if err != nil {
		return nil, err
	}
	ins = slices.DeleteFunc(ins, func(in store.SignalInput) bool { return in.SessionID != c.SessionID })
	byID := map[int64]store.SignalInput{}
	for _, in := range ins {
		byID[in.ID] = in
	}
	for _, s := range signal.Find(ins) {
		if s.Kind != signal.CommandChain || !slices.Contains(s.Observations, c.ObservationID) {
			continue
		}
		cmds := make([]store.CommandRecord, len(s.Observations))
		for i, id := range s.Observations {
			cmds[i] = *byID[id].Command
		}
		return cmds, nil
	}
	return []store.CommandRecord{c.CommandRecord}, nil
}

// rawInput is one input of a claim of kind tool, as the model returns it.
type rawInput struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Pattern string `json:"pattern"`
}

// checkTool returns the tool text of claim r of kind tool, with refs its evidence, and its
// folder and commands. The secret filter runs on the model fields first (Redact). Each command
// must be a command of the evidence that Brain may draft a tool from, with an input only in
// place of a whole word: Brain never writes new program logic (§10.1, §10.3). All the commands
// must have run in one folder, the folder of the tool; the newest folder that fits wins. The
// model does not give it. Then tool.Parse checks the contract with the E17-S3 rules. No error
// holds model text.
func checkTool(r rawClaim, refs []ref) (text, cwd string, steps [][]string, err error) {
	field := func(f *string) string {
		if f == nil {
			return ""
		}
		return Redact(strings.Join(strings.Fields(*f), " "))
	}
	if r.Commands == nil || len(*r.Commands) == 0 || r.TimeoutS == nil || r.ReadOnly == nil {
		return "", "", nil, errors.New("the tool has no commands, timeout_s or read_only")
	}
	d := draftToml{Name: field(r.Name), Description: field(r.Description), TimeoutS: *r.TimeoutS, Effects: field(r.Effects),
		ReadOnly: *r.ReadOnly}
	if r.Inputs != nil {
		d.Inputs = map[string]tool.Input{}
		for _, in := range *r.Inputs {
			name := Redact(in.Name)
			if _, dup := d.Inputs[name]; dup {
				return "", "", nil, errors.New("the tool has an input twice")
			}
			d.Inputs[name] = tool.Input{Type: Redact(in.Type), Pattern: Redact(in.Pattern)}
		}
	}
	steps = make([][]string, len(*r.Commands))
	for i, argv := range *r.Commands {
		steps[i] = make([]string, len(argv))
		for j, el := range argv {
			steps[i][j] = Redact(el)
		}
	}
	if d.Cwd = toolFolder(steps, refs); d.Cwd == "" {
		return "", "", nil, errors.New("the tool commands are not commands of the evidence in one folder")
	}
	// A tool.Parse error can quote a model field, such as the name or a pattern.
	if text, err = d.text(steps); err != nil {
		return "", "", nil, errors.New("the tool contract is not valid")
	}
	return text, d.Cwd, steps, nil
}

// toolFolder returns the newest folder of refs in which each of steps fits a command of refs
// that Brain may draft a tool from, or "" for none.
func toolFolder(steps [][]string, refs []ref) string {
	byCwd := map[string][][]string{}
	var newest []string // the folders, the folder of the newest command last
	for _, rf := range slices.SortedFunc(slices.Values(refs), func(a, b ref) int { return int(a.obs - b.obs) }) {
		if rf.cmd == nil || heldBack(*rf.cmd) || rf.cmd.CWD == "" {
			continue
		}
		if ws, ok := toolWords(rf.cmd.Argv); ok {
			byCwd[rf.cmd.CWD] = append(byCwd[rf.cmd.CWD], ws)
			newest = append(slices.DeleteFunc(newest, func(c string) bool { return c == rf.cmd.CWD }), rf.cmd.CWD)
		}
	}
	for _, cwd := range slices.Backward(newest) {
		if !slices.ContainsFunc(steps, func(step []string) bool {
			return !slices.ContainsFunc(byCwd[cwd], func(ws []string) bool { return fits(step, ws) })
		}) {
			return cwd
		}
	}
	return ""
}

// fits reports whether step, a command of a tool draft, is command ws of the evidence, with an
// input ({name}) in place of any whole word but the program.
func fits(step, ws []string) bool {
	if len(step) != len(ws) || len(ws) == 0 || step[0] != ws[0] {
		return false
	}
	for i := range ws {
		if step[i] != ws[i] && !wholeInput.MatchString(step[i]) {
			return false
		}
	}
	return true
}

// wholeInput is a word that is one whole input, as tool.Parse wants it.
var wholeInput = regexp.MustCompile(`^\{[a-z][a-z0-9_]*\}$`)
