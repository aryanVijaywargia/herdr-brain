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
// that failed as "• Failed (exit N) {command}"; the lines of a long command start with │.
// Claude shows "⏺ Bash({command})" (group 2), on more lines for a long command.
var callLine = regexp.MustCompile(`^(?:• (?:Explor(?:ing|ed)|Ran (.+))|⏺ Bash\((.+))$`)

// callMark matches the start of a line that can be a call line: the reply mark of either
// harness.
var callMark = regexp.MustCompile(`^(?:• |⏺ )`)

// failedOutput matches the first line of the output of a Claude command that failed.
var failedOutput = regexp.MustCompile(`^\s*⎿\s+Error: `)

// readers are the programs that print only the text of files, a list of files or their own
// arguments, or that change the folder [NEW, #363]. A run of them prints no error of a run of
// another program. Programs that run other programs, as awk and xargs, are not readers, and
// runsOther names the modes of a reader that do.
var readers = []string{"cat", "cd", "column", "cut", "echo", "egrep", "fgrep", "find", "grep", "head", "jq", "ls",
	"nl", "printf", "rg", "sed", "sort", "tail", "tr", "tree", "uniq", "wc"}

// diagnostic matches a line that a reader or the shell prints about its own error, as
// "cat: notes.md: No such file or directory" or "zsh:1: no such file or directory: x", after
// the tool mark. A pipe can hide the exit status of a reader, so such a line is still an
// error of the run.
var diagnostic = regexp.MustCompile(`^[^\pL\pN]*(?:sh|bash|zsh|` + strings.Join(readers, "|") + `)(?::\d+)?: `)

// linePrint matches a sed script that only prints lines by number, as "1,80p" or "$p;10p".
var linePrint = regexp.MustCompile(`^(?:(?:\d+|\$)(?:,(?:\d+|\$))?p;?)+$`)

// fileText reports whether out, the tool output of a pane read, is text of files that a
// command printed, as a README that the agent reads (#363), with call the pane text above it
// and cut whether call is cut: the last call line of call shows a command that worked and runs
// only readers, and no line of out is an error of a reader or the shell. Such text is no error
// that a run printed, so it starts no failure (§6.5, §7.1). A cut call, a cut command (…), a
// shell expansion, a program that is not a reader, or a reader that runs another program
// keeps out as tool output that can fail.
func fileText(call string, cut bool, out string) bool {
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
	if m == nil || failedOutput.MatchString(outs[0]) || slices.ContainsFunc(outs, diagnostic.MatchString) {
		return false
	}
	if m[1] == "" && m[2] == "" {
		return true // Codex shows no output of an exploring call
	}
	cmd := []string{m[1] + m[2]}
	for _, l := range lines[i+1:] {
		cmd = append(cmd, strings.TrimPrefix(strings.TrimSpace(l), "│ "))
	}
	s := strings.Join(cmd, "\n")
	if m[2] != "" {
		var closed bool
		if s, closed = strings.CutSuffix(s, ")"); !closed {
			return false
		}
	}
	cs, ok := commands(s)
	if !ok || strings.Contains(s, "…") {
		return false
	}
	for _, words := range cs {
		for len(words) > 0 && strings.Contains(words[0], "=") {
			words = words[1:] // a variable for the command
		}
		if len(words) == 0 {
			continue
		}
		prog := path.Base(words[0])
		if !slices.Contains(readers, prog) || runsOther(prog, words[1:]) {
			return false
		}
	}
	return true
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
