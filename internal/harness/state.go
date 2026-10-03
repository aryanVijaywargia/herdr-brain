// Package harness computes the adapter state of each harness (HLD §8.5, E8-S4). Check computes
// it on each call from herdr, the install record, the CLI and the conformance results; no
// store holds it. `brain status` and `brain doctor` call Check (E9). The hook never does,
// because Check runs `<cli> --version`.
package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
)

// State is the one word for a harness in `brain status`.
type State string

// The states, from the lowest to the highest (§8.5). Retest overrides the others.
const (
	None       State = "none"       // herdr sees no pane of the kind and nothing is installed
	Detected   State = "detected"   // herdr reports the kind in some pane
	Configured State = "configured" // the recorded Brain entry is in the hook file
	Verified   State = "verified"   // a conformance pass matches the current values
	Retest     State = "retest"     // an untested or unreadable CLI version, or hook input of an unknown shape
)

// Status is the state of one harness, with each part apart, so `brain status` can show a
// configured hook with no test as unverified (§8.5, S28).
type Status struct {
	Kind       string
	Detected   bool
	Configured bool
	Verified   bool
	Retest     bool
	CLIVersion string // empty when `<cli> --version` failed; VersionErr says why
	VersionErr error
}

// State returns the one word for s.
func (s Status) State() State {
	switch {
	case s.Retest:
		return Retest
	case s.Verified:
		return Verified
	case s.Configured:
		return Configured
	case s.Detected:
		return Detected
	}
	return None
}

// Panes lists the panes of a herdr session. *herdr.Client is one.
type Panes interface {
	Panes(ctx context.Context) ([]herdr.Pane, error)
}

// Files are the data home files that Check reads.
type Files struct {
	Install     string // <data home>/install.json (paths.InstallFile)
	Conformance string // <data home>/conformance.json (paths.ConformanceFile)
	Retest      string // the folder of the retest markers that the hook writes (MarkRetest)
}

// versionRe matches a version word in `<cli> --version`: "2.1.283 (Claude Code)" and
// "codex-cli 0.154.0" (spike/m0/FINDINGS.md).
var versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Check returns the status of each profile, in order. It finds each CLI by its profile kind on
// the PATH. A retest marker sets retest until a conformance pass that completed after it.
func Check(ctx context.Context, h Panes, f Files, profiles []*adapter.Profile) ([]Status, error) {
	panes, err := h.Panes(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := install.Read(f.Install)
	if err != nil {
		return nil, err
	}
	results, err := readConformance(f.Conformance)
	if err != nil {
		return nil, err
	}
	out := make([]Status, len(profiles))
	for i, p := range profiles {
		s := Status{Kind: p.Kind, Detected: detected(panes, p.Kind)}
		e, ok := rec.Entries[p.Kind]
		if ok {
			if s.Configured, err = configured(e, p.HookKey(), p.Events.Start); err != nil {
				return nil, err
			}
		}
		s.CLIVersion, s.VersionErr = cliVersion(ctx, p.Kind)
		m, marked, err := readMarker(f.Retest, p.Kind)
		if err != nil {
			return nil, err
		}
		r, ok := last(results, p.Kind)
		current := s.Configured && ok && s.VersionErr == nil && r.Result == Pass &&
			r.CLIVersion == s.CLIVersion && r.ProfileSHA256 == p.SHA256 && r.EntrySHA256 == e.SHA256
		// Hook input of an unknown shape after the pass invalidates it (S14). Only a pass on
		// the current values that completed after the marker clears the marker.
		if current && r.At.After(m.At) {
			marked = false
		}
		s.Verified = current && !marked
		// A pass tests the version it ran on (§8.5 "tested version"): a verified CLI after an
		// update needs no retest, and a CLI that returns to the profile version after a pass on
		// another version needs one.
		tested := p.TestedVersion
		if ok && r.Result == Pass {
			tested = r.CLIVersion
		}
		s.Retest = (s.CLIVersion != tested || marked) && !s.Verified
		out[i] = s
	}
	return out, nil
}

func detected(panes []herdr.Pane, kind string) bool {
	for _, p := range panes {
		if p.Agent != nil && *p.Agent == kind {
			return true
		}
	}
	return false
}

// configured reports whether the start event array in the hooks object at root key key of the
// hook file holds an entry with the recorded hash. The entry must be there, not only somewhere
// in the file: only the start event delivers the brief.
func configured(e install.Entry, key, event string) (bool, error) {
	data, err := os.ReadFile(e.Target)
	// A deleted hook file holds no entry.
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// RawMessage keeps the exact bytes of each entry, as install hashed them.
	// The other root members, such as the Claude settings, may be of any shape.
	var root map[string]json.RawMessage
	var hooks map[string][]json.RawMessage
	if json.Unmarshal(data, &root) != nil || json.Unmarshal(root[key], &hooks) != nil {
		return false, nil //nolint:nilerr // a file of another shape holds no start hook that the CLI runs
	}
	for _, g := range hooks[event] {
		sum := sha256.Sum256(g)
		if hex.EncodeToString(sum[:]) == e.SHA256 {
			return true, nil
		}
	}
	return false, nil
}

// cliVersion runs `<kind> --version` and returns the first version word of its output.
func cliVersion(ctx context.Context, kind string) (string, error) {
	out, err := exec.CommandContext(ctx, kind, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", kind, err)
	}
	for _, w := range strings.Fields(string(out)) {
		if versionRe.MatchString(w) {
			return w, nil
		}
	}
	return "", fmt.Errorf("%s --version: no version in %q", kind, out)
}
