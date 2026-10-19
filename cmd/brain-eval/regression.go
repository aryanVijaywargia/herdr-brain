package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

// regressionStage is the stage of the regression cases (E20-S1): each bug that
// real use found gets its own case, named with its issue number, so one fix cannot hide
// another failure. Its cases come from the [[bug]] entries of a probe file. A case runs the
// Go tests that hold the fixture of its bug, with no herdr, no CLI and no model, so it has no
// control.
const regressionStage = "regression"

// bugEntry is one [[bug]] of a probe file: the regression case of one bug.
type bugEntry struct {
	Issue int `toml:"issue"`
	// Tests are the Go tests of the case, each "<package folder>:<test name>", for example
	// "internal/herdr:TestLetterIDs" or "internal/adapter:TestScreen/claude-tip".
	Tests []string `toml:"tests"`
}

// GoTest is one Go test of a regression case.
type GoTest struct {
	Package string // the package folder, relative to the repo root: internal/herdr
	Name    string // the test name as `go test -v` prints it, with its subtests: TestScreen/claude-tip
}

// goTestEntry is the form of one test of a [[bug]]. [NEW] form.
var goTestEntry = regexp.MustCompile(`^([a-z0-9_-]+(?:/[a-z0-9_-]+)*):(Test\w*(?:/[^/\s]+)*)$`)

// bugCases checks the [[bug]] entries of a probe file and returns their regression cases. The
// case of issue n has the id "bug-n".
func bugCases(entries []bugEntry) ([]Case, error) {
	var cases []Case
	for i, e := range entries {
		tests, err := e.check()
		if err != nil {
			return nil, fmt.Errorf("bug %d (issue %d): %w", i+1, e.Issue, err)
		}
		cases = append(cases, Case{ID: fmt.Sprintf("bug-%d", e.Issue), Stage: regressionStage,
			Source: fmt.Sprintf("issue #%d", e.Issue), Tests: tests})
	}
	return cases, nil
}

// check checks one [[bug]] and returns its tests.
func (e bugEntry) check() ([]GoTest, error) {
	var errs []error
	if e.Issue < 1 {
		errs = append(errs, fmt.Errorf("issue %d: want the number of the bug's GitHub issue", e.Issue))
	}
	if len(e.Tests) == 0 {
		errs = append(errs, errors.New("tests is empty"))
	}
	var tests []GoTest
	for _, s := range e.Tests {
		m := goTestEntry.FindStringSubmatch(s)
		if m == nil {
			errs = append(errs, fmt.Errorf("test %q: want <package folder>:<test name>, as internal/herdr:TestLetterIDs", s))
			continue
		}
		t := GoTest{Package: m[1], Name: m[2]}
		if slices.Contains(tests, t) {
			errs = append(errs, fmt.Errorf("test %q is listed twice", s))
			continue
		}
		tests = append(tests, t)
	}
	return tests, errors.Join(errs...)
}

// runRegression runs the tests of the regression cases with `go test` in the repo folder
// root, once for each package, and prints one pass or fail line for each case, then one
// summary line, such as "regression: 24 of 24 pass" (#318). A case passes when each of its
// tests passed; a skipped test or one that did not run is no pass. A go test that cannot run
// fails the cases of its package with its error. It reports whether a case failed.
func runRegression(ctx context.Context, root string, environ []string, cases []Case, w io.Writer) (failed bool) {
	tops := map[string][]string{} // each package folder to the top-level tests that its cases name
	for _, c := range cases {
		for _, t := range c.Tests {
			top, _, _ := strings.Cut(t.Name, "/")
			if !slices.Contains(tops[t.Package], top) {
				tops[t.Package] = append(tops[t.Package], top)
			}
		}
	}
	got := map[GoTest]string{} // each test that ran to its action: pass, fail or skip
	why := map[string]string{} // each package folder to the first error line of its run
	for _, pkg := range slices.Sorted(maps.Keys(tops)) {
		actions, line := goTest(ctx, root, environ, pkg, tops[pkg])
		for name, a := range actions {
			got[GoTest{Package: pkg, Name: name}] = a
		}
		why[pkg] = line
	}
	passed := 0
	for _, c := range cases {
		line, ok := regressionLine(c, got, why)
		fmt.Fprintln(w, line)
		if ok {
			passed++
		}
	}
	fmt.Fprintf(w, "%s: %d of %d pass\n", regressionStage, passed, len(cases))
	return passed < len(cases)
}

// testEvent is one event of `go test -json` (go doc test2json). Go 1.24 and later give a
// build error as build-output events.
type testEvent struct {
	Action string
	Test   string
	Output string
}

// goTest runs the top-level tests tops of package folder pkg, with their subtests, and
// returns the action of each test and subtest that ran: pass, fail or skip. line is the first
// error line of the build or of go test, so that a test that did not run has a reason; "" when
// there is none. A go test that cannot start, or whose output cannot be read, gives no
// actions and its error as line.
func goTest(ctx context.Context, root string, environ []string, pkg string, tops []string) (actions map[string]string, line string) {
	cmd := exec.CommandContext(ctx, "go", "test", "-json", "-count=1", "-run", "^("+strings.Join(tops, "|")+")$", "./"+pkg)
	cmd.Dir, cmd.Env = root, environ
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return nil, err.Error()
	}
	actions = map[string]string{}
	var build strings.Builder
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var e testEvent
		if err := dec.Decode(&e); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, "read the go test output: " + err.Error()
		}
		switch {
		case e.Action == "build-output":
			build.WriteString(e.Output)
		case e.Test != "" && (e.Action == "pass" || e.Action == "fail" || e.Action == "skip"):
			actions[e.Test] = e.Action
		}
	}
	for _, l := range strings.Split(build.String()+stderr.String(), "\n") {
		// A line that starts with "#" names the package of the build errors under it.
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			return actions, l
		}
	}
	return actions, ""
}

// regressionLine is the line of regression case c, from the actions got of the tests that ran
// and the first error line why of each package: pass, or fail with each test that did not
// pass. ok reports a pass.
func regressionLine(c Case, got map[GoTest]string, why map[string]string) (line string, ok bool) {
	var bad []string
	for _, t := range c.Tests {
		switch a := result(got, t); {
		case a == "pass":
		case a != "":
			bad = append(bad, fmt.Sprintf("%s %s: %s", t.Package, t.Name, a))
		case why[t.Package] != "":
			bad = append(bad, fmt.Sprintf("%s %s: not run (%s)", t.Package, t.Name, why[t.Package]))
		default:
			bad = append(bad, fmt.Sprintf("%s %s: not run", t.Package, t.Name))
		}
	}
	if len(bad) > 0 {
		return c.ID + ": fail; " + strings.Join(bad, "; "), false
	}
	return c.ID + ": pass", true
}

// result is the action of test t in got. A pass with a skipped subtest is a skip: go test
// passes a test whose subtest skips, but that part of the fixture tested nothing.
func result(got map[GoTest]string, t GoTest) string {
	a := got[t]
	if a != "pass" {
		return a
	}
	for o, oa := range got {
		if oa == "skip" && o.Package == t.Package && strings.HasPrefix(o.Name, t.Name+"/") {
			return "skip"
		}
	}
	return a
}
