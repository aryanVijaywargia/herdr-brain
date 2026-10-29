package launch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// mcpServer is the Brain MCP server of the launch in the folder dir (E24-S6): `brain mcp
// --launch <dir>`, run by the brain binary self. The server reads its identity from the launch
// folder on each call, never from the env. Codex passes a server only its default env and the
// names of env_vars: HERDR_BIN_PATH lets the server find the herdr of the pane.
func mcpServer(self, dir string) *Server {
	return &Server{Command: self, Args: []string{"mcp", "--launch", dir}, EnvVars: []string{"HERDR_BIN_PATH"}}
}

// claudeServers refuses a Claude launch whose user --mcp-config names a server brain. Claude
// loads each --mcp-config, and a launch server wins over a user-scope server of the same name
// (E24-S1), but nobody checked which of two --mcp-config servers named brain wins. If the
// user's server won, the Brain allow rule mcp__brain would run its tools with no question.
func claudeServers(a *args, _ Deps) error {
	n := 0
	for _, f := range a.flags {
		if f.name != "--mcp-config" {
			continue
		}
		values := a.raw[f.first+1 : f.end]
		if f.value != "" {
			values = []string{f.value}
		}
		for _, v := range values {
			n++
			// A JSON text can hold secrets, such as the env of a server: name it only by its place.
			name := fmt.Sprintf("--mcp-config value %d (JSON text)", n)
			if !strings.HasPrefix(strings.TrimSpace(v), "{") {
				name = "--mcp-config " + v
			}
			cfg, err := readSettings(v)
			if err != nil {
				return fmt.Errorf("%s: Brain cannot read its server names, so Brain starts nothing: %w", name, err)
			}
			servers, ok := cfg["mcpServers"].(map[string]any)
			if cfg["mcpServers"] != nil && !ok {
				return fmt.Errorf("%s: mcpServers is not an object, so Brain starts nothing", name)
			}
			if _, ok := servers[serverName]; ok {
				return fmt.Errorf("%s: it names a server %s, the name of the Brain MCP server of the launch, "+
					"so Brain starts nothing; rename that server, or run claude without brain", name, serverName)
			}
		}
	}
	return nil
}

// profileRe is a Codex profile name that --profile takes (Codex 0.165
// protocol/src/config_types.rs ProfileV2Name). Codex refuses any other name and starts nothing.
var profileRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// codexServers refuses a Codex launch while a config file that Codex loads for it has a
// mcp_servers.brain table. Codex merges the tables of its config layers key by key, so that
// table and the Brain -c mcp_servers.brain.* flags make one server with the keys of both
// (Codex 0.165 config/src/merge.rs). An old global entry stays there until E24-S8 removes it.
func codexServers(a *args, d Deps) error {
	files, err := codexConfigs(a, d)
	if err != nil {
		return err
	}
	for _, f := range files {
		has, err := hasBrainServer(f)
		if err != nil {
			return err
		}
		if has {
			return fmt.Errorf("%s has a [mcp_servers.%s] table: Codex merges it with the Brain MCP server of the launch, "+
				"so Brain starts nothing; remove that table, or run codex without brain", f, serverName)
		}
	}
	return nil
}

