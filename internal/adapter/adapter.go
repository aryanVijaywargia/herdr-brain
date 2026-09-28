// Package adapter is the base adapter (HLD §8.5): the code that every harness shares. It loads
// a harness profile, reads the hook input by the profile's JSON paths and writes the brief in
// the profile's output form. It knows no harness by name. Storing the adapter state is E8-S4.
package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
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
	} `toml:"events"`
	StartCondition string `toml:"start_condition"`
	OutputForm     string `toml:"output_form"`
	Input          struct {
		SessionID string `toml:"session_id"`
		CWD       string `toml:"cwd"`
	} `toml:"input"`
	TestedVersion string `toml:"tested_version"`

	// cond is StartCondition, parsed; its path is empty when the profile has no condition.
	cond condition
}

// condition is a start condition of the form `<JSON path> == <JSON value>`, for example
// `invocationNum == 0` (§8.3 step 2).
type condition struct {
	path  string
	value any
}

// Input is what the base adapter reads from one hook input.
type Input struct {
	SessionID string
	CWD       string
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

// Parse reads the agent session id and the cwd from raw hook input by the profile's JSON
// paths. It returns an error that wraps ErrRetest when the input is not a JSON object, a path
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
	return Input{SessionID: sessionID, CWD: cwd}, nil
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
