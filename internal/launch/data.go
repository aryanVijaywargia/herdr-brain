package launch

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/aryanVijaywargia/herdr-brain/adapters"
)

// rule is what Brain does with a user's flag that clashes with a Brain flag (E24-S1).
type rule string

const (
	// ruleMerge: Brain puts the user's value into its own flag and drops the user's flag.
	ruleMerge rule = "merge"
	// ruleKeep: the user's flag stays, and Brain drops its own flag of the same name.
	ruleKeep rule = "keep"
	// ruleRefuse: Brain says why and starts nothing.
	ruleRefuse rule = "refuse"
	// ruleNoBrain: Brain starts the plain CLI.
	ruleNoBrain rule = "no-brain"
)

// conflict is one conflict rule of the launch data.
type conflict struct {
	Flags  []string `toml:"flags"`
	Value  string   `toml:"value"` // a pattern of the user's value; "" matches any value
	Rule   rule     `toml:"rule"`
	Reason string   `toml:"reason"`
	value  *regexp.Regexp
}

// matches reports whether the user's flag f is one of this rule.
func (c *conflict) matches(f flag) bool {
	return slices.Contains(c.Flags, f.name) && (c.value == nil || c.value.MatchString(f.value))
}

// cliData is the launch data of one CLI: adapters/launch/<kind>.toml.
type cliData struct {
	kind          string
	MinVersion    string     `toml:"min_version"`
	Flags         []string   `toml:"flags"` // the flags of each launch, before the flags of the plan
	Subcommands   []string   `toml:"subcommands"`
	PlainFlags    []string   `toml:"plain_flags"`
	ValueFlags    []string   `toml:"value_flags"`
	OptionalFlags []string   `toml:"optional_flags"` // the flags that take the next arg when it is not a flag
	VariadicFlags []string   `toml:"variadic_flags"`
	Conflicts     []conflict `toml:"conflicts"`
	build         builder
}

// versionRe is a version word of `<cli> --version`: "2.1.312 (Claude Code)", "codex-cli 0.165.0".
var versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Kinds returns the CLI kinds with launch data, one for each adapters/launch/<kind>.toml, in
// name order: `brain <kind>` launches each.
func Kinds() []string {
	// fs.Glob returns only a bad pattern error, and this pattern is good.
	names, _ := fs.Glob(adapters.Launch, "launch/*.toml")
	kinds := make([]string, len(names))
	for i, n := range names {
		kinds[i] = strings.TrimSuffix(path.Base(n), ".toml")
	}
	return kinds
}

// load reads and checks the launch data of CLI kind.
func load(kind string) (*cliData, error) {
	name := "launch/" + kind + ".toml"
	data, err := fs.ReadFile(adapters.Launch, name)
	if err != nil {
		return nil, fmt.Errorf("read launch data: %w", err)
	}
	d := &cliData{kind: kind}
	md, err := toml.Decode(string(data), d)
	if err != nil {
		return nil, fmt.Errorf("launch data %s: %w", name, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		return nil, fmt.Errorf("launch data %s: unknown field %q", name, keys[0].String())
	}
	if err := d.check(); err != nil {
		return nil, fmt.Errorf("launch data %s: %w", name, err)
	}
	return d, nil
}

func (d *cliData) check() error {
	switch d.kind {
	case "claude":
		d.build = buildClaude
	case "codex":
		d.build = buildCodex
	default:
		return fmt.Errorf("no launch builder for %q", d.kind)
	}
	if !versionRe.MatchString(d.MinVersion) {
		return fmt.Errorf("min_version %q is not MAJOR.MINOR.PATCH", d.MinVersion)
	}
	for i := range d.Conflicts {
		c := &d.Conflicts[i]
		switch {
		case len(c.Flags) == 0:
			return fmt.Errorf("conflict %d has no flags", i)
		case !slices.Contains([]rule{ruleMerge, ruleKeep, ruleRefuse, ruleNoBrain}, c.Rule):
			return fmt.Errorf("conflict %d: unknown rule %q", i, c.Rule)
		case c.Reason == "" && (c.Rule == ruleRefuse || c.Rule == ruleNoBrain):
			return fmt.Errorf("conflict %d: rule %s needs a reason", i, c.Rule)
		}
		if c.Value != "" {
			re, err := regexp.Compile(c.Value)
			if err != nil {
				return fmt.Errorf("conflict %d: %w", i, err)
			}
			c.value = re
		}
	}
	return nil
}

// older reports whether version v of `<cli> --version` is older than the min_version of d.
func (d *cliData) older(v string) bool {
	return slices.Compare(versionNums(v), versionNums(d.MinVersion)) < 0
}

// versionNums returns the three numbers of a version that matches versionRe.
func versionNums(v string) []int {
	var nums []int
	for _, s := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(s) // versionRe: digits only
		nums = append(nums, n)
	}
	return nums
}
