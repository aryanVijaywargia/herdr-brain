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
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/aryanVijaywargia/herdr-brain/adapters"
)

// formAdditionalContext is the output form of Claude and Codex (HLD §8.3, M0 item 1). Its
// output also names the start event in hookSpecificOutput.hookEventName.
const formAdditionalContext = "hookSpecificOutput.additionalContext"

// defaultHooksKey is the root key of the hook object in a CLI hook file: Claude and Codex keep
// their events in "hooks" (§8.3).
const defaultHooksKey = "hooks"

// TrustCodex is the hook_trust value of a CLI that runs a new or changed hook entry only after
// you trust it, with the trust state in the hooks.state table of its config.toml, as Codex
// 0.154 does (spike/m0/FINDINGS.md "E8-S3: trust check").
const TrustCodex = "codex"

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
	// StartCondition is one or more checks `<JSON path> == <JSON value>`, joined by "&&" [NEW],
	// for example `invocationNum == 0 && initialNumSteps == 1` (§8.3 step 2, E16-S11).
	StartCondition string `toml:"start_condition"`
	// OutputForm is the JSON path of the brief in the hook output, for example
	// `injectSteps[0].ephemeralMessage` (E16-S11). A path is object keys joined by dots; a key
	// may end in array steps such as "[0]" [NEW syntax].
	OutputForm string `toml:"output_form"`
	// OutputNone [NEW] is the output of a run that delivers nothing, for a CLI that wants JSON
	// on every hook run: agy reads "{}" (spike/m0/FINDINGS.md "E16-S2"). "" prints nothing.
	OutputNone string `toml:"output_none"`
	// HooksKey [NEW] is the root key of the object that holds the CLI events in the hook file:
	// "hooks" when empty, as for Claude and Codex. agy keeps each named hook at its own root
	// key, so its profile names Brain's key (E16-S2).
	HooksKey string `toml:"hooks_key"`
	// FlatHandlers [NEW]: the entry of each event but the tool events is the handler object
	// itself, not a matcher group around it, as agy wants for PreInvocation and Stop (E16-S2).
	FlatHandlers bool `toml:"flat_handlers"`
	Input        struct {
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
	// HookTrust [NEW] names the trust step that the CLI needs before it runs a Brain hook entry:
	// "" for none, or TrustCodex. Setup shows its steps, and doctor checks it (E16-S10).
	HookTrust string `toml:"hook_trust"`
	// AltScreen [NEW] says that the CLI draws in the alternate screen: the eval runner reads its
	// visible screen after idle (E11-S1). An inline CLI keeps its scrollback.
	AltScreen bool `toml:"alt_screen"`
	// Eval [NEW] holds the CLI arguments of a fresh CLI that the eval runner starts (E10-S3).
	Eval struct {
		// Args go to each fresh CLI. "{{dir}}" in an argument is the real path of the
		// runner's scratch folder, as a quoted string.
		Args []string `toml:"args"`
		// SetupArgs also go to a CLI that runs a setup step, so that it may run
		// `brain remember` with no ask: the runner never answers a permission dialog.
		SetupArgs []string `toml:"setup_args"`
		// Dir [NEW] is one fixed scratch folder for the cases that probe this CLI, for a CLI
		// that trusts folders by path and asks again in each new folder, as agy 1.2.16 does
		// (E16-S12). The runner empties it before each case. "" is a fresh temp folder.
		Dir string `toml:"dir"`
	} `toml:"eval"`
	// NoiseLines are optional regular expressions of screen lines that herdr capture drops,
	// for example the input box (§6.3, E11-S3). A line matches when a pattern matches all of it.
	NoiseLines []string `toml:"noise_lines"`
	// NoiseBlocks [NEW] are optional screen parts of more than one line that herdr capture
	// drops, such as a tip that wraps or a start banner (#258). A block is a line that Start
	// matches and each next line that Next matches; a blank line ends it, unless Next matches
	// the empty line (#291). Patterns match a whole line.
	NoiseBlocks []struct {
		Start string `toml:"start"`
		Next  string `toml:"next"`
		// AfterBlank [NEW] makes the block start only under a blank line or at the top of
		// the read: a prompt line with the same text is right under the prompt, so it stays
		// (#316).
		AfterBlank bool `toml:"after_blank"`
	} `toml:"noise_blocks"`
	// NoiseAbove [NEW] are optional screen lines that herdr capture drops only when the next
	// line that is not blank matches Below, such as the placeholder of the empty input box
	// above the rule or the footer of the box (#316): a sent prompt with the same text has
	// other text under it. Patterns match a whole line.
	NoiseAbove []struct {
		Line  string `toml:"line"`
		Below string `toml:"below"`
	} `toml:"noise_above"`
	// SidePanel [NEW] is an optional column that the CLI draws to the right of the main text,
	// such as the Claude diff panel (#258). Header is a regular expression of its header row;
	// herdr capture cuts each line of a read at the column where the header starts.
	SidePanel struct {
		Header string `toml:"header"`
		// MinColumn is the leftmost display column of the panel: a header text further left
		// is main text.
		MinColumn int `toml:"min_column"`
	} `toml:"side_panel"`
	// SHA256 is the hex SHA-256 of the profile file. A conformance result holds it, so a
	// changed profile needs a new test (§8.5, E8-S4).
	SHA256 string `toml:"-"`

	// cond is StartCondition, parsed; empty when the profile has no condition.
	cond []condition
	// noise is NoiseLines, compiled.
	noise []*regexp.Regexp
	// blocks is NoiseBlocks, compiled.
	blocks []noiseBlock
	// above is NoiseAbove, compiled.
	above []noiseAbove
	// panel is SidePanel.Header, compiled; nil when the profile has no side panel.
	panel *regexp.Regexp
	// exit is ExitPattern, compiled; nil when the profile has none.
	exit *regexp.Regexp
}

