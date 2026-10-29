package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/aryanVijaywargia/herdr-brain/internal/install"
)

// Trust is the Codex trust state of the Brain hook entry (E8-S3 trust test, the status
// rule of Codex 0.154).
type Trust string

// The trust states. Codex runs the hook only when it is Trusted.
const (
	Trusted   Trust = "trusted"   // the stored trusted_hash equals the hash of the entry
	Modified  Trust = "modified"  // a stored trusted_hash of another hash: the entry changed
	Untrusted Trust = "untrusted" // no stored trusted_hash for the key
	Disabled  Trust = "disabled"  // trusted, but its state sets enabled = false
	// NoEntry: the hooks.json that Codex reads holds no group with the recorded hash, and no
	// group of one handler with the recorded command.
	NoEntry Trust = "no entry"
)

// defaultContextLimit is DEFAULT_HOOK_OUTPUT_TOKEN_LIMIT in Codex 0.154. Codex drops an
// additionalContextLimit equal to it before it hashes (E8-S3).
const defaultContextLimit = 2500

// eventLabels maps a hooks.json event name to its trust key label (hooks/src/lib.rs, E8-S3).
var eventLabels = map[string]string{
	"PreToolUse":        "pre_tool_use",
	"PermissionRequest": "permission_request",
	"PostToolUse":       "post_tool_use",
	"PreCompact":        "pre_compact",
	"PostCompact":       "post_compact",
	"SessionStart":      "session_start",
	"SessionEnd":        "session_end",
	"UserPromptSubmit":  "user_prompt_submit",
	"SubagentStart":     "subagent_start",
	"SubagentStop":      "subagent_stop",
	"Stop":              "stop",
	"Interrupt":         "interrupt",
}

// handler is one hook handler of a hooks.json matcher group, as Codex reads it.
type handler struct {
	Type                   string  `json:"type"`
	Command                string  `json:"command"`
	Timeout                *uint64 `json:"timeout"`
	Async                  bool    `json:"async"`
	StatusMessage          *string `json:"statusMessage"`
	AdditionalContextLimit *uint64 `json:"additionalContextLimit"`
}

type group struct {
	Matcher *string   `json:"matcher"`
	Hooks   []handler `json:"hooks"`
}

// CodexHome is the folder where Codex reads hooks.json and config.toml: the canonical path of
// codexHomeEnv ($CODEX_HOME) when it is set, else <home>/.codex, not resolved for links (E8-S3).
func CodexHome(home, codexHomeEnv string) (string, error) {
	if codexHomeEnv == "" {
		return filepath.Join(home, ".codex"), nil
	}
	abs, err := filepath.Abs(codexHomeEnv)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// CodexTrust returns the trust state of the recorded Brain entry e of CLI event event in the
// Codex home dir. It finds the entry in <dir>/hooks.json by its hash, or, for an entry that you
// edited, such as its timeout, by its command (#315). It builds the trust key and the hash of
// that group as Codex 0.154 does, and reads only the hooks.state table of <dir>/config.toml. It
// never writes a file: you trust the hook in Codex.
func CodexTrust(dir string, e install.Entry, event string) (Trust, error) {
	label, ok := eventLabels[event]
	if !ok {
		return "", fmt.Errorf("codex trust: unknown event %q", event)
	}
	source := filepath.Join(dir, "hooks.json")
	data, err := os.ReadFile(source)
	if errors.Is(err, fs.ErrNotExist) {
		return NoEntry, nil
	}
	if err != nil {
		return "", err
	}
	var doc struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return NoEntry, nil //nolint:nilerr // Codex runs no hook from a file of another shape
	}
	groups := doc.Hooks[event]
	gi := slices.IndexFunc(groups, func(g json.RawMessage) bool {
		sum := sha256.Sum256(g)
		return hex.EncodeToString(sum[:]) == e.SHA256
	})
	if gi < 0 {
		// Codex trusts an edited entry by its own hash, so its state is the one to report.
		gi = slices.IndexFunc(groups, func(g json.RawMessage) bool {
			var x group
			return json.Unmarshal(g, &x) == nil && len(x.Hooks) == 1 && x.Hooks[0].Command == e.Command
		})
	}
	if gi < 0 {
		return NoEntry, nil
	}
	var g group
	if err := json.Unmarshal(groups[gi], &g); err != nil || len(g.Hooks) != 1 {
		return "", fmt.Errorf("codex trust: the Brain group in %s is not one handler", source)
	}
	sum, err := hookHash(event, label, g.Matcher, g.Hooks[0])
	if err != nil {
		return "", err
	}
	states, err := readHookStates(filepath.Join(dir, "config.toml"))
	if err != nil {
		return "", err
	}
	st, ok := states[fmt.Sprintf("%s:%s:%d:0", source, label, gi)]
	switch {
	case !ok || st.TrustedHash == "":
		return Untrusted, nil
	case st.TrustedHash != sum:
		return Modified, nil
	case st.Enabled != nil && !*st.Enabled:
		return Disabled, nil
	}
	return Trusted, nil
}

// hookState is one [hooks.state."<key>"] table of the Codex config.toml.
type hookState struct {
	TrustedHash string `toml:"trusted_hash"`
	Enabled     *bool  `toml:"enabled"`
}

