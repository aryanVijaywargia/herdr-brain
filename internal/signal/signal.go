// Package signal holds the local signal rules of the learning pipeline (§7.1). The rules use
// no model. They are wide on purpose: the extractor removes the false positives. A signal only
// points at observations (a revert at commits). It stores no new text, and no signals table
// exists (§11): E12-S2 turns signals into candidates.
package signal

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// Kind is the kind of a signal (§7.1).
type Kind string

// The signal kinds. An explicit lesson comes from brain remember directly, not from a rule; a
// lesson is an explicit lesson in plain words in captured text (#253).
const (
	Correction      Kind = "correction"
	Lesson          Kind = "lesson"
	Procedure       Kind = "procedure"
	RepeatedPrompt  Kind = "repeated_prompt"
	CommandChain    Kind = "command_chain"
	Failure         Kind = "failure"
	Revert          Kind = "revert"
	BlockedApproval Kind = "blocked_approval"
)

// RevertEvent is the event of a git observation: a revert that git shows (#283).
const RevertEvent = "revert"

// promptEvent is the event of a hook prompt record (adapter.EventPrompt).
const promptEvent = "prompt"

// Signal is one signal. It holds ids and a hash, no text.
type Signal struct {
	Kind Kind
	// Observations are the observations of the signal. The first starts it. For a correction,
	// a procedure and a repeated prompt the rest are the earlier observations that it repeats,
	// oldest first. For a command chain they are the commands of the chain, in order.
	Observations []int64
	// Key is the hash of the signature of a failure, a command chain or an approval screen:
	// two signals with the same key are the same failure, chain or screen. E12-S2 counts the
	// sessions by key (§7.2). It is "" for the other kinds.
	Key string
}

// senders lists the senders whose observations can start a signal of each kind (§6.5).
// Assistant text is context only: it starts none. Tool output starts only failures, and a
// command chain comes only from command records, which are tool hook records. A blocked
// approval is not listed: herdr's blocked state starts it, not the sender of the screen text.
// A revert has no confirmed sender: you or the agent can run git.
var senders = map[Kind][]store.Sender{
	Correction:     {store.SenderYou, store.SenderUnconfirmed},
	Lesson:         {store.SenderYou, store.SenderUnconfirmed},
	Procedure:      {store.SenderYou, store.SenderUnconfirmed},
	RepeatedPrompt: {store.SenderYou, store.SenderUnconfirmed},
	Failure:        {store.SenderTool},
	CommandChain:   {store.SenderTool},
	Revert:         {store.SenderUnconfirmed},
}

// eligible reports whether an observation of sender s can start a signal of kind k.
func eligible(k Kind, s store.Sender) bool {
	return slices.Contains(senders[k], s)
}

// Supports reports whether an observation of sender s can support a candidate of kind k: the
// same table as for a signal of that kind (§6.5). It is the check of store.Attach.
func Supports(k store.CandidateKind, s store.Sender) bool { return eligible(Kind(k), s) }

// Find returns the signals in ins, the observations in time order (store.SignalInputs). It
// skips gaps and Brain-origin text. The text rules also skip observations that do not count
// (§6.4): a replay, or a hook record that only adds detail to a pane read. A git observation
// is a revert only: its text is Brain's own line, not text of a pane.
func Find(ins []store.SignalInput) []Signal {
	var kept []store.SignalInput
	var texts []text
	var sigs []Signal
	for _, in := range ins {
		if in.Status == store.ObservationGap || in.BrainOrigin {
			continue
		}
		if in.Source == store.SourceGit {
			if in.Event == RevertEvent && in.Counts && eligible(Revert, in.Sender) {
				sigs = append(sigs, Signal{Kind: Revert, Observations: []int64{in.ID}})
			}
			continue
		}
		kept = append(kept, in)
		if in.Counts && in.Command == nil && in.Excerpt != "" {
			texts = append(texts, newText(in))
		}
	}
	sigs = append(sigs, corrections(texts)...)
	sigs = append(sigs, procedures(texts)...)
	sigs = append(sigs, repeatedPrompts(texts)...)
	sigs = append(sigs, lessons(texts)...)
	sigs = append(sigs, commandChains(kept)...)
	sigs = append(sigs, failures(kept)...)
	sigs = append(sigs, blockedApprovals(texts)...)
	return sigs
}

