package signal

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

// callLine matches the line of a pane read that shows a shell call above its tool output [NEW,
// the recorded reads in internal/brained/testdata, #363, M2 eval run 5]. Codex shows a read, a
// search or a list of files as "• Exploring" and then "• Explored", with the files and the
// search words under it, a command that worked as "• Ran {command}" (group 1) and a command
// that failed as "• Failed (exit {status}) {command}" (groups 2 and 3, M2 eval run 6); the lines
// of a long command start with │. Claude shows "⏺ Bash({command})" (group 4), on more lines for
// a long command.
var callLine = regexp.MustCompile(`^(?:• (?:Explor(?:ing|ed)|Ran (.+)|Failed \(exit (\d+)\) (.+))|⏺ Bash\((.+))$`)

// callMark matches the start of a line that can be a call line: the reply mark of either
// harness.
var callMark = regexp.MustCompile(`^(?:• |⏺ )`)

// failedOutput matches the first line of the output of a Claude command that failed, with its
// exit status in group 1 when Claude shows it.
var failedOutput = regexp.MustCompile(`^\s*⎿\s+Error: (?:Exit code (\d+)\s*$)?`)

// readers are the programs that print only the text of files, a list of files or their own
// arguments, or that change the folder [NEW, #363]. A run of them prints no error of a run of
// another program. Programs that run other programs, as awk and xargs, are not readers, and
// runsOther names the modes of a reader that do.
var readers = []string{"cat", "cd", "column", "cut", "echo", "egrep", "fgrep", "find", "grep", "head", "jq", "ls",
	"nl", "printf", "rg", "sed", "sort", "tail", "tr", "tree", "uniq", "wc"}

// searches are the readers that exit with status 1 when they find no match, and print nothing
// about it (#375). They print a line of their own and exit with status 2 on an error.
var searches = []string{"egrep", "fgrep", "grep", "rg"}

// brainProgram is the name of Brain's own command. Its output, as recall items, status lines and
// its refusals, is text of Brain, not of a run in the project (#376), except for brainRuns.
const brainProgram = "brain"

// brainRuns reports whether brain with args runs a tool of the project and prints its output:
// brain run (§10.1) and brain automation launch (§10.4).
func brainRuns(args []string) bool {
	return len(args) > 0 && (args[0] == "run" || args[0] == "automation" && len(args) > 1 && args[1] == "launch")
}

// diagnostic matches a line that a reader or the shell prints about its own error, as
// "cat: notes.md: No such file or directory" or "zsh:1: no such file or directory: x", after
// the tool mark. A pipe can hide the exit status of a reader, so such a line is still an
// error of the run.
var diagnostic = regexp.MustCompile(`^[^\pL\pN]*(?:sh|bash|zsh|` + strings.Join(readers, "|") + `)(?::\d+)?: `)

// linePrint matches a sed script that only prints lines by number, as "1,80p" or "$p;10p".
var linePrint = regexp.MustCompile(`^(?:(?:\d+|\$)(?:,(?:\d+|\$))?p;?)+$`)

// noFailure reports whether out, the tool output of a pane read, holds no failure of a run, with
// call the pane text above it and cut whether call is cut. The last call line of call shows the
// whole command, and no line of out is an error of a reader or the shell, and one of these holds:
//   - every program of the command is a reader, and the command worked: out is text of files
//     that the command printed, as a README that the agent reads (#363);
//   - every program is a reader, at least one is a search, and the command failed with status 1:
//     a search found no match (#375). Every other reader that fails prints a diagnostic line;
//   - every program is brain or a reader, and at least one is brain: out is Brain's own text,
//     also a refusal with status 1 (#376).
//
// Such text starts no failure (§6.5, §7.1). A cut call, a cut command (…), a shell expansion, a
// program that is not a reader, or a reader that runs another program keeps out as tool output
// that can fail. A run that exits with status 0 can still hide a failure, as find -exec does.
func noFailure(call string, cut bool, out string) bool {
	lines := strings.Split(call, "\n")
	i := len(lines) - 1
	for i >= 0 && !callMark.MatchString(lines[i]) {
		i--
	}
	if cut || i < 0 {
		return false
	}
	m := callLine.FindStringSubmatch(lines[i])
	outs := strings.Split(out, "\n")
	if m == nil || slices.ContainsFunc(outs, diagnostic.MatchString) {
		return false
	}
	var worked bool
	var status string // the exit status of a command that failed, "" when not known
	switch {
	case m[1] != "":
		worked = true
	case m[3] != "":
		status = m[2]
	case m[4] != "":
		f := failedOutput.FindStringSubmatch(outs[0])
		worked = f == nil
		if f != nil {
			status = f[1]
		}
	default:
		return true // Codex shows no output of an exploring call
	}
	cmd := []string{m[1] + m[3] + m[4]}
	for _, l := range lines[i+1:] {
		cmd = append(cmd, strings.TrimPrefix(strings.TrimSpace(l), "│ "))
	}
	s := strings.Join(cmd, "\n")
	if m[4] != "" {
		var closed bool
		if s, closed = strings.CutSuffix(s, ")"); !closed {
			return false
		}
	}
	cs, ok := commands(s)
	if !ok || strings.Contains(s, "…") {
		return false
	}
	reads, search, brain := programs(cs)
	return reads && (brain || worked || search && status == "1")
}

