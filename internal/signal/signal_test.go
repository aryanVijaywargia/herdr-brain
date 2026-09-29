package signal

import (
	"reflect"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const (
	you       = store.SenderYou
	unconf    = store.SenderUnconfirmed
	assistant = store.SenderAssistant
	tool      = store.SenderTool
)

// obs is a pane observation fixture: id, session, sender and text.
func obs(id, session int64, sender store.Sender, excerpt string) store.SignalInput {
	return store.SignalInput{Observation: store.Observation{ID: id, Source: store.SourceHerdr, Event: "idle",
		Excerpt: excerpt, Status: store.ObservationComplete, Sender: sender, Counts: true}, SessionID: session}
}

// blocked is a pane observation fixture of a blocked agent.
func blocked(id, session int64, sender store.Sender, excerpt string) store.SignalInput {
	in := obs(id, session, sender, excerpt)
	in.Event = "blocked"
	return in
}

// cmd is a tool hook observation fixture with its command record.
func cmd(id, session int64, argv string, exit ...int) store.SignalInput {
	in := obs(id, session, tool, argv)
	in.Source, in.Event = store.SourceHook, "tool"
	in.Command = &store.CommandRecord{ObservationID: id, Argv: argv}
	if len(exit) > 0 {
		in.Command.ExitStatus = &exit[0]
	}
	return in
}

// with returns in changed by f.
func with(in store.SignalInput, f func(*store.SignalInput)) store.SignalInput {
	f(&in)
	return in
}

// ofKind returns the observation ids of the signals of kind k in sigs.
func ofKind(sigs []Signal, k Kind) [][]int64 {
	var ids [][]int64
	for _, s := range sigs {
		if s.Kind == k {
			ids = append(ids, s.Observations)
		}
	}
	return ids
}

// keys returns the keys of the signals of kind k in sigs.
func keys(sigs []Signal, k Kind) []string {
	var ks []string
	for _, s := range sigs {
		if s.Kind == k {
			ks = append(ks, s.Key)
		}
	}
	return ks
}

type ruleCase struct {
	name string
	ins  []store.SignalInput
	want [][]int64
}

func runRule(t *testing.T, k Kind, cases []ruleCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ofKind(Find(tc.ins), k); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Find %s = %v, want %v", k, got, tc.want)
			}
		})
	}
}

func TestCorrection(t *testing.T) {
	runRule(t, Correction, []ruleCase{
		{"marker", []store.SignalInput{obs(1, 1, unconf, "No, don't use rm here.")}, [][]int64{{1}}},
		{"each marker", []store.SignalInput{obs(1, 1, unconf, "I said pnpm"), obs(2, 1, unconf, "do it again"),
			obs(3, 1, unconf, "use yarn instead"), obs(4, 1, unconf, "that is wrong"), obs(5, 1, unconf, "STOP"),
			obs(6, 1, unconf, "dont push"), obs(7, 1, unconf, "do not push")},
			[][]int64{{1}, {2}, {3}, {4}, {5}, {6}, {7}}},
		{"typographic apostrophe", []store.SignalInput{obs(1, 1, unconf, "don’t touch main")}, [][]int64{{1}}},
		{"your text", []store.SignalInput{obs(1, 1, you, "no, the other file")}, [][]int64{{1}}},
		{"repeat in the session", []store.SignalInput{obs(1, 1, unconf, "Use pnpm for installs."),
			obs(2, 1, unconf, "use pnpm for installs")}, [][]int64{{2, 1}}},
		{"no marker", []store.SignalInput{obs(1, 1, unconf, "Please add a test for the parser.")}, nil},
		{"marker inside a word", []store.SignalInput{obs(1, 1, unconf, "notice the nonce, restop")}, nil},
		{"repeat in another session", []store.SignalInput{obs(1, 1, unconf, "use pnpm for installs"),
			obs(2, 2, unconf, "use pnpm for installs")}, [][]int64{{2, 1}}},
		{"assistant marker", []store.SignalInput{obs(1, 1, assistant, "No, that is wrong. I will stop.")}, nil},
		{"tool marker", []store.SignalInput{obs(1, 1, tool, "no such option; stop")}, nil},
		{"repeat of assistant text", []store.SignalInput{obs(1, 1, assistant, "use pnpm for installs"),
			obs(2, 1, unconf, "use pnpm for installs")}, nil},
		{"gap", []store.SignalInput{with(obs(1, 1, unconf, "no"), func(in *store.SignalInput) {
			in.Status = store.ObservationGap
		})}, nil},
		{"brain origin", []store.SignalInput{with(obs(1, 1, unconf, "no"), func(in *store.SignalInput) {
			in.BrainOrigin = true
		})}, nil},
		{"not counted", []store.SignalInput{with(obs(1, 1, unconf, "no"), func(in *store.SignalInput) {
			in.Counts = false
		})}, nil},
	})
}

