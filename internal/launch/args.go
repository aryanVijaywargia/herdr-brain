package launch

import (
	"fmt"
	"slices"
	"strings"
)

// flag is one flag of the user's args.
type flag struct {
	name  string // such as "--settings" or "-c"
	value string // its value; "" for none, and for the values of a variadic flag
	// first and end: the user's args [first, end) hold the flag and its values.
	first, end int
	// top: the flag comes before the subcommand, or there is none, so it is a flag of the CLI
	// itself. A prompt does not end the top level: `codex hi --no-daemon` is a top-level flag.
	top bool
}

// args are the user's args of one `brain <cli>` call, read with the launch data of the CLI
// the way the CLI reads them, well enough to find the subcommand and the conflicts.
type args struct {
	d     *cliData
	raw   []string
	flags []flag
	sub   string // the subcommand, or "" for none
}

// parse reads the user's args raw of the CLI of d. The first positional arg is the subcommand
// when it is one of the subcommands or Brain subcommands of d; else it is the prompt. After
// "--" each arg is positional.
func parse(d *cliData, raw []string) *args {
	a := &args{d: d, raw: raw}
	top, positional := true, false
	for i := 0; i < len(raw); i++ {
		arg := raw[i]
		if arg == "--" {
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			if !positional && slices.Contains(d.Subcommands, arg) {
				a.sub = arg
			}
			if !positional && (a.sub != "" || slices.Contains(d.BrainSubcommands, arg)) {
				top = false
			}
			positional = true
			continue
		}
		first := i
		fs, attached := d.split(arg)
		if !attached {
			i = d.values(raw, i, &fs[len(fs)-1])
		}
		for _, f := range fs {
			f.first, f.end, f.top = first, i+1, top
			a.flags = append(a.flags, f)
		}
	}
	return a
}

// split reads the flag arg: "--name", "--name=value", "-c", "-cVALUE" or "-c=VALUE" of a short
// flag that takes a value, or a cluster of short flags such as "-pc". attached reports that
// arg holds the value of its last flag.
func (d *cliData) split(arg string) (fs []flag, attached bool) {
	if strings.HasPrefix(arg, "--") {
		name, value, ok := strings.Cut(arg, "=")
		return []flag{{name: name, value: value}}, ok
	}
	for i := 1; i < len(arg); i++ {
		f := flag{name: "-" + arg[i:i+1]}
		if rest := arg[i+1:]; rest != "" && d.takesValue(f.name) {
			f.value = strings.TrimPrefix(rest, "=")
			return append(fs, f), true
		}
		fs = append(fs, f)
	}
	return fs, false
}

// takesValue reports whether the flag name can take a value.
func (d *cliData) takesValue(name string) bool {
	return slices.Contains(d.ValueFlags, name) || slices.Contains(d.OptionalFlags, name) ||
		slices.Contains(d.VariadicFlags, name)
}

// values reads the values of the flag f at raw[i] from the next args, and returns the index
// of the last arg of f: a value flag takes the next arg, an optional flag the next arg that is
// not a flag, and a variadic flag each next arg up to the next flag.
func (d *cliData) values(raw []string, i int, f *flag) int {
	next := func() bool { return i+1 < len(raw) && !strings.HasPrefix(raw[i+1], "-") }
	switch {
	case slices.Contains(d.ValueFlags, f.name):
		if i+1 < len(raw) {
			i++
			f.value = raw[i]
		}
	case slices.Contains(d.OptionalFlags, f.name):
		if next() {
			i++
			f.value = raw[i]
		}
	case slices.Contains(d.VariadicFlags, f.name):
		for next() {
			i++
		}
	}
	return i
}

// conflict returns the conflict of rule r that the user's flag f matches, or nil.
func (d *cliData) conflict(f flag, r rule) *conflict {
	for i := range d.Conflicts {
		if c := &d.Conflicts[i]; c.Rule == r && c.matches(f) {
			return c
		}
	}
	return nil
}

// plain returns why the args get no Brain flags, or "" for a form that gets them: a
// subcommand, a plain-launch flag or a no-Brain conflict.
func (a *args) plain() string {
	if a.sub != "" {
		return "the subcommand " + a.sub
	}
	for _, f := range a.flags {
		if slices.Contains(a.d.PlainFlags, f.name) {
			return "the flag " + f.name
		}
		if c := a.d.conflict(f, ruleNoBrain); c != nil {
			return text(f) + ": " + c.Reason
		}
	}
	return ""
}

// refused returns the error of the first refuse conflict of the args, or nil.
func (a *args) refused() error {
	for _, f := range a.flags {
		if c := a.d.conflict(f, ruleRefuse); c != nil {
			return fmt.Errorf("%s: %s, so Brain starts nothing; remove it, or run %s without brain", text(f), c.Reason, a.d.kind)
		}
	}
	return nil
}

// session returns the Claude session that the user's args name with its ID: the value of the
// last --session-id, else of the last --resume or -r, unless --fork-session gives the resumed
// session a new ID. "" when they name none: a new session, --continue, or --resume with no ID,
// which picks a session that Brain does not know before the start.
func (a *args) session() string {
	var id, resume string
	fork := false
	for _, f := range a.flags {
		switch {
		case !f.top:
		case f.name == "--session-id":
			id = f.value
		case f.name == "--resume" || f.name == "-r":
			resume = f.value
		case f.name == "--fork-session":
			fork = true
		}
	}
	if id != "" || fork {
		return id
	}
	return resume
}

// text is the flag f as the user gave it, for a message.
func text(f flag) string {
	if f.value == "" {
		return f.name
	}
	return f.name + " " + f.value
}

// argv returns the args of the CLI for a launch with plan p in the launch folder dir: the
// Brain flags, then the user's args with no change. A keep rule drops the Brain flag of the
// same name, and notes holds its reason. A merge rule drops the user's flag when the builder
// put its value into a Brain flag of the same name.
func (a *args) argv(dir string, p Plan) (argv, notes []string, err error) {
	user := map[string]string{}
	for _, f := range a.flags {
		if a.d.conflict(f, ruleMerge) != nil {
			user[f.name] = f.value // the CLI takes the last one
		}
	}
	built, err := a.d.build(dir, p, user)
	if err != nil {
		return nil, nil, err
	}
	brain := append(slices.Clone(a.d.Flags), built...)
	has := func(name string) bool {
		return slices.ContainsFunc(brain, func(b string) bool { return flagName(b) == name })
	}
	drop := make([]bool, len(a.raw))
	for _, f := range a.flags {
		if c := a.d.conflict(f, ruleKeep); c != nil && f.top && has(f.name) {
			brain = slices.DeleteFunc(brain, func(b string) bool { return flagName(b) == f.name })
			if c.Reason != "" {
				notes = append(notes, c.Reason)
			}
		}
		if a.d.conflict(f, ruleMerge) != nil && has(f.name) {
			for i := f.first; i < f.end; i++ {
				drop[i] = true
			}
		}
	}
	for i, arg := range a.raw {
		if !drop[i] {
			brain = append(brain, arg)
		}
	}
	return brain, notes, nil
}

// flagName is the name of the Brain flag b: the part before "=".
func flagName(b string) string {
	name, _, _ := strings.Cut(b, "=")
	return name
}