// codexConfigs returns the config files that Codex 0.165 loads for a launch with the user's
// args of a and the env and working folder of d (config/src/loader/mod.rs
// load_config_layers_state): the system config.toml, $CODEX_HOME/config.toml, the profile
// $CODEX_HOME/<name>.config.toml of --profile, the .codex/config.toml of the project folders,
// and the legacy managed_config.toml. Codex reads the project folders from the project root
// down to the working folder (-C, else the folder of the call). This list takes each folder up
// to /, so it needs no project root markers. It also takes an untrusted project: Codex loads
// its file once you trust the folder at the start prompt (tui/src/lib.rs).
//
// Known limits: the list cannot hold every layer that Codex merges.
//   - resume and fork can load the project config of the folder of the saved session, not the
//     folder of the call (tui/src/lib.rs:1008 and :1766, tui/src/session_resume.rs:105). The
//     Brain command, args and env_vars still win there, because the -c layer is above the
//     project layers; only the server keys that Brain does not set can merge.
//   - The cloud config of an organization (config/src/loader/mod.rs:212 and :290,
//     config/src/cloud_config_layers.rs:91) and the macOS managed preferences of
//     com.openai.codex (config/src/loader/layer_io.rs:122, loader/macos.rs:26, merged above the
//     -c layer at loader/mod.rs:464) are no files that Brain can read in a reliable way.
func codexConfigs(a *args, d Deps) ([]string, error) {
	home, err := codexConfigHome(d)
	if err != nil {
		return nil, err
	}
	files := []string{filepath.Join(d.CodexSystem, "config.toml"), filepath.Join(home, "config.toml")}
	if p := a.last("-p", "--profile"); profileRe.MatchString(p) {
		files = append(files, filepath.Join(home, p+".config.toml"))
	}
	cwd := d.Dir
	if c := a.last("-C", "--cd"); c != "" {
		cwd = c
		if !filepath.IsAbs(c) {
			cwd = filepath.Join(d.Dir, c)
		}
	}
	for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
		if dot := filepath.Join(dir, ".codex"); isDir(dot) && !sameFolder(dot, home) {
			files = append(files, filepath.Join(dot, "config.toml"))
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return append(files, filepath.Join(d.CodexSystem, "managed_config.toml")), nil
}

// codexConfigHome returns the Codex home of d as Codex 0.165 finds it
// (utils/home-dir/src/lib.rs find_codex_home): a CODEX_HOME that is set must be a folder, and
// Codex resolves its links before its "..", so Brain does the same before it adds file names;
// a cleaned "link/.." can be another folder. With no CODEX_HOME, it is ~/.codex.
func codexConfigHome(d Deps) (string, error) {
	if home := lookup(d.Environ, "CODEX_HOME"); home != "" {
		if !filepath.IsAbs(home) {
			home = d.Dir + string(filepath.Separator) + home // no Join: it would clean "link/.."
		}
		resolved, err := filepath.EvalSymlinks(home)
		if err != nil {
			return "", fmt.Errorf("CODEX_HOME: %w, so Codex does not start", err)
		}
		if !isDir(resolved) {
			return "", fmt.Errorf("CODEX_HOME %s is not a folder, so Codex does not start", resolved)
		}
		return resolved, nil
	}
	h := lookup(d.Environ, "HOME")
	if !filepath.IsAbs(h) {
		return "", errors.New("HOME is not an absolute path, so Brain cannot read the Codex config")
	}
	resolved, err := filepath.EvalSymlinks(h)
	if err != nil {
		return "", fmt.Errorf("HOME: %w", err)
	}
	return filepath.Join(resolved, ".codex"), nil
}

// isDir reports whether path is a folder. As Codex, a path that Brain cannot stat is none.
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// sameFolder reports whether the folders a and b are one folder. Codex skips the project
// folder .codex that is CODEX_HOME: its config.toml is the user config.
func sameFolder(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}

// hasBrainServer reports whether the Codex config file path has a mcp_servers.brain key. A
// file that does not exist is no config layer, as in Codex.
func hasBrainServer(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the Codex config: %w", err)
	}
	var cfg map[string]any
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return false, fmt.Errorf("read the Codex config %s: %w", path, err)
	}
	servers, _ := cfg["mcp_servers"].(map[string]any) // not a table: Codex refuses the file itself
	_, ok := servers[serverName]
	return ok, nil
}

// last returns the value of the last user flag of the names, or "" for none. Codex takes the
// flag of the subcommand over the top-level one, and it comes later.
func (a *args) last(names ...string) string {
	v := ""
	for _, f := range a.flags {
		if slices.Contains(names, f.name) {
			v = f.value
		}
	}
	return v
}