// programs reports, for cs, the simple commands of a shell command, whether every program that
// is not brain is a reader that runs no other program, whether one of them is a search, and
// whether one is brain. A brain that runs a tool is no reader.
func programs(cs [][]string) (reads, search, brain bool) {
	reads = true
	for _, words := range cs {
		for len(words) > 0 && strings.Contains(words[0], "=") {
			words = words[1:] // a variable for the command
		}
		if len(words) == 0 {
			continue
		}
		switch prog := path.Base(words[0]); {
		case prog == brainProgram && !brainRuns(words[1:]):
			brain = true
		case !slices.Contains(readers, prog) || runsOther(prog, words[1:]):
			reads = false
		default:
			search = search || slices.Contains(searches, prog)
		}
	}
	return reads, search, brain
}

// brainCall reports whether shell command s runs brain, with no program other than readers
// (#376).
func brainCall(s string) bool {
	cs, ok := commands(s)
	if !ok {
		return false
	}
	reads, _, brain := programs(cs)
	return reads && brain
}

// commands splits shell command s into its simple commands, each as its words without quotes:
// at the pipes, the lists (&&, ||, ;, &) and the new lines outside quotes. An & of a redirect,
// as in 2>&1 or &>, does not split it. It reports false when s expands a variable or the
// output of a command outside single quotes ($, `, <( or >(): its words are not known.
func commands(s string) ([][]string, bool) {
	var cs [][]string
	var words []string
	var w strings.Builder
	word := func() {
		if w.Len() > 0 {
			words = append(words, w.String())
			w.Reset()
		}
	}
	var quote rune
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote == '\'' && r != '\'':
			w.WriteRune(r)
		case r == '$' || r == '`' || quote == 0 && (r == '<' || r == '>') && i+1 < len(rs) && rs[i+1] == '(':
			return nil, false
		case r == '\\' && i+1 < len(rs):
			i++
			w.WriteRune(rs[i])
		case r == quote:
			quote = 0
		case quote != 0:
			w.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case r == '|' || r == ';' || r == '\n' || r == '&' && !redirect(rs, i):
			if i+1 < len(rs) && rs[i+1] == r && r != '\n' {
				i++
			}
			word()
			cs, words = append(cs, words), nil
		case r == ' ' || r == '\t':
			word()
		default:
			w.WriteRune(r)
		}
	}
	word()
	return append(cs, words), true
}

// redirect reports whether the & at rs[i] is part of a redirect, as in 2>&1 or &>.
func redirect(rs []rune, i int) bool {
	return i > 0 && (rs[i-1] == '>' || rs[i-1] == '<') || i+1 < len(rs) && rs[i+1] == '>'
}

// runsOther reports whether reader prog runs another program with args: find with -exec or
// -ok, rg with --pre, sort with --compress-program, or sed with a script that does not only
// print lines by number (GNU sed runs a command with its e command).
func runsOther(prog string, args []string) bool {
	has := func(prefixes ...string) bool {
		return slices.ContainsFunc(args, func(a string) bool {
			return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(a, p) })
		})
	}
	switch prog {
	case "find":
		return has("-exec", "-ok")
	case "rg":
		return has("--pre")
	case "sort":
		return has("--compress-program")
	case "sed":
		return !printsLines(args)
	}
	return false
}

// printsLines reports whether args, the arguments of sed, hold scripts that only print lines
// by number. A script file is not known.
func printsLines(args []string) bool {
	scripts := 0
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-e" || a == "--expression":
			if i+1 == len(args) || !linePrint.MatchString(args[i+1]) {
				return false
			}
			i++
			scripts++
		case strings.HasPrefix(a, "-f") || strings.HasPrefix(a, "--file"):
			return false
		case strings.HasPrefix(a, "-"):
		case scripts == 0:
			if !linePrint.MatchString(a) {
				return false
			}
			scripts++
		}
	}
	return scripts > 0
}
