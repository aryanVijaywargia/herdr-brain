package launch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
)

// Plan is what Brain gives one launch: its private hooks, its MCP server and its start brief.
// A builder turns it into files in the launch folder and flags. E24-S2 launches with an empty
// plan; E24-S4 adds the brief, E24-S5 the capture hooks and E24-S6 the MCP server.
type Plan struct {
	Hooks []Hook
	MCP   *Server // nil for none
	// Brief is the start brief. Both builders write it to brief.md in the launch folder; Claude
	// gets it with --append-system-prompt-file, and a Codex SessionStart hook prints it.
	Brief string
}

// Hook is one command hook of the launch.
type Hook struct {
	Event   string // the CLI event: SessionStart, UserPromptSubmit, PostToolUse or Stop
	Matcher string // the tool matcher; "" for none
	Command string
}

// Server is the Brain MCP server of the launch, a stdio server.
type Server struct {
	Command string
	Args    []string
	// EnvVars are the env names that Codex passes to the server. Claude passes its whole env.
	EnvVars []string
}

// builder writes the files of plan p in the launch folder dir and returns the Brain flags
// that name them. user holds the last value of each user flag with a merge rule: the builder
// puts it into its own flag of that name, and the caller then drops the user's flag.
type builder func(dir string, p Plan, user map[string]string) ([]string, error)

const (
	// serverName is the name of the Brain MCP server in the config of the CLI.
	serverName = "brain"
	briefFile  = "brief.md"
	// codexHookSource is the source path of the -c hooks in the Codex trust keys (macOS and
	// Linux, Codex 0.165).
	codexHookSource = "/<session-flags>/config.toml"
)

// buildClaude builds the Claude flags: --settings with the hooks and the allow rule of the
// Brain tools, --mcp-config, and the brief with the system prompt snapshot off, so that a
// later plain resume does not send the brief again (E24-S1).
func buildClaude(dir string, p Plan, user map[string]string) ([]string, error) {
	var flags []string
	if len(p.Hooks) > 0 || p.MCP != nil {
		s, err := claudeSettings(p, user["--settings"])
		if err != nil {
			return nil, err
		}
		path, err := writeJSON(dir, "settings.json", s)
		if err != nil {
			return nil, err
		}
		flags = append(flags, "--settings="+path)
	}
	if p.MCP != nil {
		cfg := map[string]any{"mcpServers": map[string]any{serverName: map[string]any{
			"type": "stdio", "command": p.MCP.Command, "args": append([]string{}, p.MCP.Args...)}}}
		path, err := writeJSON(dir, "mcp.json", cfg)
		if err != nil {
			return nil, err
		}
		flags = append(flags, "--mcp-config="+path)
	}
	if p.Brief == "" {
		return flags, nil
	}
	text := p.Brief
	if f, ok := user["--append-system-prompt-file"]; ok {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("--append-system-prompt-file: %w", err)
		}
		text = strings.TrimRight(string(b), "\n") + "\n\n" + p.Brief
	}
	path, err := writeFile(dir, briefFile, []byte(text))
	if err != nil {
		return nil, err
	}
	return append(flags, "--append-system-prompt-file="+path, "--system-prompt-snapshot=off"), nil
}

// claudeSettings returns the Brain settings of plan p merged into the user's --settings value
// user, a JSON text or the path of a JSON file; "" is none. The Brain hooks come after the
// user's hooks of each event, and the user's other settings stay.
func claudeSettings(p Plan, user string) (map[string]any, error) {
	s := map[string]any{}
	if user != "" {
		var err error
		if s, err = readSettings(user); err != nil {
			return nil, fmt.Errorf("--settings: %w", err)
		}
	}
	for _, h := range p.Hooks {
		hooks, err := object(s, "hooks")
		if err != nil {
			return nil, err
		}
		list, ok := hooks[h.Event].([]any)
		if hooks[h.Event] != nil && !ok {
			return nil, fmt.Errorf("--settings: hooks.%s is not a list", h.Event)
		}
		g := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": h.Command}}}
		if h.Matcher != "" {
			g["matcher"] = h.Matcher
		}
		hooks[h.Event] = append(list, g)
	}
	if p.MCP == nil {
		return s, nil
	}
	perms, err := object(s, "permissions")
	if err != nil {
		return nil, err
	}
	allow, ok := perms["allow"].([]any)
	if perms["allow"] != nil && !ok {
		return nil, errors.New("--settings: permissions.allow is not a list")
	}
	// The rule allows each tool of the Brain server, so manual mode asks before none.
	if rule := "mcp__" + serverName; !slices.Contains(allow, any(rule)) {
		perms["allow"] = append(allow, rule)
	}
	return s, nil
}

