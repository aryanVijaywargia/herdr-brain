package extract

import (
	"cmp"
	"slices"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// envVar is one variable of the environment of Brain that an extractor call passes on to the
// CLI (#428). cli is the CLI that reads it, "" for both. secret says that its value is a
// credential, or a URL that can hold one: a failure reason never shows that value (hide).
type envVar struct {
	name   string
	cli    store.ExtractorCLI
	secret bool
}

// callEnv is the allowlist of the variables that a CLI needs to sign in and to reach its
// endpoint (#428), from the documentation of each CLI [Claude Code 2.1.303 env-vars page, Codex
// 0.163 environment-variables page and binary]. Brain passes a variable only when it is set, and
// never shows, logs or stores its value. Any other variable stays out of the call (E13-S1),
// also HERDR_* and every CLAUDE_CODE_* variable that is not named here.
var callEnv = []envVar{
	// Both CLIs: the private temp folder of the user, a proxy in either case of the name, and a
	// custom CA bundle for a TLS proxy.
	{"TMPDIR", "", false},
	{"HTTPS_PROXY", "", true}, {"https_proxy", "", true},
	{"HTTP_PROXY", "", true}, {"http_proxy", "", true},
	{"ALL_PROXY", "", true}, {"all_proxy", "", true},
	{"NO_PROXY", "", false}, {"no_proxy", "", false},
	{"SSL_CERT_FILE", "", false}, {"SSL_CERT_DIR", "", false}, {"NODE_EXTRA_CA_CERTS", "", false},
	// Amazon Bedrock, a provider of both CLIs: its region and AWS sign-in. A profile lives in
	// ~/.aws, under HOME.
	{"AWS_REGION", "", false}, {"AWS_PROFILE", "", false},
	{"AWS_ACCESS_KEY_ID", "", true}, {"AWS_SECRET_ACCESS_KEY", "", true}, {"AWS_SESSION_TOKEN", "", true},
	{"AWS_BEARER_TOKEN_BEDROCK", "", true},
	// claude: an API key, a bearer token or the claude setup-token sign-in, a gateway or proxy
	// with its headers, and the folder of its own sign-in.
	{"ANTHROPIC_API_KEY", store.ExtractorClaude, true}, {"ANTHROPIC_AUTH_TOKEN", store.ExtractorClaude, true},
	{"CLAUDE_CODE_OAUTH_TOKEN", store.ExtractorClaude, true},
	{"ANTHROPIC_BASE_URL", store.ExtractorClaude, true}, {"ANTHROPIC_CUSTOM_HEADERS", store.ExtractorClaude, true},
	{"CLAUDE_CONFIG_DIR", store.ExtractorClaude, false},
	// claude on Amazon Bedrock or Google Vertex AI: the switch and the values that it needs.
	{"CLAUDE_CODE_USE_BEDROCK", store.ExtractorClaude, false}, {"CLAUDE_CODE_USE_VERTEX", store.ExtractorClaude, false},
	{"ANTHROPIC_VERTEX_PROJECT_ID", store.ExtractorClaude, false}, {"CLOUD_ML_REGION", store.ExtractorClaude, false},
	{"GOOGLE_APPLICATION_CREDENTIALS", store.ExtractorClaude, false},
	// codex: an API key or an access token, the base URL that older versions read (a newer one
	// takes openai_base_url from config.toml), the folder of its config and sign-in, and its own
	// CA bundle.
	{"OPENAI_API_KEY", store.ExtractorCodex, true}, {"CODEX_API_KEY", store.ExtractorCodex, true}, {"CODEX_ACCESS_TOKEN", store.ExtractorCodex, true},
	{"OPENAI_BASE_URL", store.ExtractorCodex, true}, {"CODEX_HOME", store.ExtractorCodex, false}, {"CODEX_CA_CERTIFICATE", store.ExtractorCodex, false},
}

// cleanEnv is the environment of a call of extractor CLI cli, with getenv the environment of
// Brain: HOME, PATH, USER and LANG (E13-S1), and each variable of callEnv for cli that is set
// (#428).
func cleanEnv(cli store.ExtractorCLI, getenv func(string) string) []string {
	env := []string{"HOME=" + getenv("HOME"), "PATH=" + getenv("PATH"), "USER=" + getenv("USER"), "LANG=en_US.UTF-8"}
	for _, v := range callEnv {
		if val := getenv(v.name); val != "" && (v.cli == "" || v.cli == cli) {
			env = append(env, v.name+"="+val)
		}
	}
	return env
}

// hide replaces in text each value of a secret variable of env (callEnv) with "[secret]", so the
// output of a CLI cannot carry a sign-in value into a stored reason (#428). It also hides each
// line of a value, and the value of each "Name: Value" line, as a CLI can print one header of
// ANTHROPIC_CUSTOM_HEADERS. The longest part goes first, so no end of a value stays.
func hide(text string, env []string) string {
	var parts []string
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		if !slices.ContainsFunc(callEnv, func(v envVar) bool { return v.name == name && v.secret }) {
			continue
		}
		parts = append(parts, val)
		for _, l := range strings.Split(val, "\n") {
			_, h, _ := strings.Cut(l, ":")
			parts = append(parts, strings.TrimSpace(l), strings.TrimSpace(h))
		}
	}
	slices.SortFunc(parts, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, p := range parts {
		if p != "" {
			text = strings.ReplaceAll(text, p, "[secret]")
		}
	}
	return text
}
