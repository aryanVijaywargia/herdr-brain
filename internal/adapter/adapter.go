// Package adapter is the base adapter (HLD §8.5): the code that every harness shares. It loads
// a harness profile, reads the hook input by the profile's JSON paths and writes the brief in
// the profile's output form. It knows no harness by name. Package harness computes the
// adapter state (E8-S4).
package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/aryanVijaywargia/herdr-brain/adapters"
)

// formAdditionalContext is the output form of Claude and Codex (HLD §8.3, M0 item 1).
const formAdditionalContext = "hookSpecificOutput.additionalContext"

var (
	// ErrNotStart says that the hook input does not meet the profile's start condition. The
	// hook prints nothing (§8.3 step 2).
	ErrNotStart = errors.New("hook input does not meet the start condition")
	// ErrRetest says that the hook input has a shape that the profile does not know, for
	// example after a CLI update. The hook prints nothing, and the caller sets the adapter
	// state to retest (S14).
	ErrRetest = errors.New("unknown hook input shape: adapter needs retest")
)

// Profile is one harness profile (§8.5), M1 fields only.
type Profile struct {
	Kind          string `toml:"kind"`
	InstallTarget string `toml:"install_target"`
	Events        struct {
		Start string `toml:"start"`
		// The CLI events of the hook records (E11-S5, §6.1). An empty event is off: a CLI gets
		// hook records only after its E11-S1 hook test.
		Prompt      string   `toml:"prompt"`
		Tool        []string `toml:"tool"`
		ToolMatcher string   `toml:"tool_matcher"` // the shell tool: the tool hook runs only for it
		Stop        string   `toml:"stop"`
	} `toml:"events"`
	StartCondition string `toml:"start_condition"`
	OutputForm     string `toml:"output_form"`
	Input          struct {
		SessionID string `toml:"session_id"`
		CWD       string `toml:"cwd"`
		// TranscriptPath is the path of the CLI's transcript file in the start input (E15-S4).
		// It is optional: Brain only stores and prints the path, and never reads the file.
		TranscriptPath string `toml:"transcript_path"`
		// The paths of the hook records (E11-S5).
		Prompt  string `toml:"prompt"`  // the prompt text
		Command string `toml:"command"` // the shell command of the tool
		Error   string `toml:"error"`   // the error text of a failed tool; it may hold the exit status
		Reply   string `toml:"reply"`   // the last assistant message at the stop
		// ExitPattern is a regular expression on the error text; its first group is the exit
		// status. With no match the exit status is unknown.
		ExitPattern string `toml:"exit_pattern"`
	} `toml:"input"`
	TestedVersion string `toml:"tested_version"`
	// AdditionalContextLimit goes into the hook entry when set. Codex sends a longer
	// additionalContext to a file and not to the model; 0 turns the limit off (E8 epic).
	AdditionalContextLimit *int `toml:"additional_context_limit"`
	// NoiseLines are optional regular expressions of screen lines that herdr capture drops,
	// for example the input box (§6.3, E11-S3). A line matches when a pattern matches all of it.
	NoiseLines []string `toml:"noise_lines"`
	// SHA256 is the hex SHA-256 of the profile file. A conformance result holds it, so a
	// changed profile needs a new test (§8.5, E8-S4).
	SHA256 string `toml:"-"`

	// cond is StartCondition, parsed; its path is empty when the profile has no condition.
	cond condition
	// noise is NoiseLines, compiled.
	noise []*regexp.Regexp
	// exit is ExitPattern, compiled; nil when the profile has none.
	exit *regexp.Regexp
}

// condition is a start condition of the form `<JSON path> == <JSON value>`, for example
// `invocationNum == 0` (§8.3 step 2).
type condition struct {
	path  string
	value any
}

// Input is what the base adapter reads from one hook input.
type Input struct {
	SessionID      string
	CWD            string
	TranscriptPath string // "" when the input has none
}

// Load returns the embedded profile of a harness, for example "claude".
func Load(harness string) (*Profile, error) {
	return LoadFS(adapters.Profiles, harness)
}

// LoadFS reads and checks <harness>.toml from fsys. A profile with a key that M1 does not
// know, a missing field or an unknown output form is an error.
func LoadFS(fsys fs.FS, harness string) (*Profile, error) {
	name := harness + ".toml"
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, fmt.Errorf("read profile: %w", err)
	}
	var p Profile
	md, err := toml.Decode(string(data), &p)
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", name, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		return nil, fmt.Errorf("profile %s: unknown field %q", name, keys[0].String())
	}
	if err := p.check(); err != nil {
		return nil, fmt.Errorf("profile %s: %w", name, err)
	}
	sum := sha256.Sum256(data)
	p.SHA256 = hex.EncodeToString(sum[:])
	return &p, nil
}

