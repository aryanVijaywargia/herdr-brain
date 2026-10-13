package main

import (
	"bufio"
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/aryanVijaywargia/herdr-brain/adapters"
	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
)

// fakeProfiles is the embedded profiles and an extra fake profile, which has no Go code
// (E16-S10). The fake profile lives in test data only, never in the embedded set.
func fakeProfiles(t *testing.T) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{"fake.toml": {Data: mustRead(t, "../../internal/adapter/testdata/fake/fake.toml")}}
	for _, k := range adapter.Kinds(nil) {
		data, err := fs.ReadFile(adapters.Profiles, k+".toml")
		if err != nil {
			t.Fatal(err)
		}
		fsys[k+".toml"] = &fstest.MapFile{Data: data}
	}
	return fsys
}

// AC 1: setup finds the CLI of a fake extra profile, plans it, and installs its hook entry only
// after the answer yes. The adapter state is then configured.
func TestSetupFakeProfile(t *testing.T) {
	for _, yes := range []bool{true, false} {
		t.Run(map[bool]string{true: "yes", false: "no"}[yes], func(t *testing.T) {
			home := setupHome(t, "0.9.1")
			for _, kind := range []string{"claude", "codex"} {
				if err := os.Remove(filepath.Join(home, "path", kind)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(home, "path", "fake"), []byte("#!/bin/sh\necho 'fake 0.0.1'\n"), 0o700); err != nil { //nolint:gosec // a test CLI must be executable
				t.Fatal(err)
			}
			answers := "\ny\ny\n" // no extractor, then yes to all
			if !yes {
				answers = "\nn\nn\n"
			}
			fsys := fakeProfiles(t)
			d := osSetupDeps(os.Environ())
			d.profiles = fsys
			var out bytes.Buffer
			if err := setup(context.Background(), d, nil, bufio.NewReader(strings.NewReader(answers)), &out); err != nil {
				t.Fatalf("setup() = %v\n%s", err, out.String())
			}
			target := filepath.Join(home, ".fake", "hooks.json")
			for _, want := range []string{"claude is not installed", "codex is not installed",
				"Add this entry to the BeforeTurn array of " + target, "Apply all of this? [y/N] "} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("setup output lacks %q:\n%s", want, out.String())
				}
			}
			if strings.Contains(out.String(), "Press t to trust it") {
				t.Errorf("setup shows the Codex trust steps for a profile with no hook_trust:\n%s", out.String())
			}
			_, err := os.Stat(target)
			if !yes {
				if !os.IsNotExist(err) {
					t.Errorf("setup made %s after the answer no: %v", target, err)
				}
				return
			}
			if b := mustRead(t, target); !bytes.Contains(b, []byte(" hook --profile fake --event start")) {
				t.Errorf("%s has no Brain hook entry:\n%s", target, b)
			}
			p, err := adapter.LoadFS(fsys, "fake")
			if err != nil {
				t.Fatal(err)
			}
			data := filepath.Join(home, ".local", "share", "brain")
			sts, err := harness.Check(context.Background(), noPanes{}, harness.Files{Install: filepath.Join(data, "install.json"),
				Conformance: filepath.Join(data, "conformance.json"), Retest: filepath.Join(data, "retest")}, []*adapter.Profile{p})
			if err != nil || sts[0].State() != harness.Configured {
				t.Errorf("harness.Check(fake) = %+v, %v; want configured", sts, err)
			}
			rec, err := install.Read(filepath.Join(data, "install.json"))
			if err != nil || rec.Entries["fake"].Target != target {
				t.Errorf("install record = %+v, %v; want the fake entry", rec, err)
			}
		})
	}
}

// noPanes is a pane list with no pane.
type noPanes struct{}

func (noPanes) Panes(context.Context) ([]herdr.Pane, error) { return nil, nil }
