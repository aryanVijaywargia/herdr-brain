package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// AC 1 (E20-S1 #265): each bug of the M2 real-use test and of the M2 eval run (2026-10-07) has
// one regression case, named with its issue number. Each test of a case is a test function of
// its package folder, so a renamed test shows here, not only as a failed case.
func TestRegressionCases(t *testing.T) {
	cases, err := loadProbes("../../evals/probes")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bug-247", "bug-249", "bug-250", "bug-251", "bug-252", "bug-253", "bug-254", "bug-255", "bug-256",
		"bug-257", "bug-258", "bug-263", "bug-276", "bug-277", "bug-278", "bug-279", "bug-280", "bug-281", "bug-282",
		"bug-283", "bug-284", "bug-285", "bug-291", "bug-296"}
	var got []string
	for _, c := range cases {
		if c.Stage != regressionStage {
			continue
		}
		got = append(got, c.ID)
		if c.Control || len(c.Probes) > 0 || len(c.Tests) == 0 {
			t.Errorf("case %s: control %v, %d probes, %d tests; want tests only", c.ID, c.Control, len(c.Probes), len(c.Tests))
		}
		for _, gt := range c.Tests {
			top, _, _ := strings.Cut(gt.Name, "/")
			if !hasTestFunc(t, filepath.Join("..", "..", gt.Package), top) {
				t.Errorf("case %s: package folder %s has no func %s", c.ID, gt.Package, top)
			}
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("regression cases %q, want %q", got, want)
	}
}

// hasTestFunc reports whether a _test.go file of folder dir declares the test function name.
func hasTestFunc(t *testing.T, dir, name string) bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "\nfunc "+name+"(t *testing.T)") {
			return true
		}
	}
	return false
}

// AC 2 and 3 (E20-S1): each case gets one line. A case passes only when each of its tests and
// subtests passed: a failed or skipped test fails it, and so does a test that did not run,
// with the build error when there is one. The fixture is a scratch Go module.
func TestRunRegression(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/fix\n\ngo 1.23\n")
	write(t, filepath.Join(root, "a", "a_test.go"), `package a

import "testing"

func TestPass(t *testing.T) {}

func TestFail(t *testing.T) { t.Error("broken") }

func TestSkip(t *testing.T) { t.Skip("not here") }

func TestSub(t *testing.T) {
	t.Run("ok", func(t *testing.T) {})
	t.Run("bad one", func(t *testing.T) { t.Error("broken") })
}
`)
	write(t, filepath.Join(root, "b", "b_test.go"), "package b\n\nimport \"testing\"\n\nfunc TestBuild(t *testing.T) { missing() }\n")
	bug := func(id string, tests ...GoTest) Case { return Case{ID: id, Stage: regressionStage, Tests: tests} }
	a := func(name string) GoTest { return GoTest{Package: "a", Name: name} }
	cases := []Case{
		bug("bug-1", a("TestPass"), a("TestSub/ok")),
		bug("bug-2", a("TestPass"), a("TestFail"), a("TestSub/bad_one")),
		bug("bug-3", a("TestSkip")),
		bug("bug-4", a("TestGone")),
		bug("bug-5", GoTest{Package: "b", Name: "TestBuild"}),
	}
	var out bytes.Buffer
	failed, err := runRegression(context.Background(), root, os.Environ(), cases, &out)
	if err != nil || !failed {
		t.Fatalf("runRegression() = %v, %v, want a failed case and no error\n%s", failed, err, out.String())
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	want := []string{
		"bug-1: pass",
		"bug-2: fail; a TestFail: fail; a TestSub/bad_one: fail",
		"bug-3: fail; a TestSkip: skip",
		"bug-4: fail; a TestGone: not run",
		"bug-5: fail; b TestBuild: not run (",
	}
	if len(lines) != len(want) {
		t.Fatalf("runRegression() printed %d lines, want one for each of %d cases:\n%s", len(lines), len(cases), out.String())
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("line %d = %q, want %q", i+1, lines[i], w)
		}
	}
	if !strings.Contains(lines[4], "undefined: missing") {
		t.Errorf("line 5 = %q, want the build error", lines[4])
	}
	out.Reset()
	if failed, err := runRegression(context.Background(), root, os.Environ(), cases[:1], &out); err != nil || failed ||
		out.String() != "bug-1: pass\n" {
		t.Errorf("runRegression(bug-1) = %v, %v, %q; want a pass", failed, err, out.String())
	}
}

// goEnv returns the Go cache and module folders of the test process as environment entries,
// so that a go command with a scratch HOME finds them.
func goEnv(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOCACHE", "GOMODCACHE", "GOPATH").Output()
	if err != nil {
		t.Fatal(err)
	}
	vals := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(vals) != 3 {
		t.Fatalf("go env = %q, want 3 lines", out)
	}
	return []string{"GOCACHE=" + vals[0], "GOMODCACHE=" + vals[1], "GOPATH=" + vals[2]}
}