// readHookStates returns the hooks.state table of the Codex config.toml at path by key. The
// decode keeps only that table. No file is no state: nothing is trusted.
func readHookStates(path string) (map[string]hookState, error) {
	var cfg struct {
		Hooks struct {
			State map[string]hookState `toml:"state"`
		} `toml:"hooks"`
	}
	_, err := toml.DecodeFile(path, &cfg)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("codex config %s: %w", path, err)
	}
	return cfg.Hooks.State, nil
}

// CodexHookHash returns the trust key label and the trusted hash of a Codex hook group of one
// command handler with the default timeout, as a launch passes it with -c (E24-S2). matcher
// and contextLimit are nil when the group has none. Codex 0.165 hashes a -c hook with the rule
// of a hooks.json hook (E24-S1).
func CodexHookHash(event string, matcher *string, command string, contextLimit *uint64) (label, hash string, err error) {
	label, ok := eventLabels[event]
	if !ok {
		return "", "", fmt.Errorf("codex trust: unknown event %q", event)
	}
	hash, err = hookHash(event, label, matcher, handler{Type: "command", Command: command, AdditionalContextLimit: contextLimit})
	return label, hash, err
}

// The pinned fixture of the Codex hook hash rule: the E24-S1 spike passed CodexFixtureHash with
// -c hooks.state for a -c SessionStart hook of CodexFixtureCommand, and Codex
// CodexFixtureVersion ran the hook with no trust prompt.
const (
	CodexFixtureCommand = "/tmp/e24s1-scratch/tools/hook.sh /tmp/e24s1-scratch/hooks.log CODEX-LAUNCH codex-start"
	CodexFixtureHash    = "sha256:fe07e8a190835d341d6b7eb164fb934c9efa886f63eaa143c759efbfc68752f2"
	CodexFixtureVersion = "0.165.0"
)

// CheckCodexHookHash returns an error when CodexHookHash no longer gives the pinned hash of the
// fixture. Then Codex would not trust the hooks of a launch, and it would skip them. brain
// doctor runs it (E24-S8).
func CheckCodexHookHash() error {
	_, sum, err := CodexHookHash("SessionStart", nil, CodexFixtureCommand, nil)
	if err != nil {
		return err
	}
	if sum != CodexFixtureHash {
		return fmt.Errorf("the hash of the fixture hook is %s, but Codex %s trusted %s", sum, CodexFixtureVersion, CodexFixtureHash)
	}
	return nil
}

// hookHash is hook_hash and version_for_toml of Codex 0.154, as the E8-S3 probe computes it:
// the normalized identity {event_name, matcher, hooks: [handler]} as JSON with sorted keys and
// no spaces, then SHA-256, as "sha256:<hex>".
func hookHash(event, label string, matcher *string, h handler) (string, error) {
	timeout := uint64(600)
	if event == "SessionEnd" || event == "Interrupt" {
		timeout = 1
		if h.Timeout != nil {
			timeout = min(max(*h.Timeout, 1), 3)
		}
	} else if h.Timeout != nil {
		timeout = max(*h.Timeout, 1)
	}
	norm := map[string]any{
		"type":    "command",
		"command": h.Command,
		"timeout": timeout,
		"async":   h.Async,
	}
	if h.StatusMessage != nil {
		norm["statusMessage"] = *h.StatusMessage
	}
	if h.AdditionalContextLimit != nil && *h.AdditionalContextLimit != defaultContextLimit && contextEvent(event) {
		norm["additionalContextLimit"] = *h.AdditionalContextLimit
	}
	identity := map[string]any{
		"event_name": label,
		"hooks":      []any{norm},
	}
	if matcher != nil && matcherEvent(event) {
		identity["matcher"] = *matcher
	}
	// encoding/json sorts map keys; Codex does not escape HTML.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(identity); err != nil {
		return "", err
	}
	sum := sha256.Sum256(lineSeparators(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// lineSeparators returns the JSON text b with the JSON escape of each U+2028 and U+2029 as the
// UTF-8 bytes of its character. encoding/json escapes these two characters, and the serde_json
// of Codex does not. An escaped backslash before "u2028" stays: the scan reads each escape as one pair.
func lineSeparators(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] != '\\' || i+1 == len(b):
			out = append(out, b[i])
		case bytes.HasPrefix(b[i+1:], []byte("u2028")):
			out = utf8.AppendRune(out, 0x2028)
			i += 5
		case bytes.HasPrefix(b[i+1:], []byte("u2029")):
			out = utf8.AppendRune(out, 0x2029)
			i += 5
		default:
			out = append(out, b[i], b[i+1])
			i++
		}
	}
	return out
}

// matcherEvent reports whether Codex keeps the matcher of the event (matcher_pattern_for_event).
func matcherEvent(event string) bool {
	switch event {
	case "UserPromptSubmit", "Stop", "Interrupt":
		return false
	}
	return true
}

// contextEvent reports whether the event can emit additionalContext.
func contextEvent(event string) bool {
	switch event {
	case "PreToolUse", "PostToolUse", "SessionStart", "UserPromptSubmit", "SubagentStart":
		return true
	}
	return false
}