const (
	proc1 = "First run go vet on all packages. Then run golangci-lint with a fresh cache. " +
		"Then run the full test suite with the race flag and paste the summary."
	proc2 = "1. Run go vet on all packages\n2. Run golangci-lint with a fresh cache\n" +
		"3. Run the full test suite with the race flag\n4. Paste the summary here please"
	proc3 = "Open the design doc, read the section on signals, then write a short summary " +
		"of each table. After that list the open questions and send them to the owner."
)

func TestProcedure(t *testing.T) {
	runRule(t, Procedure, []ruleCase{
		{"similar in another session", []store.SignalInput{obs(1, 1, unconf, proc1), obs(2, 2, unconf, proc2)},
			[][]int64{{2, 1}}},
		{"three times", []store.SignalInput{obs(1, 1, unconf, proc1), obs(2, 2, unconf, proc2),
			obs(3, 3, unconf, proc1)}, [][]int64{{2, 1}, {3, 1, 2}}},
		{"not similar", []store.SignalInput{obs(1, 1, unconf, proc1), obs(2, 2, unconf, proc3)}, nil},
		{"short", []store.SignalInput{obs(1, 1, unconf, "Run vet. Then lint. Then test."),
			obs(2, 2, unconf, "Run vet. Then lint. Then test.")}, nil},
		{"one step", []store.SignalInput{obs(1, 1, unconf, strings.Repeat("run the whole test suite ", 6)),
			obs(2, 2, unconf, strings.Repeat("run the whole test suite ", 6))}, nil},
		{"earlier assistant text", []store.SignalInput{obs(1, 1, assistant, proc1), obs(2, 2, unconf, proc2)}, nil},
		{"assistant repeat", []store.SignalInput{obs(1, 1, unconf, proc1), obs(2, 2, assistant, proc2)}, nil},
	})
}

func TestRepeatedPrompt(t *testing.T) {
	runRule(t, RepeatedPrompt, []ruleCase{
		{"same in another session", []store.SignalInput{obs(1, 1, unconf, "Reply with only the word ready."),
			obs(2, 2, unconf, "reply with only the word READY")}, [][]int64{{2, 1}}},
		{"same in one session", []store.SignalInput{obs(1, 1, unconf, "run the tests"),
			obs(2, 1, unconf, "run the tests")}, [][]int64{{2, 1}}},
		{"close", []store.SignalInput{obs(1, 1, unconf, "summarize the open pull requests for the brain repo today"),
			obs(2, 2, unconf, "summarize the open pull requests for the brain repo now")}, [][]int64{{2, 1}}},
		{"three times", []store.SignalInput{obs(1, 1, unconf, "deploy"), obs(2, 2, unconf, "deploy"),
			obs(3, 3, unconf, "deploy")}, [][]int64{{2, 1}, {3, 1, 2}}},
		{"different", []store.SignalInput{obs(1, 1, unconf, "run the tests"), obs(2, 2, unconf, "open the docs")}, nil},
		{"not close enough", []store.SignalInput{obs(1, 1, unconf, "fix the login bug in the api"),
			obs(2, 2, unconf, "fix the logout bug in the web app")}, nil},
		{"assistant repeat", []store.SignalInput{obs(1, 1, unconf, "run the tests"),
			obs(2, 2, assistant, "run the tests")}, nil},
		{"tool repeat", []store.SignalInput{obs(1, 1, tool, "run the tests"), obs(2, 2, tool, "run the tests")}, nil},
	})
}

