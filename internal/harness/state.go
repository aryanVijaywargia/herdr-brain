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
	"strconv"
	"strings"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
)

// State is the one word for a harness in `brain status`.
type State string

// The states, from the lowest to the highest (§8.5). Retest overrides the others. A CLI
// version that no test passed does not change the state (D13): Status.VersionNote warns.
const (
	None       State = "none"       // herdr sees no pane of the kind and nothing is installed
	Detected   State = "detected"   // herdr reports the kind in some pane
	Configured State = "configured" // the recorded Brain entry is in the hook file
	Verified   State = "verified"   // a conformance pass matches the current values
	Retest     State = "retest"     // hook input of an unknown shape that the profile cannot parse (S14)
)

// Status is the state of one harness, with each part apart, so `brain status` can show a
// configured hook with no test as unverified (§8.5, S28).
type Status struct {
	Kind       string
	Installed  bool // the CLI is on the PATH
	Detected   bool
	Configured bool
	Verified   bool
	Retest     bool      // the profile could not parse a hook input of the CLI (S14)
	RetestAt   time.Time // the time of the last such hook input
	CLIVersion string    // empty when `<cli> --version` failed; VersionErr says why
	VersionErr error
	Tested     string // the tested version: of the last pass, else of the profile (§8.5)
}

// VersionNote is the version warning of s (D13): "newer than tested" for a CLI version above
// the tested version, or "" when there is none. It changes no state.
func (s Status) VersionNote() string {
	switch {
	case !s.Installed:
		return ""
	case s.VersionErr != nil:
		return "version unknown"
	case s.CLIVersion == s.Tested:
		return ""
	case newer(s.CLIVersion, s.Tested):
		return "newer than tested (" + s.Tested + ")"
	}
	return "not the tested version (" + s.Tested + ")"
}

// newer reports whether version a is above version b. Both match versionRe; a tested version
// of another form is never below.
func newer(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	if len(bs) != len(as) {
		return false
	}
	for i := range as {
		x, _ := strconv.Atoi(as[i]) // versionRe: digits only
		y, err := strconv.Atoi(bs[i])
		if err != nil || x != y {
			return err == nil && x > y
		}
	}
	return false
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
// the PATH. A retest marker of the current profile sets retest until a conformance pass that
// completed after it. A CLI that is not on the PATH has no version and no retest (#250).
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
		s.Installed = !errors.Is(s.VersionErr, exec.ErrNotFound)
		m, marked, err := readMarker(f.Retest, p.Kind)
		if err != nil {
			return nil, err
		}
		// A marker of another profile is stale: a Brain update may parse that input now.
		marked = marked && m.Profile == p.SHA256
		r, ok := last(results, p.Kind)
		// A newer CLI version keeps the state (D13), so the version is not a current value.
		current := s.Configured && ok && r.Result == Pass && r.ProfileSHA256 == p.SHA256 && r.EntrySHA256 == e.SHA256
		// Hook input of an unknown shape after the pass invalidates it (S14). Only a pass on
		// the current values that completed after the marker clears the marker.
		if current && r.At.After(m.At) {
			marked = false
		}
		s.Verified = current && !marked
		s.Retest = s.Installed && marked
		s.RetestAt = m.At
		// A pass tests the version it ran on (§8.5 "tested version").
		s.Tested = p.TestedVersion
		if ok && r.Result == Pass {
			s.Tested = r.CLIVersion
		}
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
	// The other root members, and the other members of the hooks object such as the agy
	// "enabled" flag, may be of any shape.
	var root, hooks map[string]json.RawMessage
	var entries []json.RawMessage
	if json.Unmarshal(data, &root) != nil || json.Unmarshal(root[key], &hooks) != nil ||
		json.Unmarshal(hooks[event], &entries) != nil {
		return false, nil //nolint:nilerr // a file of another shape holds no start hook that the CLI runs
	}
	for _, g := range entries {
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
