package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/BurntSushi/toml"

	"github.com/aryanVijaywargia/herdr-brain/adapters"
	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// fakeProfiles is the embedded profiles and an extra fake profile, which has no Go code
// (E16-S10). The fake profile lives in test data only, never in the embedded set.
func fakeProfiles(t *testing.T) fstest.MapFS {
	t.Helper()
	data, err := os.ReadFile("../../internal/adapter/testdata/fake/fake.toml")
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{"fake.toml": {Data: data}}
	for _, k := range adapter.Kinds(nil) {
		data, err := fs.ReadFile(adapters.Profiles, k+".toml")
		if err != nil {
			t.Fatal(err)
		}
		fsys[k+".toml"] = &fstest.MapFile{Data: data}
	}
	return fsys
}

// fakeEnv is an env whose runner has the fake profile, with its Brain hook installed as
// `brain setup` does it.
func fakeEnv(t *testing.T) (*env, *adapter.Profile) {
	t.Helper()
	e := newEnv(t)
	e.r.Profiles = fakeProfiles(t)
	p, err := adapter.LoadFS(e.r.Profiles, "fake")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.home, ".fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	in := &install.Installer{Home: e.home, Record: e.r.Install, Binary: "/opt/brain/bin/brain"}
	if _, err := in.Install(p); err != nil {
		t.Fatal(err)
	}
	return e, p
}

// AC 2: the runner accepts a fake extra profile. A delivery run of it writes its conformance
// record, and only a pass marks it verified by the E8-S4 check.
func TestFakeProfileConformance(t *testing.T) {
	e, p := fakeEnv(t)
	e.fake.answer = knows
	bin := t.TempDir()
	cliOnPath(t, bin, "fake", "fake 2.1.283") // the runner's Version gives 2.1.283
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	files := harness.Files{Install: e.r.Install, Conformance: e.r.Conformance, Retest: t.TempDir()}
	if s := checkState(t, e, files, p); s == harness.Verified {
		t.Fatalf("state before a run %s, want not verified", s)
	}
	for _, tt := range []struct {
		res  store.Result
		want string
	}{{store.ResultWithheld, resultFail}, {store.ResultEmitted, harness.Pass}} {
		e.hook(t, tt.res, "")
		if _, err := e.r.Run(context.Background(), deliveryCase("fake")); err != nil {
			t.Fatal(err)
		}
		if got := readConformance(t, e)["fake"].Result; got != tt.want {
			t.Errorf("record after a %s receipt %q, want %q", tt.res, got, tt.want)
		}
		if s := checkState(t, e, files, p); (s == harness.Verified) != (tt.want == harness.Pass) {
			t.Errorf("state after a %s run %s, want verified only after a pass", tt.want, s)
		}
	}
	rs := readResults(t, e)
	if len(rs) == 0 || rs[0].Harness != "fake" || rs[0].ProfileSHA256 != p.SHA256 {
		t.Errorf("results %+v, want the fake probe with its profile hash", rs)
	}
}

// AC 2: the reset guard counts the processes of the CLI of each profile, the fake one too.
func TestFakeProfileWriterBlocksReset(t *testing.T) {
	for _, extra := range []bool{true, false} {
		e, _ := fakeEnv(t)
		if !extra {
			e.r.Profiles = nil // the embedded set has no fake profile
		}
		left := e.fake.add("w9:p1", t.TempDir())
		left.fg = []herdr.Process{{Name: "fake", PID: 9}}
		_, err := e.r.Run(context.Background(), []Case{{ID: "c", Setup: []string{"true"}}})
		if refused := err != nil && strings.Contains(err.Error(), "runs fake"); refused != extra {
			t.Errorf("with the fake profile %t: Run() = %v, want a refused reset only with it", extra, err)
		}
	}
}

// A probe file can ask the CLI of any profile, and a probe of a CLI with no profile is an
// error that names the kinds.
func TestProbeCheckKinds(t *testing.T) {
	var pf probeFile
	if _, err := toml.Decode(strings.Replace(goodProbe, `["claude", "codex"]`, `["claude", "fake"]`, 1), &pf); err != nil {
		t.Fatal(err)
	}
	if err := pf.Probe[0].check(adapter.Kinds(fakeProfiles(t))); err != nil {
		t.Errorf("check(with fake) = %v, want nil", err)
	}
	if err := pf.Probe[0].check(adapter.Kinds(nil)); err == nil || !strings.Contains(err.Error(), `harness "fake": want claude or codex`) {
		t.Errorf("check(embedded) = %v, want the fake harness refused", err)
	}
	if got := oneOf(adapter.Kinds(fakeProfiles(t))); got != "claude, codex or fake" {
		t.Errorf("oneOf() = %q, want %q", got, "claude, codex or fake")
	}
}