// correctionMarkers are the words of correction-like text (§7.1) [NEW]: the HLD list, with
// "dont" and "do not" for "don't". Non-English markers come later from the E14 labeled set.
var correctionMarkers = []string{"no", "don't", "dont", "do not", "i said", "again", "instead", "wrong", "stop"}

// corrections finds correction-like text: text with a correction marker, or text that
// repeats earlier text of any session (§7.1). The sender stays unconfirmed.
func corrections(texts []text) []Signal {
	var sigs []Signal
	for i, t := range texts {
		if !eligible(Correction, t.in.Sender) {
			continue
		}
		earlier := matches(texts[:i], func(e text) bool {
			return eligible(Correction, e.in.Sender) && t.similar(e, promptSimilarity)
		})
		if len(earlier) == 0 && !t.hasAny(correctionMarkers) {
			continue
		}
		sigs = append(sigs, Signal{Kind: Correction, Observations: append([]int64{t.in.ID}, earlier...)})
	}
	return sigs
}

// lessonWords are the words of an explicit lesson in plain words (§7.1).
var lessonWords = []string{"remember", "from now on", "always", "never", "going forward", "in this project"}

// lessons finds text that gives a lesson for later in plain words: text with a lesson word
// (§7.1). The rule is wide: the extractor drops "never mind" and "I always forget the flag".
// Each one is a signal from its first occurrence; its gate is 1 (§7.2). The sender stays
// unconfirmed.
func lessons(texts []text) []Signal {
	var sigs []Signal
	for _, t := range texts {
		if eligible(Lesson, t.in.Sender) && t.hasAny(lessonWords) {
			sigs = append(sigs, Signal{Kind: Lesson, Observations: []int64{t.in.ID}})
		}
	}
	return sigs
}

// The limits of a procedure [NEW]: a long instruction has at least minProcedureRunes
// characters and minSteps steps, and it is similar to an earlier one when at least
// procedureSimilarity of the words of the two are shared (Jaccard).
const (
	minProcedureRunes   = 120
	minSteps            = 3
	procedureSimilarity = 0.5
)

// procedures finds long multi-step instructions that are similar to an earlier one, in any
// session (§7.1).
func procedures(texts []text) []Signal {
	var sigs []Signal
	for i, t := range texts {
		if !eligible(Procedure, t.in.Sender) || !t.isProcedure() {
			continue
		}
		earlier := matches(texts[:i], func(e text) bool {
			return eligible(Procedure, e.in.Sender) && e.isProcedure() && t.similar(e, procedureSimilarity)
		})
		if len(earlier) > 0 {
			sigs = append(sigs, Signal{Kind: Procedure, Observations: append([]int64{t.in.ID}, earlier...)})
		}
	}
	return sigs
}

// promptSimilarity is the least share of words (Jaccard) of two close prompts [NEW]. E14-S1
// sets the prompt fingerprint of the prompt index; this is only the wide rule.
const promptSimilarity = 0.8

// repeatedPrompts finds prompts that are the same as, or close to, earlier prompts in any
// session (§7.1).
func repeatedPrompts(texts []text) []Signal {
	var sigs []Signal
	for i, t := range texts {
		if !eligible(RepeatedPrompt, t.in.Sender) {
			continue
		}
		earlier := matches(texts[:i], func(e text) bool {
			return eligible(RepeatedPrompt, e.in.Sender) && t.similar(e, promptSimilarity)
		})
		if len(earlier) > 0 {
			sigs = append(sigs, Signal{Kind: RepeatedPrompt, Observations: append([]int64{t.in.ID}, earlier...)})
		}
	}
	return sigs
}

