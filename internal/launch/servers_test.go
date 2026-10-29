package launch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// AC #446 (Codex layers): a mcp_servers.brain table in a config file that Codex loads for the
// launch refuses it: the user config.toml of CODEX_HOME (~/.codex by default), the profile of
// --profile, the .codex/config.toml of the working folder (or of -C) and of each folder above,
// trusted or not, the system config.toml and the legacy managed_config.toml. Brain starts
// nothing, makes no launch record and names the file. Another server, a profile that the args
// do not select, and a file that does not exist refuse nothing.
func TestCodexServerLayers(t *testing.T) {
	brain := "[mcp_servers.brain]\ncommand = \"old-brain\"\n"
	tests := []struct {
		name  string
		file  string // the config file, under the root folder of the test
		text  string
		env   []string // more env; "@" in a value is the root folder
		args  []string
		cwd   string // the working folder under the root; "" is "proj/sub"
		want  bool   // a refusal that names file
		error string // a part of another error
	}{
		{name: "user config", file: "home/.codex/config.toml", text: brain, want: true},
		{name: "dotted keys", file: "home/.codex/config.toml", text: "mcp_servers.brain.command = \"x\"\n", want: true},
		{name: "quoted key", file: "home/.codex/config.toml", text: "[mcp_servers.\"brain\".env]\nA = \"1\"\n", want: true},
		{name: "CODEX_HOME", file: "codex/config.toml", text: brain, env: []string{"CODEX_HOME=@/codex"}, want: true},
		{name: "default home with CODEX_HOME set", file: "home/.codex/config.toml", text: brain, env: []string{"CODEX_HOME=@/codex"}},
		{name: "another server", file: "home/.codex/config.toml", text: "[mcp_servers.other]\ncommand = \"x\"\n"},
		{name: "profile", file: "home/.codex/work.config.toml", text: brain, args: []string{"-p", "work"}, want: true},
		{name: "profile of resume", file: "home/.codex/work.config.toml", text: brain, args: []string{"resume", "--profile=work"}, want: true},
		{name: "profile not selected", file: "home/.codex/work.config.toml", text: brain, args: []string{"-p", "home"}},
		{name: "project", file: "proj/sub/.codex/config.toml", text: brain, want: true},
		{name: "folder above", file: "proj/.codex/config.toml", text: brain, want: true},
		{name: "-C folder", file: "other/.codex/config.toml", text: brain, args: []string{"-C", "../../other"}, want: true},
		{name: "not the -C folder", file: "proj/sub/.codex/config.toml", text: brain, args: []string{"--cd=@/other"}},
		{name: "system", file: "etc/config.toml", text: brain, want: true},
		{name: "managed", file: "etc/managed_config.toml", text: brain, want: true},
		{name: "not TOML", file: "home/.codex/config.toml", text: "[mcp_servers", error: "read the Codex config"},
		{name: "no file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCmdEnv(t, "2.1.312")
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range []string{"home/.codex", "codex", "proj/sub", "other", "etc"} {
				if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tt.file != "" {
				write(t, filepath.Join(root, tt.file), tt.text)
			}
			e.home, e.dir, e.system = filepath.Join(root, "home"), filepath.Join(root, "proj", "sub"), filepath.Join(root, "etc")
			environ := e.herdrEnv()
			for _, kv := range tt.env {
				environ = append(environ, strings.ReplaceAll(kv, "@", root))
			}
			args := make([]string, len(tt.args))
			for i, a := range tt.args {
				args[i] = strings.ReplaceAll(a, "@", root)
			}
			err = e.run(t, environ, "codex", args...)
			_, statErr := os.Stat(paths.Launch(e.store, 1))
			switch {
			case tt.want:
				if err == nil || !strings.Contains(err.Error(), filepath.Join(root, tt.file)+" has a [mcp_servers.brain] table") ||
					!strings.Contains(err.Error(), "Brain starts nothing") || e.exec != nil || !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("Main() = %v, exec %q, launch folder %v; want a refusal that names %s", err, e.exec, statErr, tt.file)
				}
			case tt.error != "":
				if err == nil || !strings.Contains(err.Error(), tt.error) || e.exec != nil {
					t.Errorf("Main() = %v, exec %q, want an error with %q", err, e.exec, tt.error)
				}
			case err != nil || e.exec == nil || statErr != nil:
				t.Errorf("Main() = %v, exec %q, launch folder %v; want a launch", err, e.exec, statErr)
			}
		})
	}
}

// AC #446 (Claude): a user --mcp-config that names a server brain refuses the launch, as a
// JSON text or a file, in the --flag=value form or as one value of the variadic flag. Other
// servers launch. A config that Claude could not read refuses too: Brain cannot see its names.
func TestClaudeServerConfigs(t *testing.T) {
	dir := t.TempDir()
	brainFile, otherFile := filepath.Join(dir, "brain.json"), filepath.Join(dir, "other.json")
	write(t, brainFile, `{"mcpServers":{"brain":{"command":"old-brain"}}}`)
	write(t, otherFile, `{"mcpServers":{"other":{"command":"x"}}}`)
	tests := []struct {
		args  []string
		error string // a part of the error; "" for a launch
	}{
		{args: []string{"--mcp-config=" + otherFile, "hi"}},
		{args: []string{"--mcp-config", otherFile, `{"mcpServers":{}}`, "--x"}},
		{args: []string{"--mcp-config=" + brainFile, "hi"}, error: "it names a server brain"},
		{args: []string{"--mcp-config", otherFile, brainFile}, error: "it names a server brain"},
		{args: []string{"--mcp-config", `{"mcpServers":{"brain":{}}}`}, error: "it names a server brain"},
		{args: []string{"--mcp-config=" + filepath.Join(dir, "none.json")}, error: "no such file"},
		{args: []string{"--mcp-config", `{"mcpServers":[]}`}, error: "mcpServers is not an object"},
	}
	for _, tt := range tests {
		e := newCmdEnv(t, "2.1.312")
		err := e.run(t, e.herdrEnv(), "claude", tt.args...)
		if tt.error == "" {
			if err != nil || e.exec == nil {
				t.Errorf("Main(%q) = %v, want a launch", tt.args, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.error) || !strings.Contains(err.Error(), "Brain starts nothing") || e.exec != nil {
			t.Errorf("Main(%q) = %v, exec %q, want a refusal with %q", tt.args, err, e.exec, tt.error)
		}
	}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
