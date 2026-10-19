package extract

import (
	"cmp"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// keyName is the form of a variable name that codexKey accepts.
var keyName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// reservedKeys and reservedPrefixes are names that codexKey never passes, also when config.toml
// names them: the E13-S1 base, the variables of herdr and claude, and variables that change how
// a process starts.
var (
	reservedKeys     = []string{"HOME", "PATH", "USER", "LANG", "SHELL", "TMPDIR", "IFS", "ENV", "BASH_ENV", "NODE_OPTIONS"}
	reservedPrefixes = []string{"HERDR_", "CLAUDE_", "ANTHROPIC_", "LD_", "DYLD_"}
)

// codexConfig is the part of the codex config.toml that codexKey reads: names only.
type codexConfig struct {
	ModelProvider string `toml:"model_provider"`
	Profile       string `toml:"profile"`
	Profiles      map[string]struct {
		ModelProvider string `toml:"model_provider"`
	} `toml:"profiles"`
	ModelProviders map[string]struct {
		EnvKey string `toml:"env_key"`
	} `toml:"model_providers"`
}

// codexKey returns the name of the API key variable of the model provider that codex uses, from
// $CODEX_HOME/config.toml (~/.codex/config.toml when CODEX_HOME is not set), with getenv the
// environment of Brain (#428): the env_key of [model_providers.<id>] for the top-level
// model_provider, or for the model_provider of the default profile (the top-level profile key,
// which codex exec applies with no --profile). It reads only names, never a value, and runs no
// command. It returns "" for no such key, for a name that is not keyName or is reserved, and for
// a file that is missing, that it cannot read or that is no valid TOML: the key is an extra, and
// codex itself reports its config errors.
func codexKey(getenv func(string) string) string {
	dir := getenv("CODEX_HOME")
	if dir == "" {
		dir = filepath.Join(getenv("HOME"), ".codex")
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		return ""
	}
	var c codexConfig
	if _, err := toml.Decode(string(b), &c); err != nil {
		return ""
	}
	provider := c.ModelProvider
	if c.Profile != "" {
		provider = cmp.Or(c.Profiles[c.Profile].ModelProvider, provider)
	}
	name := c.ModelProviders[provider].EnvKey
	if provider == "" || !keyName.MatchString(name) || slices.Contains(reservedKeys, name) ||
		slices.ContainsFunc(reservedPrefixes, func(p string) bool { return strings.HasPrefix(name, p) }) {
		return ""
	}
	return name
}