// commandChains finds the command chain of each turn: the commands of one session between two
// prompts, from the command records only (§7.1). In a session with hook prompt records, only a
// prompt ends a turn: a hook prompt record, or text that capture marks as yours. There a pane
// line can be tool output or screen noise that capture keeps as unconfirmed text, and it must
// not split one task into two chains (#337). A CLI with no prompt hook (agy) gives no hook
// prompt record, so there any text of yours or unconfirmed text ends a turn. Each chain is a
// signal from its first occurrence; E12-S2 counts the sessions of a key (§7.2). A chain with a
// command that the secret filter held back or that was cut gives no signal: Brain never drafts
// a tool from it (§6.7).
func commandChains(ins []store.SignalInput) []Signal {
	hooked := map[int64]bool{} // the sessions with a hook prompt record
	for _, in := range ins {
		if hookPrompt(in) {
			hooked[in.SessionID] = true
		}
	}
	var sigs []Signal
	var order []int64
	chains := map[int64][]store.SignalInput{}
	flush := func(session int64) {
		if s, ok := chain(chains[session]); ok {
			sigs = append(sigs, s)
		}
		delete(chains, session)
	}
	for _, in := range ins {
		switch {
		case in.Command != nil && eligible(CommandChain, in.Sender):
			if _, ok := chains[in.SessionID]; !ok {
				order = append(order, in.SessionID)
			}
			chains[in.SessionID] = append(chains[in.SessionID], in)
		case hookPrompt(in) || in.Sender == store.SenderYou || !hooked[in.SessionID] && eligible(RepeatedPrompt, in.Sender):
			flush(in.SessionID)
		}
	}
	for _, session := range order {
		flush(session)
	}
	return sigs
}

// hookPrompt reports whether in is a hook prompt record: the CLI fires the prompt hook for each
// prompt that it submits. It ends a turn also when it does not count, as a pane read of the
// same prompt holds the occurrence then (§6.4).
func hookPrompt(in store.SignalInput) bool {
	return in.Source == store.SourceHook && in.Event == promptEvent
}

// chain returns the command chain signal of cmds, the commands of one turn. An immediate rerun
// of the same command in the same folder counts once, as its last run (#337): an agent runs a
// command again after a failed or denied run, as Codex does after its sandbox refuses it and you
// approve, and the Codex hook records have no exit status, so the failed run looks like a run
// that worked. A tool never needs the same command twice in a row.
func chain(cmds []store.SignalInput) (Signal, bool) {
	s := Signal{Kind: CommandChain}
	argv := make([]string, 0, len(cmds))
	for i, c := range cmds {
		if c.Command.SecretFound || c.Command.Truncated {
			return Signal{}, false
		}
		a := strings.Join(strings.Fields(c.Command.Argv), " ")
		if n := len(argv); n > 0 && argv[n-1] == a && cmds[i-1].Command.CWD == c.Command.CWD {
			s.Observations[n-1] = c.ID
			continue
		}
		s.Observations = append(s.Observations, c.ID)
		argv = append(argv, a)
	}
	if len(argv) == 0 {
		return Signal{}, false
	}
	s.Key = key(CommandChain, strings.Join(argv, "\n"))
	return s, true
}

// errorLine matches a line of tool output with an error [NEW]. It is wide on purpose.
var errorLine = regexp.MustCompile(`(?i)\b(error|failed|failure|fatal|panic|exception|traceback|denied|not found|` +
	`no such file|cannot|could not|exit (code|status):? [1-9]\d*)\b`)

// digits matches the numbers that change between two runs of the same failure.
var digits = regexp.MustCompile(`\d+`)

// maxSignature is the longest failure signature in characters [NEW].
const maxSignature = 200

