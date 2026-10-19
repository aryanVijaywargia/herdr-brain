package extract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

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

// cleanEnv returns the environment of a call of extractor CLI cli, with getenv the environment
// of Brain: HOME, PATH, USER and LANG (E13-S1), each variable of callEnv for cli that is set
// (#428), and for codex the API key variable of its chosen provider (codexKey). secrets are the
// texts that a failure reason must not show: each form of each secret value (secretForms). The
// codex key is a secret also when callEnv lists its name as plain, as AWS_PROFILE.
func cleanEnv(cli store.ExtractorCLI, getenv func(string) string) (env, secrets []string) {
	env = []string{"HOME=" + getenv("HOME"), "PATH=" + getenv("PATH"), "USER=" + getenv("USER"), "LANG=en_US.UTF-8"}
	add := func(name string, secret bool) {
		val := getenv(name)
		if val == "" {
			return
		}
		if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, name+"=") }) {
			env = append(env, name+"="+val)
		}
		if secret {
			secrets = append(secrets, secretForms(val)...)
		}
	}
	for _, v := range callEnv {
		if v.cli == "" || v.cli == cli {
			add(v.name, v.secret)
		}
	}
	if cli == store.ExtractorCodex {
		if name := codexKey(getenv); name != "" {
			add(name, true)
		}
	}
	slices.Sort(secrets)
	return env, slices.Compact(secrets)
}

// secretForms returns the texts of secret value val that a CLI can print: the whole value, each
// line, the value of each "Name: Value" header line and each word after its first (the token
// of "Authorization: Bearer <token>"), and the user info, user name, password and query values
// of each URL. Each comes raw, URL-encoded and base64-encoded (encodings). A short value stays:
// to hide too much is safe, and a local proxy token can be 6 digits.
func secretForms(val string) []string {
	parts := []string{val}
	for _, l := range strings.Split(val, "\n") {
		l = strings.TrimSpace(l)
		parts = append(parts, l)
		if _, h, ok := strings.Cut(l, ":"); ok {
			h = strings.TrimSpace(h)
			parts = append(parts, h)
			if f := strings.Fields(h); len(f) > 1 {
				parts = append(parts, f[1:]...)
			}
		}
		parts = append(parts, urlParts(l)...)
	}
	var forms []string
	for _, p := range parts {
		if p != "" {
			forms = append(forms, encodings(p)...)
		}
	}
	slices.Sort(forms)
	return slices.Compact(forms)
}

// urlParts returns the user info, user name, password and query values of l as a URL, also with
// no scheme, as a proxy variable can be ("user:pass@proxy:3128"); none when l is no URL.
func urlParts(l string) []string {
	if !strings.Contains(l, "://") {
		l = "http://" + l
	}
	u, err := url.Parse(l)
	if err != nil {
		return nil
	}
	var parts []string
	if u.User != nil {
		pw, _ := u.User.Password()
		parts = append(parts, u.User.String(), u.User.Username()+":"+pw, u.User.Username(), pw)
	}
	for _, vs := range u.Query() {
		parts = append(parts, vs...)
	}
	return parts
}

// encodings returns s raw, URL-encoded as a query and as a path, and in the four base64 forms.
func encodings(s string) []string {
	b := []byte(s)
	return []string{s, url.QueryEscape(s), url.PathEscape(s), base64.StdEncoding.EncodeToString(b),
		base64.URLEncoding.EncodeToString(b), base64.RawStdEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b)}
}

// userInfo matches the user info of a URL in a text, as in http://user:pass@host.
var userInfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s@]*@`)

// hide replaces in text each match of a secret of the call (cleanEnv) with "[secret]", and the
// user info of each URL, so the output of a CLI cannot carry a sign-in value into a stored reason
// (#428).
func hide(text string, secrets []string) string {
	return userInfo.ReplaceAllString(hideExact(text, secrets, 1), "${1}[secret]@")
}

// hideExact replaces in text each match of a secret of secrets that has at least least bytes
// with "[secret]"; a run of matches that touch or overlap becomes one. It marks the matched bytes
// of the original text in one mask first, so a short secret does not match inside a "[secret]"
// that it wrote, and its memory stays in proportion to the text.
func hideExact(text string, secrets []string, least int) string {
	var mask []bool
	for _, s := range secrets {
		if len(s) < least {
			continue
		}
		covered := 0 // the end of the last match of s: a byte before it is marked
		for i := 0; i < len(text); {
			j := strings.Index(text[i:], s)
			if j < 0 {
				break
			}
			if mask == nil {
				mask = make([]bool, len(text))
			}
			start, end := i+j, i+j+len(s)
			for k := max(start, covered); k < end; k++ {
				mask[k] = true
			}
			covered, i = end, start+1
		}
	}
	if mask == nil {
		return text
	}
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		switch {
		case !mask[i]:
			b.WriteByte(text[i])
		case i == 0 || !mask[i-1]:
			b.WriteString("[secret]")
		}
	}
	return b.String()
}

// tokenEdge are the characters that hideTokens trims from the ends of a word. Square brackets
// stay, so a word that holds a "[secret]" of hide goes whole.
const tokenEdge = "\"'`,;:(){}<>.!?"

// minToken is the shortest token-shaped word in runes [NEW, #428 Sol round 2].
const minToken = 8

// hideTokens replaces each token-shaped word of the cause of a failure with "[hidden]" and puts
// the words on one line (#428): a word that is minToken runes or longer, after its end
// punctuation, with a digit or one of % + = / \ _ - . : @. Keys, URLs, hex, base64 and encoded
// forms of a secret go; plain words, such as "Failed to authenticate: OAuth session expired",
// stay. It runs on a stored reason only, never on model text.
func hideTokens(text string) string {
	words := strings.Fields(text)
	for i, w := range words {
		core := strings.Trim(w, tokenEdge)
		if utf8.RuneCountInString(core) >= minToken && strings.ContainsAny(core, `0123456789%+=/\_-.:@`) {
			words[i] = strings.Replace(w, core, "[hidden]", 1)
		}
	}
	return strings.Join(words, " ")
}

// minEcho is the shortest secret text, in bytes, that scrub hides in model text [NEW, #428 Sol
// round 2]: a shorter one, such as the base64 form "MQ" of "1", is a common part of words and
// numbers. 6 covers a 6-digit local proxy token.
const minEcho = 6

// scrub hides each secret of the call (hideExact) in every string of res, the JSON result of a
// call, before Brain checks and stores it (#428): a reply can echo a sign-in value into a claim
// and a proposal. It hides only the exact secrets, never token-shaped words, so commands and
// paths stay whole. A result that is not JSON stays as it is: check rejects it as invalid.
func scrub(res json.RawMessage, secrets []string) json.RawMessage {
	if len(res) == 0 || len(secrets) == 0 {
		return res
	}
	d := json.NewDecoder(bytes.NewReader(res))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return res
	}
	out, err := json.Marshal(scrubValue(v, secrets))
	if err != nil {
		return res // a decoded value always encodes again
	}
	return out
}

// scrubValue hides each secret of secrets in every string of v, a decoded JSON value.
func scrubValue(v any, secrets []string) any {
	switch x := v.(type) {
	case string:
		return hideExact(x, secrets, minEcho)
	case []any:
		for i := range x {
			x[i] = scrubValue(x[i], secrets)
		}
	case map[string]any:
		for k, e := range x {
			x[k] = scrubValue(e, secrets)
		}
	}
	return v
}
