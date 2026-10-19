package extract

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// AC #428 (Sol round 1): codexKey reads the name of the API key variable of the chosen codex
// provider from config.toml, also through the default profile, and nothing else. A reserved or
// bad name, a built-in provider, an array of tables and a missing or broken file give none.
func TestCodexKey(t *testing.T) {
	proxy := "[model_providers.cliproxy]\nname = \"CLIProxyAPI\"\nbase_url = \"http://127.0.0.1:8317/v1\"\n"
	for _, c := range []struct{ name, config, want string }{
		{"provider", "model_provider = \"cliproxy\"\n" + proxy + "env_key = \"CLIPROXY_KEY\" # the key\n", "CLIPROXY_KEY"},
		{"quoted table", "model_provider = 'cli-proxy'\n[model_providers.\"cli-proxy\"]\nenv_key = 'CP_KEY'\n", "CP_KEY"},
		{"dotted key", "model_provider = \"p3\"\nmodel_providers.p3.env_key = \"P3_KEY\"\n", "P3_KEY"},
		{"profile", "model_provider = \"openai\"\nprofile = \"work\"\n[profiles.work]\nmodel_provider = \"p2\"\n" +
			"[model_providers.p2]\nenv_key = \"P2_KEY\"\n", "P2_KEY"},
		{"profile with no provider", "model_provider = \"cliproxy\"\nprofile = \"x\"\n" + proxy + "env_key = \"CLIPROXY_KEY\"\n",
			"CLIPROXY_KEY"},
		{"built-in provider", "model_provider = \"openai\"\n", ""},
		{"no provider", proxy + "env_key = \"CLIPROXY_KEY\"\n", ""},
		{"reserved name", "model_provider = \"cliproxy\"\n" + proxy + "env_key = \"HOME\"\n", ""},
		{"herdr name", "model_provider = \"cliproxy\"\n" + proxy + "env_key = \"HERDR_TOKEN\"\n", ""},
		{"claude name", "model_provider = \"cliproxy\"\n" + proxy + "env_key = \"CLAUDE_CODE_OAUTH_TOKEN\"\n", ""},
		{"loader name", "model_provider = \"cliproxy\"\n" + proxy + "env_key = \"DYLD_INSERT_LIBRARIES\"\n", ""},
		{"bad name", "model_provider = \"cliproxy\"\n" + proxy + "env_key = \"my key\"\n", ""},
		{"array of tables", "model_provider = \"cliproxy\"\n[[model_providers.cliproxy]]\nenv_key = \"CLIPROXY_KEY\"\n", ""},
		{"broken file", "model_provider = \"cliproxy\n[model_providers.cliproxy\nenv_key = CLIPROXY_KEY\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(c.config), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := codexKey(func(name string) string { return map[string]string{"CODEX_HOME": dir}[name] }); got != c.want {
				t.Errorf("codexKey() = %q, want %q", got, c.want)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		home := t.TempDir()
		if got := codexKey(func(name string) string { return map[string]string{"HOME": home}[name] }); got != "" {
			t.Errorf("codexKey() = %q, want none", got)
		}
	})
}

// AC #428 (Sol round 1): the API key variable of the codex provider goes to codex only, when it
// is set, and its value is a secret of the call.
func TestCleanEnvCodexKey(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "model_provider = \"cliproxy\"\n[model_providers.cliproxy]\nenv_key = \"CLIPROXY_KEY\"\n"
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"HOME": home, "CLIPROXY_KEY": "cp-key-31337"}
	getenv := func(name string) string { return vars[name] }
	hasKey := func(env []string) bool {
		return slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "CLIPROXY_KEY=") })
	}
	env, secrets := cleanEnv(store.ExtractorCodex, getenv)
	if !hasKey(env) || !slices.Contains(secrets, "cp-key-31337") {
		t.Errorf("the codex env or its secrets have no CLIPROXY_KEY")
	}
	if env, _ := cleanEnv(store.ExtractorClaude, getenv); hasKey(env) {
		t.Errorf("the claude env has CLIPROXY_KEY")
	}
	delete(vars, "CLIPROXY_KEY")
	if env, _ := cleanEnv(store.ExtractorCodex, getenv); hasKey(env) {
		t.Errorf("the codex env has CLIPROXY_KEY, which is not set")
	}
}