// failures finds errors in tool output, from their first occurrence (§7.1). The 2-session
// gate is E12-S2's (§7.2). A command record fails when its exit status is not 0. Tool text
// fails when a line holds an error; its signature is that line with the numbers taken out. A
// tool hook record holds the command, not its output, so only its exit status counts.
func failures(ins []store.SignalInput) []Signal {
	var sigs []Signal
	for _, in := range ins {
		if !eligible(Failure, in.Sender) {
			continue
		}
		var sig string
		switch {
		case in.Command != nil:
			if in.Command.ExitStatus == nil || *in.Command.ExitStatus == 0 {
				continue
			}
			sig = "exit " + strconv.Itoa(*in.Command.ExitStatus) + "\n" + strings.Join(strings.Fields(in.Command.Argv), " ")
		case in.Counts:
			line := firstMatch(in.Excerpt, errorLine)
			if line == "" {
				continue
			}
			sig = cutRunes(digits.ReplaceAllString(normalize(line), "#"), maxSignature)
		default:
			continue
		}
		sigs = append(sigs, Signal{Kind: Failure, Observations: []int64{in.ID}, Key: key(Failure, sig)})
	}
	return sigs
}

// approvalLine matches a line of an approval screen [NEW, the recorded blocked reads of
// Claude and Codex in internal/brained/testdata]: the question, the reason and the command.
var approvalLine = regexp.MustCompile(`(?i)^\s*(do you want to .*\?|would you like to .*\?|reason: .+|\$ .+)\s*$`)

// blockedApprovals finds approval screens in the reads of a blocked agent (§7.1). Each screen
// is a signal; the same screen for the same command has the same key. M4 uses them; E12
// records them only.
func blockedApprovals(texts []text) []Signal {
	var sigs []Signal
	for _, t := range texts {
		if t.in.Source != store.SourceHerdr || t.in.Event != "blocked" {
			continue
		}
		if screen := dialog(t.in.Excerpt); screen != "" {
			sigs = append(sigs, Signal{Kind: BlockedApproval, Observations: []int64{t.in.ID},
				Key: key(BlockedApproval, screen)})
		}
	}
	return sigs
}

// dialog returns the approval dialog in s, or "": the indented block of lines around the
// first approval line, in lower case with single spaces. Both harnesses indent the dialog,
// and the whole block (the command, the file and its new text) tells two screens apart. The
// agent's own text before it starts at column 0, so it stays out.
func dialog(s string) string {
	lines := strings.Split(s, "\n")
	i := slices.IndexFunc(lines, approvalLine.MatchString)
	if i < 0 {
		return ""
	}
	inBlock := func(l string) bool { return strings.TrimSpace(l) == "" || strings.HasPrefix(l, " ") }
	start, end := i, i+1
	for start > 0 && inBlock(lines[start-1]) {
		start--
	}
	for end < len(lines) && inBlock(lines[end]) {
		end++
	}
	var block []string
	for _, l := range lines[start:end] {
		if l = strings.Join(strings.Fields(strings.ToLower(l)), " "); l != "" {
			block = append(block, l)
		}
	}
	return strings.Join(block, "\n")
}

// key is the hash of signature sig of kind k. It holds no text.
func key(k Kind, sig string) string {
	h := sha256.Sum256([]byte(string(k) + "\x00" + sig))
	return hex.EncodeToString(h[:])
}

// firstMatch returns the first line of s that re matches, or "".
func firstMatch(s string, re *regexp.Regexp) string {
	for _, l := range strings.Split(s, "\n") {
		if re.MatchString(l) {
			return l
		}
	}
	return ""
}

// matches returns the ids of the texts that ok accepts, in order.
func matches(texts []text, ok func(text) bool) []int64 {
	var ids []int64
	for _, t := range texts {
		if ok(t) {
			ids = append(ids, t.in.ID)
		}
	}
	return ids
}
