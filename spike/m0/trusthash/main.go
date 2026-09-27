// trusthash is the E8-S3 probe. It computes the Codex 0.154 hook trust key and
// trusted_hash for every command hook in a hooks.json file, and compares them
// with the [hooks.state."<key>"] tables of a Codex config.toml. It only reads.
//
// Usage: trusthash <hooks.json> [config.toml]
//
//	trusthash -inline <event> <command> <timeout> [config.toml]
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// sessionFlagsSource is the key source Codex uses for `-c hooks.<Event>=...`
// hooks [M0 "Codex hook trust"].
const sessionFlagsSource = "/<session-flags>/config.toml"

// defaultContextLimit is DEFAULT_HOOK_OUTPUT_TOKEN_LIMIT in Codex 0.154. Codex
// drops an additionalContextLimit equal to it before it hashes [Codex source].
const defaultContextLimit = 2500

// eventLabels maps a hooks.json event name to its key label (hooks/src/lib.rs).
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

type hooksFile struct {
	Hooks map[string][]group `json:"hooks"`
}

// entry is one command hook with its trust key and its current hash.
type entry struct {
	Key  string
	Hash string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "trusthash:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	var entries []entry
	var configPath string
	switch {
	case len(args) >= 4 && args[0] == "-inline":
		timeout, err := strconv.ParseUint(args[3], 10, 64)
		if err != nil {
			return fmt.Errorf("timeout: %w", err)
		}
		h := handler{Type: "command", Command: args[2], Timeout: &timeout}
		entries, err = entriesFor(sessionFlagsSource, map[string][]group{args[1]: {{Hooks: []handler{h}}}})
		if err != nil {
			return err
		}
		if len(args) > 4 {
			configPath = args[4]
		}
	case len(args) >= 1:
		raw, err := os.ReadFile(args[0])
		if err != nil {
			return err
		}
		var f hooksFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return fmt.Errorf("parse %s: %w", args[0], err)
		}
		abs, err := filepath.Abs(args[0])
		if err != nil {
			return err
		}
		entries, err = entriesFor(abs, f.Hooks)
		if err != nil {
			return err
		}
		if len(args) > 1 {
			configPath = args[1]
		}
	default:
		return errors.New("usage: trusthash <hooks.json> [config.toml] | -inline <event> <command> <timeout> [config.toml]")
	}

	state := map[string]string{}
	if configPath != "" {
		var err error
		if state, err = readTrustedHashes(configPath); err != nil {
			return err
		}
	}
	for _, e := range entries {
		status := "untrusted"
		if stored, ok := state[e.Key]; ok {
			status = "modified"
			if stored == e.Hash {
				status = "trusted"
			}
		}
		fmt.Printf("%s\n  hash   %s\n  status %s\n", e.Key, e.Hash, status)
	}
	return nil
}

// entriesFor returns the key and the hash of every command hook, in the order
// Codex numbers them: group index, then handler index within the group.
func entriesFor(keySource string, events map[string][]group) ([]entry, error) {
	names := make([]string, 0, len(events))
	for name := range events {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []entry
	for _, name := range names {
		label, ok := eventLabels[name]
		if !ok {
			return nil, fmt.Errorf("unknown event %q", name)
		}
		for gi, g := range events[name] {
			for hi, h := range g.Hooks {
				if h.Type != "command" {
					continue
				}
				sum, err := hookHash(name, label, g.Matcher, h)
				if err != nil {
					return nil, err
				}
				out = append(out, entry{Key: fmt.Sprintf("%s:%s:%d:%d", keySource, label, gi, hi), Hash: sum})
			}
		}
	}
	return out, nil
}

// hookHash mirrors hook_hash and version_for_toml in Codex 0.154: the
// normalized identity {event_name, matcher, hooks: [handler]} as JSON with
// sorted keys and no spaces, then SHA-256, as "sha256:<hex>".
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
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(identity); err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// matcherEvent reports whether Codex keeps the matcher for the event
// (matcher_pattern_for_event).
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

// readTrustedHashes reads only the [hooks.state."<key>"] tables of a Codex
// config.toml and returns key -> trusted_hash. It skips every other line.
func readTrustedHashes(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	key := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			key = ""
			if rest, ok := strings.CutPrefix(line, `[hooks.state."`); ok {
				key, _ = strings.CutSuffix(rest, `"]`)
			}
			continue
		}
		if key == "" {
			continue
		}
		if v, ok := strings.CutPrefix(line, "trusted_hash"); ok {
			v = strings.TrimSpace(v)
			v = strings.TrimSpace(strings.TrimPrefix(v, "="))
			out[key] = strings.Trim(v, `"`)
		}
	}
	return out, sc.Err()
}