// condition is one check of a start condition, `<JSON path> == <JSON value>`, for example
// `invocationNum == 0` (§8.3 step 2).
type condition struct {
	path  string
	value any
}

// HookKey returns the root key of the hook object in the CLI hook file.
func (p *Profile) HookKey() string {
	if p.HooksKey == "" {
		return defaultHooksKey
	}
	return p.HooksKey
}

// Input is what the base adapter reads from one hook input.
type Input struct {
	SessionID      string
	CWD            string
	TranscriptPath string // "" when the input has none
}

// Load returns the embedded profile of a harness, for example "claude".
func Load(harness string) (*Profile, error) {
	return LoadFS(nil, harness)
}

// Kinds returns the kind of each profile of fsys, one for each <kind>.toml, in name order. A
// nil fsys is the embedded set (E16-S10): the entry points take this list, so a new CLI needs
// only its profile file (S60).
func Kinds(fsys fs.FS) []string {
	// fs.Glob returns only a bad pattern error, and this pattern is good.
	names, _ := fs.Glob(set(fsys), "*.toml")
	kinds := make([]string, len(names))
	for i, n := range names {
		kinds[i] = strings.TrimSuffix(n, ".toml")
	}
	return kinds
}

// LoadAll returns every profile of fsys, in the order of Kinds. A nil fsys is the embedded set.
func LoadAll(fsys fs.FS) ([]*Profile, error) {
	kinds := Kinds(fsys)
	ps := make([]*Profile, len(kinds))
	for i, k := range kinds {
		p, err := LoadFS(fsys, k)
		if err != nil {
			return nil, err
		}
		ps[i] = p
	}
	return ps, nil
}

// set is fsys, or the embedded profiles when fsys is nil.
func set(fsys fs.FS) fs.FS {
	if fsys == nil {
		return adapters.Profiles
	}
	return fsys
}