func TestCommandChain(t *testing.T) {
	runRule(t, CommandChain, []ruleCase{
		{"one chain per turn", []store.SignalInput{obs(1, 1, unconf, "check the build"), cmd(2, 1, "go vet ./..."),
			obs(3, 1, assistant, "vet passed"), cmd(4, 1, "go test ./..."), obs(5, 1, unconf, "now lint"),
			cmd(6, 1, "golangci-lint run")}, [][]int64{{2, 4}, {6}}},
		{"sessions apart", []store.SignalInput{cmd(1, 1, "ls"), cmd(2, 2, "pwd"), cmd(3, 1, "cat a"),
			obs(4, 2, unconf, "next"), cmd(5, 2, "ls")}, [][]int64{{2}, {1, 3}, {5}}},
		{"secret held back", []store.SignalInput{cmd(1, 1, "ls"), with(cmd(2, 1, ""), func(in *store.SignalInput) {
			in.Command.SecretFound = true
		})}, nil},
		{"truncated", []store.SignalInput{with(cmd(1, 1, "echo long"), func(in *store.SignalInput) {
			in.Command.Truncated = true
		})}, nil},
		{"tool text with no command record", []store.SignalInput{obs(1, 1, tool, "go test ./...")}, nil},
		{"brain origin", []store.SignalInput{with(cmd(1, 1, "ls"), func(in *store.SignalInput) {
			in.BrainOrigin = true
		})}, nil},
	})
	a := Find([]store.SignalInput{cmd(1, 1, "go vet ./..."), cmd(2, 1, "go  test ./..."),
		cmd(3, 2, "go vet ./..."), cmd(4, 2, "go test ./..."), cmd(5, 3, "go test ./...")})
	if ks := keys(a, CommandChain); len(ks) != 3 || ks[0] != ks[1] || ks[0] == ks[2] {
		t.Errorf("chain keys = %v, want the same key for the same chain in two sessions and another for a third", ks)
	}
}

func TestFailure(t *testing.T) {
	runRule(t, Failure, []ruleCase{
		{"tool error", []store.SignalInput{obs(1, 1, tool, "  ⎿  cat: notes.md: No such file or directory")},
			[][]int64{{1}}},
		{"each session", []store.SignalInput{obs(1, 1, tool, "Error: build failed"),
			obs(2, 2, tool, "Error: build failed")}, [][]int64{{1}, {2}}},
		{"failed command", []store.SignalInput{cmd(1, 1, "go test ./...", 1)}, [][]int64{{1}}},
		{"tool text with no error", []store.SignalInput{obs(1, 1, tool, "ok  internal/store 0.5s")}, nil},
		{"command exit 0", []store.SignalInput{cmd(1, 1, "grep error log.txt", 0)}, nil},
		{"command with no exit status", []store.SignalInput{cmd(1, 1, "grep error log.txt")}, nil},
		{"assistant error", []store.SignalInput{obs(1, 1, assistant, "The build failed with an error.")}, nil},
		{"unconfirmed error", []store.SignalInput{obs(1, 1, unconf, "why does it say fatal error?")}, nil},
		{"not counted", []store.SignalInput{with(obs(1, 1, tool, "fatal: not a git repository"),
			func(in *store.SignalInput) { in.Counts = false })}, nil},
	})
	sigs := Find([]store.SignalInput{obs(1, 1, tool, "panic: index 3 out of range at main.go:12"),
		obs(2, 2, tool, "panic: index 7 out of range at main.go:40"), obs(3, 2, tool, "Error: permission denied"),
		cmd(4, 1, "make", 2), cmd(5, 2, "make", 2), cmd(6, 2, "make", 1)})
	if ks := keys(sigs, Failure); len(ks) != 6 || ks[0] != ks[1] || ks[0] == ks[2] || ks[3] != ks[4] || ks[3] == ks[5] {
		t.Errorf("failure keys = %v, want one key for one signature with other numbers, and another key else", ks)
	}
}

const (
	codexApproval = "  Would you like to run the following command?\n\n  Environment: local\n\n" +
		"  Reason: Do you want to allow touch made.txt to run with escalated permissions?\n\n  $ touch made.txt"
	claudeApproval = " Create file\n hi.txt\n  1 hi\n Do you want to create hi.txt?"
)

func TestBlockedApproval(t *testing.T) {
	runRule(t, BlockedApproval, []ruleCase{
		{"codex screen", []store.SignalInput{blocked(1, 1, assistant, codexApproval)}, [][]int64{{1}}},
		{"claude screen", []store.SignalInput{blocked(1, 1, unconf, claudeApproval)}, [][]int64{{1}}},
		{"not blocked", []store.SignalInput{obs(1, 1, assistant, codexApproval)}, nil},
		{"blocked with no screen", []store.SignalInput{blocked(1, 1, assistant, "• Running touch made.txt")}, nil},
		{"hook record", []store.SignalInput{with(blocked(1, 1, unconf, codexApproval), func(in *store.SignalInput) {
			in.Source = store.SourceHook
		})}, nil},
	})
	other := strings.ReplaceAll(codexApproval, "made.txt", "other.txt")
	sigs := Find([]store.SignalInput{blocked(1, 1, assistant, codexApproval), blocked(2, 2, assistant, codexApproval),
		blocked(3, 2, assistant, other)})
	if ks := keys(sigs, BlockedApproval); len(ks) != 3 || ks[0] != ks[1] || ks[0] == ks[2] {
		t.Errorf("approval keys = %v, want one key for the same screen and command, another for another command", ks)
	}
}

