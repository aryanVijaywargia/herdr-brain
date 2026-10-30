package launch

import (
	"path/filepath"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
)

// captureHooks returns Brain's start hook (E24-S4) and its prompt, tool and stop hooks
// (E24-S5) of the CLI kind for the launch folder dir: `<brain> hook --profile <kind> --event
// <event> --launch <dir>`, on the CLI events and the tool matcher of the adapter profile. The
// folder path is safe in an argument: the launch ID stays in its 0600 file. brain is the brain
// binary.
func captureHooks(kind, brain, dir string) ([]Hook, error) {
	p, err := adapter.Load(kind)
	if err != nil {
		return nil, err
	}
	var hs []Hook
	for _, h := range p.Hooks() {
		hs = append(hs, Hook{Event: h.CLIEvent, Matcher: h.Matcher,
			Command: shellQuote(brain) + " hook --profile " + kind + " --event " + h.Event + " --launch " + shellQuote(dir)})
	}
	return hs, nil
}

// codexHome returns the absolute Codex home of a launch of the CLI kind with the env environ:
// CODEX_HOME, else ~/.codex. brained finds the rollouts of the launch there. It is "" for
// another CLI, and for an env with no home: brained then reads no rollout of the launch, and
// hooks and pane reads still capture it.
func codexHome(kind string, environ []string) (string, error) {
	if kind != "codex" {
		return "", nil
	}
	home := lookup(environ, "CODEX_HOME")
	if home == "" {
		if h := lookup(environ, "HOME"); h != "" {
			home = filepath.Join(h, ".codex")
		}
	}
	if home == "" {
		return "", nil
	}
	return filepath.Abs(home)
}

// shellQuote is s as one shell word: as it is when it has only safe characters, else in single
// quotes. A CLI runs a hook command with the shell.
func shellQuote(s string) string {
	safe := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+", r)
	}
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !safe(r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