// LoadFS reads and checks <harness>.toml from fsys; a nil fsys is the embedded set. A profile
// with a key that M1 does not know, a missing field or an unknown output form is an error.
func LoadFS(fsys fs.FS, harness string) (*Profile, error) {
	name := harness + ".toml"
	data, err := fs.ReadFile(set(fsys), name)
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
	// The entry points find a profile by its file name (Kinds), and key it by its kind.
	if p.Kind != harness {
		return nil, fmt.Errorf("profile %s: kind %q is not the file name", name, p.Kind)
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
	for _, c := range []struct{ key, path string }{
		{"output_form", p.OutputForm},
		{"input.session_id", p.Input.SessionID},
		{"input.cwd", p.Input.CWD},
		{"input.transcript_path", p.Input.TranscriptPath},
		{"input.prompt", p.Input.Prompt},
		{"input.command", p.Input.Command},
		{"input.error", p.Input.Error},
		{"input.reply", p.Input.Reply},
	} {
		if _, err := steps(c.path); c.path != "" && err != nil {
			return fmt.Errorf("%s: %w", c.key, err)
		}
	}
	if p.OutputNone != "" && !json.Valid([]byte(p.OutputNone)) {
		return fmt.Errorf("output_none %q is not JSON", p.OutputNone)
	}
	if p.HookTrust != "" && p.HookTrust != TrustCodex {
		return fmt.Errorf("unknown hook_trust %q", p.HookTrust)
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
	if err := p.checkScreen(); err != nil {
		return err
	}
	if p.StartCondition == "" {
		return nil
	}
	for _, check := range strings.Split(p.StartCondition, "&&") {
		path, value, ok := strings.Cut(check, "==")
		path = strings.TrimSpace(path)
		if !ok || path == "" {
			return fmt.Errorf("start_condition %q is not `<path> == <value>`", check)
		}
		if _, err := steps(path); err != nil {
			return fmt.Errorf("start_condition %q: %w", check, err)
		}
		c := condition{path: path}
		if err := json.Unmarshal([]byte(value), &c.value); err != nil {
			return fmt.Errorf("start_condition %q: value: %w", check, err)
		}
		p.cond = append(p.cond, c)
	}
	return nil
}

// Parse reads the agent session id, the cwd and the transcript path from raw hook input by the
// profile's JSON paths. It returns an error that wraps ErrRetest when the input is not a JSON object, a path
// has no non-empty string, or the path of a start condition check is missing. It returns
// ErrNotStart when the input does not meet each check of the start condition. On any error the
// hook prints no brief.
func (p *Profile) Parse(raw []byte) (Input, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Input{}, fmt.Errorf("%w: %w", ErrRetest, err)
	}
	for _, c := range p.cond {
		v, ok := lookup(doc, c.path)
		if !ok {
			return Input{}, fmt.Errorf("%w: no %q for the start condition", ErrRetest, c.path)
		}
		if !reflect.DeepEqual(v, c.value) {
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

// Write writes brief to w in the profile's output form: the Claude and Codex form with the
// start event name, or else one JSON object that holds brief at the output_form path.
func (p *Profile) Write(w io.Writer, brief string) error {
	var out any
	if p.OutputForm == formAdditionalContext {
		hso := struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}{}
		hso.HookSpecificOutput.HookEventName = p.Events.Start
		hso.HookSpecificOutput.AdditionalContext = brief
		out = hso
	} else {
		out = put(nil, p.OutputForm, brief)
	}
	enc := json.NewEncoder(w)
	// The brief markers hold < and >; the CLI reads JSON, not HTML, so keep them readable.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("write hook output: %w", err)
	}
	return nil
}

// WriteNone writes the profile's output for a run that delivers nothing, and nothing when the
// profile has none.
func (p *Profile) WriteNone(w io.Writer) error {
	if p.OutputNone == "" {
		return nil
	}
	if _, err := io.WriteString(w, p.OutputNone+"\n"); err != nil {
		return fmt.Errorf("write hook output: %w", err)
	}
	return nil
}

// StartInput returns hook start input that Parse reads as sessionID and cwd: the two values at
// the profile's JSON paths, and the value of its start condition. The eval runner calls
// `brain hook` with it (§12.1 exclusion), so input of the profile's own shape does not set the
// adapter to retest.
func (p *Profile) StartInput(sessionID, cwd string) ([]byte, error) {
	doc := put(nil, p.Input.SessionID, sessionID)
	doc = put(doc, p.Input.CWD, cwd)
	for _, c := range p.cond {
		doc = put(doc, c.path, c.value)
	}
	return json.Marshal(doc)
}

// pathRe is one dot-separated part of a JSON path: an object key, then zero or more array
// steps such as "[0]".
var pathRe = regexp.MustCompile(`^([^.\[\]]+)((?:\[\d+\])*)$`)

// stepRe is one array step of a path part.
var stepRe = regexp.MustCompile(`\[(\d+)\]`)

// steps splits a JSON path, for example "injectSteps[0].ephemeralMessage", into its steps: a
// string for an object key, an int for an array index.
func steps(path string) ([]any, error) {
	var out []any
	for _, part := range strings.Split(path, ".") {
		m := pathRe.FindStringSubmatch(part)
		if m == nil {
			return nil, fmt.Errorf("bad JSON path %q", path)
		}
		out = append(out, m[1])
		for _, s := range stepRe.FindAllStringSubmatch(m[2], -1) {
			i, err := strconv.Atoi(s[1])
			if err != nil {
				return nil, fmt.Errorf("bad JSON path %q: %w", path, err)
			}
			out = append(out, i)
		}
	}
	return out, nil
}

// put sets the JSON path in doc to v, makes the objects and arrays on the way, and returns
// the new doc. A nil doc is a new object. The profile check has parsed each path.
func put(doc any, path string, v any) any {
	st, _ := steps(path)
	return assign(doc, st, v)
}

// assign sets the value at steps st under node to v; see put.
func assign(node any, st []any, v any) any {
	if len(st) == 0 {
		return v
	}
	if i, ok := st[0].(int); ok {
		arr, _ := node.([]any)
		for len(arr) <= i {
			arr = append(arr, nil)
		}
		arr[i] = assign(arr[i], st[1:], v)
		return arr
	}
	key, _ := st[0].(string)
	obj, ok := node.(map[string]any)
	if !ok {
		obj = map[string]any{}
	}
	obj[key] = assign(obj[key], st[1:], v)
	return obj
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

// lookup follows a JSON path, for example "session.id" or "workspacePaths[0]", through nested
// objects and arrays.
func lookup(doc map[string]any, path string) (any, bool) {
	st, err := steps(path)
	if err != nil {
		return nil, false
	}
	var v any = doc
	for _, s := range st {
		switch s := s.(type) {
		case string:
			obj, ok := v.(map[string]any)
			if !ok {
				return nil, false
			}
			if v, ok = obj[s]; !ok {
				return nil, false
			}
		case int:
			arr, ok := v.([]any)
			if !ok || s >= len(arr) {
				return nil, false
			}
			v = arr[s]
		}
	}
	return v, true
}