// The whole dialog tells two screens apart: the new text of a file is part of it. The agent's
// text before the dialog is not.
func TestApprovalScreen(t *testing.T) {
	bye := strings.ReplaceAll(claudeApproval, "1 hi", "1 bye")
	said := "• I will run it now.\n  It needs approval.\n\n• Running touch made.txt\n\n" + codexApproval
	sigs := Find([]store.SignalInput{blocked(1, 1, unconf, claudeApproval), blocked(2, 1, unconf, bye),
		blocked(3, 1, assistant, "• I’ll request escalated permissions.\n\n• Running touch made.txt\n\n"+codexApproval),
		blocked(4, 2, assistant, said)})
	if ks := keys(sigs, BlockedApproval); len(ks) != 4 || ks[0] == ks[1] || ks[2] != ks[3] {
		t.Errorf("approval keys = %v, want other keys for other file text, one key when only the agent's text differs", ks)
	}
}

// An assistant quote of a correction starts no correction signal (S37), and no signal at all.
func TestAssistantQuote(t *testing.T) {
	sigs := Find([]store.SignalInput{obs(1, 1, assistant, `You said: "No, don't push to main again. Stop."`),
		obs(2, 1, assistant, `You said: "No, don't push to main again. Stop."`)})
	if len(sigs) != 0 {
		t.Errorf("Find(assistant quote) = %+v, want no signal", sigs)
	}
}

// The same correction-like text from each sender starts a correction only for the
// unconfirmed sender. A real tool error still starts a failure, and command records still
// give a command chain (§6.5).
func TestSenders(t *testing.T) {
	const text = "No, that's wrong. I said use pnpm instead."
	for _, tc := range []struct {
		sender store.Sender
		want   [][]int64
	}{{unconf, [][]int64{{1}}}, {assistant, nil}, {tool, nil}} {
		if got := ofKind(Find([]store.SignalInput{obs(1, 1, tc.sender, text)}), Correction); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Find correction from %s = %v, want %v", tc.sender, got, tc.want)
		}
	}
	sigs := Find([]store.SignalInput{obs(1, 1, tool, text), obs(2, 1, tool, "error: cannot find module pnpm"),
		cmd(3, 1, "pnpm install"), cmd(4, 1, "pnpm test", 1)})
	for k, want := range map[Kind][][]int64{
		Correction:   nil,
		Failure:      {{2}, {4}},
		CommandChain: {{3, 4}},
	} {
		if got := ofKind(sigs, k); !reflect.DeepEqual(got, want) {
			t.Errorf("Find %s with tool senders = %v, want %v", k, got, want)
		}
	}
}

// One failure in one session gives a failure signal: the 2-session gate is E12-S2's.
func TestOneFailure(t *testing.T) {
	sigs := Find([]store.SignalInput{obs(1, 1, unconf, "build it"), obs(2, 1, tool, "fatal error: build failed")})
	if len(sigs) != 1 || sigs[0].Kind != Failure || !reflect.DeepEqual(sigs[0].Observations, []int64{2}) || sigs[0].Key == "" {
		t.Errorf("Find(one failure) = %+v, want one failure signal of observation 2 with a key", sigs)
	}
}

func TestEligible(t *testing.T) {
	for _, k := range []Kind{Correction, Procedure, RepeatedPrompt, CommandChain, Failure} {
		if eligible(k, assistant) {
			t.Errorf("eligible(%s, assistant) = true, want false: assistant text is context only", k)
		}
	}
	for _, k := range []Kind{Correction, Procedure, RepeatedPrompt} {
		if !eligible(k, unconf) || eligible(k, tool) {
			t.Errorf("eligible(%s): want unconfirmed text only, not tool output", k)
		}
	}
	if eligible(Failure, unconf) || !eligible(Failure, tool) || eligible(CommandChain, unconf) {
		t.Error("eligible: want failures and command chains from tool records only")
	}
}

// Each candidate kind is a signal kind, and a sender supports a candidate as it supports a
// signal of that kind (§6.5).
func TestSupports(t *testing.T) {
	for _, k := range []store.CandidateKind{store.CandidateCorrection, store.CandidateFailure,
		store.CandidateProcedure, store.CandidateCommandChain} {
		for _, s := range []store.Sender{you, unconf, assistant, tool} {
			if got, want := Supports(k, s), eligible(Kind(k), s); got != want {
				t.Errorf("Supports(%s, %s) = %v, want %v", k, s, got, want)
			}
		}
	}
	if Supports(store.CandidateCorrection, assistant) || !Supports(store.CandidateFailure, tool) {
		t.Error("Supports: want no correction from assistant text, and failures from tool records")
	}
}
