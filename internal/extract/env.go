package extract

import (
	"slices"
	"strings"
)

// The denylist of the environment of an extractor call (#428). The call inherits the env of
// brained, which is the herdr server env, so the CLI signs in as it does in a herdr pane: with
// its own login, an API key or token, a proxy or gateway, or a cloud provider. Only these
// variables go, as they tell the CLI that it runs inside herdr, Brain or another agent session.
var (
	// denyPrefixes: HERDR_* says that a process runs in a herdr pane, and BRAIN_* changes what a
	// brain command does.
	denyPrefixes = []string{"HERDR_", "BRAIN_"}
	// denyNames are the markers that Claude Code and codex set in the processes of a session:
	// a nested claude -p or codex exec would take the parent session, its socket or its sandbox
	// as its own [Claude Code 2.1.303 env-vars page and its child env; codex 0.163 binary].
	denyNames = []string{
		"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SSE_PORT", "CLAUDE_CODE_SESSION_ID",
		"CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_MESSAGING_SOCKET",
		"CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_REMOTE", "CLAUDE_CODE_REMOTE_SESSION_ID",
		"CLAUDE_AGENT_SDK_VERSION", "CLAUDE_PID", "CLAUDE_EFFORT", "CLAUDE_JOB_DIR", "CLAUDE_PROJECT_DIR",
		"CLAUDE_ENV_FILE", "CLAUDE_PLUGIN_ROOT", "CLAUDE_PLUGIN_DATA",
		"CODEX_SANDBOX", "CODEX_SANDBOX_NETWORK_DISABLED", "CODEX_THREAD_ID", "CODEX_TOOL_CALL_ID",
	}
)

// cleanEnv is the environment of an extractor call: environ, the environment of brained, with
// no variable of the denylist (#428). LANG is en_US.UTF-8 when environ has none (E13-S1). Brain
// passes the values on and never reads, logs or stores them.
func cleanEnv(environ []string) []string {
	env := make([]string, 0, len(environ)+1)
	lang := false
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(denyNames, name) || slices.ContainsFunc(denyPrefixes, func(p string) bool {
			return strings.HasPrefix(name, p)
		}) {
			continue
		}
		lang = lang || name == "LANG"
		env = append(env, kv)
	}
	if !lang {
		env = append(env, "LANG=en_US.UTF-8")
	}
	return env
}
