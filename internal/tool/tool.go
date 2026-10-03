// Package tool reads the contract of a Brain tool (HLD §10.1): its tool.toml, and the reviewed
// run.sh when a pipeline needs one. It refuses a contract that puts an input where it could
// change the command, and it gives the one hash of the whole contract that your approval
// covers. It does not run a tool: brain run (E17-S4) does.
package tool

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// The files of one tool revision (§10.1).
const (
	TomlName   = "tool.toml"
	ScriptName = "run.sh"
)

// scriptLine starts the run.sh part of a tool text. [NEW] the text form: the text of a tool
// revision is its tool.toml, then, only when a pipeline needs one, this line and the run.sh.
// So the text holds the whole contract, and the files are the text in two parts.
const scriptLine = "--- " + ScriptName + " ---\n"

// Contract is a checked tool contract.
type Contract struct {
	Name        string           `toml:"name"`
	Description string           `toml:"description"`
	Cwd         string           `toml:"cwd"`
	TimeoutS    int64            `toml:"timeout_s"`
	Effects     string           `toml:"effects"`
	ReadOnly    bool             `toml:"read_only"` // the reviewed read-only restriction [NEW name]
	Argv        []string         `toml:"argv"`
	Steps       [][]string       `toml:"steps"`
	Inputs      map[string]Input `toml:"inputs"`
	// Script is the run.sh text, or "" when the tool has none.
	Script string `toml:"-"`
}

// Input is the schema of one input: a type and a pattern that each value must match.
type Input struct {
	Type    string `toml:"type"`
	Pattern string `toml:"pattern"`
}

// The input types. [NEW] the set.
var inputTypes = []string{"string", "integer"}

var (
	// name is the form of a tool name, the same as a skill name. [NEW]
	name = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	// inputName is the form of an input name. [NEW]
	inputName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// placeholder is an input in an argv element or in run.sh: {name}. [NEW] the form, from
	// the §10.1 example.
	placeholder = regexp.MustCompile(`\{([a-z][a-z0-9_]*)\}`)
)

// shells are the programs whose -c flag takes a command text. [NEW] the list.
var shells = []string{"sh", "bash", "zsh", "dash", "ksh"}

// Split cuts a tool text into its tool.toml and its run.sh ("" when it has none).
func Split(text string) (toml, script string) {
	toml, script, ok := strings.Cut(text, "\n"+scriptLine)
	if ok {
		toml += "\n"
	}
	return toml, script
}

// Files are the files of the tool text: tool.toml, and run.sh when the text has one. Their
// bytes joined back give the text.
func Files(text string) map[string]string {
	toml, script := Split(text)
	fs := map[string]string{TomlName: toml}
	if script != "" {
		fs[ScriptName] = script
	}
	return fs
}

// Hash is the one hash of the whole contract of a tool revision (§10.1): its text (the
// command bytes, the input schema, the cwd, the timeout, the effects and the restriction)
// and its scope. [NEW] the input: the lines "scope: <scope>" and "workspace: <key>", an empty
// line, then the text.
func Hash(text, scope, workspaceKey string) string {
	sum := sha256.Sum256([]byte("scope: " + scope + "\nworkspace: " + workspaceKey + "\n\n" + text))
	return hex.EncodeToString(sum[:])
}