func (p *Profile) check() error {
	required := []struct{ key, value string }{
		{"kind", p.Kind},
		{"install_target", p.InstallTarget},
		{"events.start", p.Events.Start},
		{"output_form", p.OutputForm},
		{"input.session_id", p.Input.SessionID},
		{"input.cwd", p.Input.CWD},
		{"tested_version", p.TestedVersion},
	}
	for _, r := range required {
		if r.value == "" {
			return fmt.Errorf("%s is empty", r.key)
		}
	}
	if p.OutputForm != formAdditionalContext {
		return fmt.Errorf("unknown output_form %q", p.OutputForm)
	}
	for _, c := range []struct{ event, key, path string }{
		{p.Events.Prompt, "input.prompt", p.Input.Prompt},
		{strings.Join(p.Events.Tool, ""), "input.command", p.Input.Command},
		{p.Events.Stop, "input.reply", p.Input.Reply},
	} {
		if c.event != "" && c.path == "" {
			return fmt.Errorf("%s is empty", c.key)
		}
	}
	if p.Input.ExitPattern != "" {
		re, err := regexp.Compile(p.Input.ExitPattern)
		if err != nil || re.NumSubexp() < 1 {
			return fmt.Errorf("input.exit_pattern %q: want a regular expression with one group", p.Input.ExitPattern)
		}
		p.exit = re
	}
	for _, n := range p.NoiseLines {
		re, err := regexp.Compile(`^(?:` + n + `)$`)
		if err != nil {
			return fmt.Errorf("noise_lines %q: %w", n, err)
		}
		p.noise = append(p.noise, re)
	}
	if p.StartCondition == "" {
		return nil
	}
	path, value, ok := strings.Cut(p.StartCondition, "==")
	path = strings.TrimSpace(path)
	if !ok || path == "" {
		return fmt.Errorf("start_condition %q is not `<path> == <value>`", p.StartCondition)
	}
	if err := json.Unmarshal([]byte(value), &p.cond.value); err != nil {
		return fmt.Errorf("start_condition %q: value: %w", p.StartCondition, err)
	}
	p.cond.path = path
	return nil
}

// Noise reports whether line is screen noise by the profile's noise_lines.
func (p *Profile) Noise(line string) bool {
	for _, re := range p.noise {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// Parse reads the agent session id, the cwd and the transcript path from raw hook input by the
// profile's JSON paths. It returns an error that wraps ErrRetest when the input is not a JSON object, a path
// has no non-empty string, or the start condition's path is missing. It returns ErrNotStart
// when the input does not meet the start condition. On any error the hook prints nothing.
func (p *Profile) Parse(raw []byte) (Input, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Input{}, fmt.Errorf("%w: %w", ErrRetest, err)
	}
	if p.cond.path != "" {
		v, ok := lookup(doc, p.cond.path)
		if !ok {
			return Input{}, fmt.Errorf("%w: no %q for the start condition", ErrRetest, p.cond.path)
		}
		if !reflect.DeepEqual(v, p.cond.value) {
			return Input{}, ErrNotStart
		}
	}
	sessionID, err := text(doc, p.Input.SessionID)
	if err != nil {
		return Input{}, err
	}
	cwd, err := text(doc, p.Input.CWD)
	if err != nil {
		return Input{}, err
	}
	in := Input{SessionID: sessionID, CWD: cwd}
	// A start with no transcript path still delivers; its receipt holds no path (E15-S4).
	if v, ok := lookup(doc, p.Input.TranscriptPath); ok {
		in.TranscriptPath, _ = v.(string) // a value that is not a string, such as null, is no path
	}
	return in, nil
}

// Write writes brief to w in the profile's output form.
func (p *Profile) Write(w io.Writer, brief string) error {
	out := struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}{}
	out.HookSpecificOutput.HookEventName = p.Events.Start
	out.HookSpecificOutput.AdditionalContext = brief
	enc := json.NewEncoder(w)
	// The brief markers hold < and >; the CLI reads JSON, not HTML, so keep them readable.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("write hook output: %w", err)
	}
	return nil
}

// text returns the non-empty string at path in doc, or an error that wraps ErrRetest.
func text(doc map[string]any, path string) (string, error) {
	v, ok := lookup(doc, path)
	s, isString := v.(string)
	if !ok || !isString || s == "" {
		return "", fmt.Errorf("%w: no string at %q", ErrRetest, path)
	}
	return s, nil
}

// lookup follows a dotted JSON path, for example "session.id", through nested objects.
func lookup(doc map[string]any, path string) (any, bool) {
	var v any = doc
	for _, key := range strings.Split(path, ".") {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = obj[key]; !ok {
			return nil, false
		}
	}
	return v, true
}
