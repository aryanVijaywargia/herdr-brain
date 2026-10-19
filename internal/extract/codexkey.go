package extract

import (
	"cmp"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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

// codexKey returns the name of the API key variable of the model provider that codex uses, from
// $CODEX_HOME/config.toml (~/.codex/config.toml when CODEX_HOME is not set), with getenv the
// environment of Brain (#428): the env_key of [model_providers.<id>] for the top-level
// model_provider, or for the model_provider of the default profile (the top-level profile key,
// which codex exec applies with no --profile). It reads only names, never a value, and runs no
// command. It returns "" for no such key, for a name that is not keyName or is reserved, and for
// a file that is missing or that it cannot read: the key is an extra, and codex itself reports
// its config errors.
func codexKey(getenv func(string) string) string {
	dir := getenv("CODEX_HOME")
	if dir == "" {
		dir = filepath.Join(getenv("HOME"), ".codex")
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		return ""
	}
	m := tomlStrings(string(b))
	provider := m["model_provider"]
	if p := m["profile"]; p != "" {
		provider = cmp.Or(m["profiles."+p+".model_provider"], provider)
	}
	name := m["model_providers."+provider+".env_key"]
	if provider == "" || !keyName.MatchString(name) || slices.Contains(reservedKeys, name) ||
		slices.ContainsFunc(reservedPrefixes, func(p string) bool { return strings.HasPrefix(name, p) }) {
		return ""
	}
	return name
}

// tomlStrings returns the one-line string values of TOML text by their full dotted key, as
// "model_providers.proxy.env_key": the table of the line plus the key. It skips any other value,
// an array of tables and a line that it cannot read. It is not a TOML parser: it reads only what
// codexKey needs.
func tomlStrings(text string) map[string]string {
	m := map[string]string{}
	table := ""
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "[["):
			table = "\x00" // an array of tables: its keys go nowhere
			continue
		case strings.HasPrefix(l, "["):
			end := strings.LastIndex(l, "]")
			if end < 0 {
				table = "\x00"
				continue
			}
			table = dotted(l[1:end])
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok || strings.HasPrefix(l, "#") || table == "\x00" {
			continue
		}
		val, ok := tomlString(strings.TrimSpace(v))
		if !ok {
			continue
		}
		key := dotted(k)
		if table != "" {
			key = table + "." + key
		}
		m[key] = val
	}
	return m
}

// dotted returns TOML key k, as a.b or a."b c", with its parts unquoted and joined by ".".
func dotted(k string) string {
	var parts []string
	var cur strings.Builder
	var quote rune
	for _, r := range strings.TrimSpace(k) {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
		case r == '.':
			parts = append(parts, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return strings.Join(append(parts, strings.TrimSpace(cur.String())), ".")
}

// tomlString returns the text of one-line TOML string v, in double or single quotes, before an
// end comment. A multi-line string or any other value is no string here.
func tomlString(v string) (string, bool) {
	if len(v) < 2 || (v[0] != '"' && v[0] != '\'') || strings.HasPrefix(v, `"""`) || strings.HasPrefix(v, "'''") {
		return "", false
	}
	end := strings.IndexByte(v[1:], v[0])
	if end < 0 {
		return "", false
	}
	return v[1 : 1+end], true
}