// readSettings reads the user's --settings value v: a JSON object, or the path of a file
// that holds one, as Claude reads it.
func readSettings(v string) (map[string]any, error) {
	data := []byte(v)
	if !strings.HasPrefix(strings.TrimSpace(v), "{") {
		var err error
		if data, err = os.ReadFile(v); err != nil {
			return nil, err
		}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber() // keep the numbers of the user's settings as they are
	var s map[string]any
	if err := d.Decode(&s); err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	if s == nil {
		return nil, errors.New("not a JSON object")
	}
	return s, nil
}

// object returns the object at key of s, and adds an empty one when s has none.
func object(s map[string]any, key string) (map[string]any, error) {
	if s[key] == nil {
		s[key] = map[string]any{}
	}
	o, ok := s[key].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("--settings: %s is not an object", key)
	}
	return o, nil
}

// buildCodex builds the Codex -c flags: the Brain MCP server, the hooks, and the trust of the
// hooks in one hooks.state table, so Codex runs them with no trust prompt and writes nothing
// to config.toml (E24-S1). A later hooks.state table would replace the whole table, so all
// the trust entries go in one flag.
func buildCodex(dir string, p Plan, _ map[string]string) ([]string, error) {
	var flags []string
	if p.MCP != nil {
		prefix := "mcp_servers." + serverName + "."
		flags = append(flags,
			"-c", prefix+"command="+tomlString(p.MCP.Command),
			"-c", prefix+"args="+tomlArray(p.MCP.Args),
			"-c", prefix+"env_vars="+tomlArray(p.MCP.EnvVars))
	}
	hooks, err := codexHooks(p.Hooks)
	if err != nil {
		return nil, err
	}
	flags = append(flags, hooks...)
	if p.Brief != "" {
		if _, err := writeFile(dir, briefFile, []byte(p.Brief)); err != nil {
			return nil, err
		}
	}
	return flags, nil
}

// codexHooks returns the -c flags of the hooks hs: one hooks.<Event> array for each event, in
// the order of hs, and one hooks.state table with the trusted hash of each group. Each hook
// is its own group; additionalContextLimit 0 makes Codex send a brief inline.
func codexHooks(hs []Hook) ([]string, error) {
	var events, state []string
	groups := map[string][]string{}
	limit := uint64(0)
	for _, h := range hs {
		if _, ok := groups[h.Event]; !ok {
			events = append(events, h.Event)
		}
		var matcher *string
		g := "{"
		if h.Matcher != "" {
			matcher = &h.Matcher
			g += "matcher=" + tomlString(h.Matcher) + ","
		}
		g += `hooks=[{type="command",command=` + tomlString(h.Command) + `,additionalContextLimit=0}]}`
		label, sum, err := harness.CodexHookHash(h.Event, matcher, h.Command, &limit)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s:%s:%d:0", codexHookSource, label, len(groups[h.Event]))
		state = append(state, tomlString(key)+"={trusted_hash="+tomlString(sum)+"}")
		groups[h.Event] = append(groups[h.Event], g)
	}
	var flags []string
	for _, e := range events {
		flags = append(flags, "-c", "hooks."+e+"=["+strings.Join(groups[e], ",")+"]")
	}
	if len(state) > 0 {
		flags = append(flags, "-c", "hooks.state={"+strings.Join(state, ",")+"}")
	}
	return flags, nil
}

// tomlString is s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// tomlArray is ss as a TOML array of basic strings.
func tomlArray(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = tomlString(s)
	}
	return "[" + strings.Join(q, ",") + "]"
}

// writeJSON writes v as JSON to the private file name in the launch folder dir.
func writeJSON(dir, name string, v any) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return writeFile(dir, name, append(data, '\n'))
}

// writeFile writes data to the new file name in the launch folder dir, mode 0600, and
// returns its path. A launch file can hold the user's settings, so only you may read it.
func writeFile(dir, name string, data []byte) (string, error) {
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if err = errors.Join(err, f.Close()); err != nil {
		return "", err
	}
	return path, nil
}