// Parse reads and checks a tool text. It refuses an unknown or missing field, and each
// contract that breaks a §10.1 rule: an input in the first argv element, an input that fills
// only part of an element, an input in a command position, an input with no type or no
// pattern, and a run.sh that puts an input in its text.
func Parse(text string) (Contract, error) {
	tomlText, script := Split(text)
	var c Contract
	md, err := toml.NewDecoder(strings.NewReader(tomlText)).Decode(&c)
	if err != nil {
		return Contract{}, fmt.Errorf("%s: %w", TomlName, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		return Contract{}, fmt.Errorf("%s: unknown field %s", TomlName, keys[0])
	}
	c.Script = script
	if !md.IsDefined("read_only") {
		return Contract{}, fmt.Errorf("%s: read_only is missing: say true or false", TomlName)
	}
	return c, c.check()
}

func (c Contract) check() error {
	switch {
	case !name.MatchString(c.Name):
		return fmt.Errorf("%s: name %q: use lower-case letters, digits and hyphens", TomlName, c.Name)
	case strings.TrimSpace(c.Description) == "":
		return fmt.Errorf("%s: description is missing", TomlName)
	case strings.TrimSpace(c.Cwd) == "":
		return fmt.Errorf("%s: cwd is missing", TomlName)
	case c.TimeoutS < 1:
		return fmt.Errorf("%s: timeout_s %d: want 1 or more", TomlName, c.TimeoutS)
	case strings.TrimSpace(c.Effects) == "":
		return fmt.Errorf("%s: effects is missing", TomlName)
	case (c.Argv == nil) == (c.Steps == nil):
		return fmt.Errorf("%s: want argv or steps, not both and not none", TomlName)
	}
	for n, in := range c.Inputs {
		if err := in.check(n); err != nil {
			return err
		}
	}
	steps := c.Steps
	if c.Argv != nil {
		steps = [][]string{c.Argv}
	}
	if len(steps) == 0 {
		return fmt.Errorf("%s: steps is empty", TomlName)
	}
	for i, argv := range steps {
		if err := c.checkArgv(argv); err != nil {
			return fmt.Errorf("%s: step %d: %w", TomlName, i+1, err)
		}
	}
	return c.checkScript(steps)
}

func (in Input) check(n string) error {
	switch {
	case !inputName.MatchString(n):
		return fmt.Errorf("%s: input %q: use a lower-case letter, then letters, digits and _", TomlName, n)
	case !slices.Contains(inputTypes, in.Type):
		return fmt.Errorf("%s: input %s: type %q: want one of %s", TomlName, n, in.Type, strings.Join(inputTypes, ", "))
	case in.Pattern == "":
		return fmt.Errorf("%s: input %s has no pattern", TomlName, n)
	}
	if _, err := regexp.Compile(in.Pattern); err != nil {
		return fmt.Errorf("%s: input %s: pattern: %w", TomlName, n, err)
	}
	return nil
}

// checkArgv checks one argv list. An input fills one whole element, never the first one, and
// never an element after a shell's -c flag or after eval: there it would be command text.
func (c Contract) checkArgv(argv []string) error {
	if len(argv) == 0 {
		return errors.New("argv is empty")
	}
	shell, command := false, false
	for i, el := range argv {
		ms := placeholder.FindAllStringSubmatch(el, -1)
		for _, m := range ms {
			if _, ok := c.Inputs[m[1]]; !ok {
				return fmt.Errorf("element %d: input %s has no type and no pattern: add [inputs.%s]", i+1, m[1], m[1])
			}
		}
		if len(ms) > 0 {
			switch {
			case i == 0:
				return fmt.Errorf("element 1 %q: an input never fills the first element", el)
			case len(ms) > 1 || ms[0][0] != el:
				return fmt.Errorf("element %d %q: an input fills one whole element", i+1, el)
			case command:
				return fmt.Errorf("element %d %q: an input never fills a command position (after sh -c, bash -c or eval)", i+1, el)
			}
			continue
		}
		base := path.Base(el)
		switch {
		case base == "eval":
			command = true
		case slices.Contains(shells, base):
			shell = true
		case shell && cFlag(el):
			command = true
		}
	}
	return nil
}

// cFlag reports whether el is a short flag group with c, such as -c or -lc.
func cFlag(el string) bool {
	f, ok := strings.CutPrefix(el, "-")
	return ok && f != "" && !strings.HasPrefix(f, "-") && strings.Contains(f, "c")
}

// checkScript checks the run.sh: only a step that runs it needs one, and an input reaches it
// as "$1", "$2", never inside its text.
func (c Contract) checkScript(steps [][]string) error {
	uses := slices.ContainsFunc(steps, func(argv []string) bool { return argv[0] == ScriptName })
	switch {
	case uses && c.Script == "":
		return fmt.Errorf("%s: a step runs %s, but the tool has none", TomlName, ScriptName)
	case !uses && c.Script != "":
		return fmt.Errorf("%s: no step runs it: a %s is only for a pipeline", ScriptName, ScriptName)
	}
	if m := placeholder.FindString(c.Script); m != "" {
		return fmt.Errorf("%s: %s is in the text: an input reaches %s as \"$1\", \"$2\"", ScriptName, m, ScriptName)
	}
	return nil
}
